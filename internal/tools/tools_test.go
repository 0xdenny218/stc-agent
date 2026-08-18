package tools_test

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/0xdenny218/stc-agent/internal/loop"
	"github.com/0xdenny218/stc-agent/internal/model"
	"github.com/0xdenny218/stc-agent/internal/session"
	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
)

type stubChat struct{}

func (stubChat) Chat(stdctx.Context, model.ChatRequest, func(string)) (*model.ChatResponse, error) {
	return &model.ChatResponse{Message: model.Message{Role: "assistant", Content: "ok"}}, nil
}

func (stubChat) Model() string { return "stub" }

// Contract/ToolEffectExactness：工具注册是可逆效应——fiber 装载/卸载后
// 工具视图精确对应；且 toolset 是稳定注册表，工具增删不重载消费方
// fiber（spec D3 + M2 验收：loop fiber 的周期 Context 指针不变）。
func TestToolEffectExactness(t *testing.T) {
	root := stc.New()
	defer root.Close()

	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()

	// loop 的另两个依赖以稳定桩提供，专注观察工具效应。
	if _, err := root.Provide(model.KeyChat, stubChat{}); err != nil {
		t.Fatalf("provide chat: %v", err)
	}
	if _, err := root.Provide(session.KeySession, &session.Session{}); err != nil {
		t.Fatalf("provide session: %v", err)
	}

	load := func(c stc.Component) *stc.Fiber {
		t.Helper()
		f := root.Load(c)
		if err := f.Ready(ctx); err != nil {
			t.Fatalf("fiber %s: %v", f.Name(), err)
		}
		return f
	}
	load(tools.ToolsetComponent())
	lf := load(loop.Component(3))
	cycle := lf.Context()

	ts, err := stc.Service[*tools.Toolset](root, tools.KeyTools)
	if err != nil {
		t.Fatalf("resolve toolset: %v", err)
	}
	names := func() []string {
		var out []string
		for _, tool := range ts.List() {
			out = append(out, tool.Name)
		}
		return out
	}
	assertNames := func(want []string) {
		t.Helper()
		if got := names(); !reflect.DeepEqual(got, want) {
			t.Fatalf("toolset view: got %v, want %v", got, want)
		}
		if lf.Context() != cycle {
			t.Fatal("loop fiber reloaded by tool churn (toolset must be a stable registry)")
		}
	}

	assertNames(nil)
	rf := load(tools.ReadFileComponent())
	assertNames([]string{"read_file"})
	load(tools.WriteFileComponent())
	assertNames([]string{"read_file", "write_file"})

	rf.Dispose()
	if err := rf.Gone(ctx); err != nil {
		t.Fatalf("read_file gone: %v", err)
	}
	assertNames([]string{"write_file"})

	load(tools.ReadFileComponent())
	assertNames([]string{"read_file", "write_file"})
}

