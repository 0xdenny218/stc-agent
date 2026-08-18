package define_test

import (
	stdctx "context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xdenny218/stc-agent/internal/define"
	"github.com/0xdenny218/stc-agent/internal/guest"
	"github.com/0xdenny218/stc-agent/internal/testutil"
	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
)

// pongSource 是一个最小合法 guest 源码：提供 tool.pong，invoke 回显前缀。
const pongSource = `package main

import "github.com/0xdenny218/stc-go/guest"

func init() {
	guest.OnInvoke(func(args string) string {
		return "pong:" + args
	})
}

//export start
func start() {
	_ = guest.Provide("tool.pong", ` + "`" + `{"name":"pong","description":"echo pong","parameters":{"type":"object","properties":{}}}` + "`" + `)
}

func main() {}
`

// setup 装载 toolset + 运行时 + define fiber，返回工具查找器与 define
// fiber 句柄。查找器不 fatal：找不到时返回 ok=false，供"无残项"断言。
func setup(t *testing.T, opts define.Options) (func(string) (tools.Tool, bool), *stc.Fiber) {
	t.Helper()
	root := stc.New()
	t.Cleanup(func() { root.Close() })
	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 15*time.Second)
	defer cancel()
	comps := []stc.Component{tools.ToolsetComponent(), guest.RuntimeComponent(), define.Component(opts)}
	fibs := make([]*stc.Fiber, len(comps))
	for i, c := range comps {
		f := root.Load(c)
		fibs[i] = f
		if err := f.Ready(ctx); err != nil {
			t.Fatalf("fiber %s: %v", f.Name(), err)
		}
	}
	ts, err := stc.Service[*tools.Toolset](root, tools.KeyTools)
	if err != nil {
		t.Fatalf("resolve toolset: %v", err)
	}
	return func(name string) (tools.Tool, bool) {
		t.Helper()
		return ts.Lookup(name)
	}, fibs[2]
}

func args(t *testing.T, kv map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(kv)
	if err != nil {
		t.Fatal(err)
	}
	return json.RawMessage(b)
}

// fakeTinygo 写一个可执行的假 tinygo 脚本。
func fakeTinygo(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tinygo")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefineGuestValidation(t *testing.T) {
	lookup, _ := setup(t, define.Options{ToolsDir: filepath.Join(t.TempDir(), "tools")})
	def, ok := lookup("define_guest")
	if !ok {
		t.Fatal("define_guest not registered")
	}
	bg := stdctx.Background()

	for _, tc := range []struct {
		name string
		kv   map[string]any
		want string
	}{
		{"missing name", map[string]any{"source": "x"}, "name and source are required"},
		{"missing source", map[string]any{"name": "pong"}, "name and source are required"},
		{"traversal", map[string]any{"name": "../evil", "source": "x"}, "single path segment"},
		{"slash", map[string]any{"name": "a/b", "source": "x"}, "single path segment"},
	} {
		if _, err := def.Invoke(bg, args(t, tc.kv)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: expected error containing %q, got %v", tc.name, tc.want, err)
		}
	}
}

func TestDefineGuestCompileFailureRollsBack(t *testing.T) {
	td := filepath.Join(t.TempDir(), "tools")
	tg := fakeTinygo(t, `echo "fake compile error" >&2; exit 1`)
	lookup, _ := setup(t, define.Options{ToolsDir: td, TinyGo: tg})
	def, ok := lookup("define_guest")
	if !ok {
		t.Fatal("define_guest not registered")
	}

	_, err := def.Invoke(stdctx.Background(), args(t, map[string]any{"name": "pong", "source": pongSource}))
	if err == nil || !strings.Contains(err.Error(), "compile pong") || !strings.Contains(err.Error(), "fake compile error") {
		t.Fatalf("expected compile error, got %v", err)
	}
	// toolset 无残项、wasm 已删、源码保留（模型可据此重试）。
	if _, ok := lookup("pong"); ok {
		t.Fatal("failed guest left a tool in the toolset")
	}
	if _, err := os.Stat(filepath.Join(td, "pong.wasm")); !os.IsNotExist(err) {
		t.Fatal("failed guest left a wasm on disk")
	}
	if _, err := os.Stat(filepath.Join(td, "pong.go")); err != nil {
		t.Fatalf("source should be kept: %v", err)
	}
}

func TestDefineGuestLoadFailureRollsBack(t *testing.T) {
	td := filepath.Join(t.TempDir(), "tools")
	// "成功"的编译但产出垃圾 wasm：guest.Load 在 probe 阶段失败。
	tg := fakeTinygo(t, `while [ -n "$1" ]; do if [ "$1" = "-o" ]; then shift; printf 'not wasm' > "$1"; exit 0; fi; shift; done; exit 1`)
	lookup, _ := setup(t, define.Options{ToolsDir: td, TinyGo: tg})
	def, ok := lookup("define_guest")
	if !ok {
		t.Fatal("define_guest not registered")
	}

	_, err := def.Invoke(stdctx.Background(), args(t, map[string]any{"name": "pong", "source": pongSource}))
	if err == nil || !strings.Contains(err.Error(), "load pong") {
		t.Fatalf("expected load error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(td, "pong.wasm")); !os.IsNotExist(err) {
		t.Fatal("failed guest left a wasm on disk")
	}
	if _, ok := lookup("pong"); ok {
		t.Fatal("failed guest left a tool in the toolset")
	}
}

func TestDefineGuestHappyPath(t *testing.T) {
	tg := testutil.TinygoPath(t) // 本地无 tinygo 则跳过；CI 有
	td := filepath.Join(t.TempDir(), "tools")
	lookup, defFiber := setup(t, define.Options{ToolsDir: td, TinyGo: tg})
	def, ok := lookup("define_guest")
	if !ok {
		t.Fatal("define_guest not registered")
	}
	bg := stdctx.Background()

	// 模型写源码 → 宿主编译装载 → 新工具可被调用。
	out, err := def.Invoke(bg, args(t, map[string]any{"name": "pong", "source": pongSource}))
	if err != nil {
		t.Fatalf("define_guest: %v", err)
	}
	if !strings.Contains(out, `guest tool "pong" defined`) {
		t.Fatalf("define_guest output: %q", out)
	}
	pong, ok := lookup("pong")
	if !ok {
		t.Fatal("pong not loaded into toolset")
	}
	got, err := pong.Invoke(bg, json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("pong invoke: %v", err)
	}
	if got != `pong:{"x":1}` {
		t.Fatalf("pong output: %q", got)
	}

	// 重新定义同名：旧逆先回卷，新版本上位。
	v2 := strings.Replace(pongSource, `return "pong:" + args`, `return "pong-v2:" + args`, 1)
	if _, err := def.Invoke(bg, args(t, map[string]any{"name": "pong", "source": v2})); err != nil {
		t.Fatalf("re-define_guest: %v", err)
	}
	pong, ok = lookup("pong") // 新版本：旧 handle 已回卷，须重新取工具
	if !ok {
		t.Fatal("pong missing after re-define")
	}
	got, err = pong.Invoke(bg, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("pong v2 invoke: %v", err)
	}
	if got != `pong-v2:{}` {
		t.Fatalf("pong v2 output: %q", got)
	}

	// fiber 卸载回卷全部 guest 逆：pong 从 toolset 撤出。
	defFiber.Dispose()
	if err := defFiber.Gone(bg); err != nil {
		t.Fatalf("define fiber gone: %v", err)
	}
	if _, ok := lookup("pong"); ok {
		t.Fatal("pong survived define fiber unload")
	}
}