// 工具调用行为：经 fiber 注册进 toolset 后按名 Lookup 调用。
func TestToolInvoke(t *testing.T) {
	root := stc.New()
	defer root.Close()

	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()

	dir := t.TempDir()
	for _, c := range []stc.Component{
		tools.ToolsetComponent(),
		tools.ReadFileComponent(),
		tools.WriteFileComponent(),
		tools.ShellComponent(dir, 100*time.Millisecond),
	} {
		f := root.Load(c)
		if err := f.Ready(ctx); err != nil {
			t.Fatalf("fiber %s: %v", f.Name(), err)
		}
	}
	ts, err := stc.Service[*tools.Toolset](root, tools.KeyTools)
	if err != nil {
		t.Fatalf("resolve toolset: %v", err)
	}

	bg := stdctx.Background()
	lookup := func(name string) tools.Tool {
		t.Helper()
		tool, ok := ts.Lookup(name)
		if !ok {
			t.Fatalf("tool %q not registered", name)
		}
		return tool
	}
	args := func(kv ...string) json.RawMessage {
		t.Helper()
		if len(kv)%2 != 0 {
			t.Fatal("args: odd key/value count")
		}
		var sb strings.Builder
		sb.WriteByte('{')
		for i := 0; i < len(kv); i += 2 {
			if i > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "%q:%q", kv[i], kv[i+1])
		}
		sb.WriteByte('}')
		return json.RawMessage(sb.String())
	}

	t.Run("write then read", func(t *testing.T) {
		p := filepath.Join(dir, "a.txt")
		out, err := lookup("write_file").Invoke(bg, args("path", p, "content", "hi"))
		if err != nil {
			t.Fatalf("write_file: %v", err)
		}
		if want := fmt.Sprintf("wrote 2 bytes to %s", p); out != want {
			t.Fatalf("write_file output: %q, want %q", out, want)
		}
		out, err = lookup("read_file").Invoke(bg, args("path", p))
		if err != nil {
			t.Fatalf("read_file: %v", err)
		}
		if out != "hi" {
			t.Fatalf("read_file output: %q", out)
		}
	})

	t.Run("read missing file", func(t *testing.T) {
		if _, err := lookup("read_file").Invoke(bg, args("path", filepath.Join(dir, "nope"))); err == nil {
			t.Fatal("expected error for missing file")
		}
	})

	t.Run("bad args are tool errors", func(t *testing.T) {
		if _, err := lookup("read_file").Invoke(bg, json.RawMessage(`{nope`)); err == nil ||
			!strings.Contains(err.Error(), "invalid arguments") {
			t.Fatalf("bad JSON args: %v", err)
		}
		if _, err := lookup("read_file").Invoke(bg, json.RawMessage(`{}`)); err == nil ||
			!strings.Contains(err.Error(), "path is required") {
			t.Fatalf("missing path: %v", err)
		}
		if _, err := lookup("shell").Invoke(bg, json.RawMessage(`{}`)); err == nil ||
			!strings.Contains(err.Error(), "command is required") {
			t.Fatalf("missing command: %v", err)
		}
	})

	t.Run("output capped", func(t *testing.T) {
		p := filepath.Join(dir, "big.txt")
		if err := os.WriteFile(p, []byte(strings.Repeat("a", 40<<10)), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := lookup("read_file").Invoke(bg, args("path", p))
		if err != nil {
			t.Fatalf("read_file: %v", err)
		}
		const note = "\n... (output truncated)"
		if len(out) != (32<<10)+len(note) || !strings.HasSuffix(out, note) {
			t.Fatalf("capped output length %d, suffix %q", len(out), out[len(out)-40:])
		}
	})

	t.Run("shell echo", func(t *testing.T) {
		out, err := lookup("shell").Invoke(bg, args("command", "echo hi"))
		if err != nil {
			t.Fatalf("shell: %v", err)
		}
		if out != "hi\n" {
			t.Fatalf("shell output: %q", out)
		}
	})

	t.Run("shell runs in configured dir", func(t *testing.T) {
		out, err := lookup("shell").Invoke(bg, args("command", "pwd"))
		if err != nil {
			t.Fatalf("shell: %v", err)
		}
		// 逻辑路径与符号链接解析后的路径都可接受（macOS 上 /var 是符号链接）。
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(out); got != dir && got != resolved {
			t.Fatalf("shell dir: %q, want %q (or resolved %q)", got, dir, resolved)
		}
	})

	t.Run("shell exit status is a result", func(t *testing.T) {
		out, err := lookup("shell").Invoke(bg, args("command", "echo oops; exit 3"))
		if err != nil {
			t.Fatalf("exit status must not be an invoke error: %v", err)
		}
		if !strings.Contains(out, "oops") || !strings.Contains(out, "(exit status 3)") {
			t.Fatalf("shell output: %q", out)
		}
	})

	t.Run("shell timeout", func(t *testing.T) {
		_, err := lookup("shell").Invoke(bg, args("command", "sleep 2"))
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("shell timeout: %v", err)
		}
	})
}
