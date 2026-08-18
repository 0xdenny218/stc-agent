package skills_test

// skills 的契约测试（spec M8）：SKILL.md 解析、skill fiber 的段落/工具
// 效应与编辑热生效、supervisor 的落盘即装/删除即卸。含 wasm 的用例需要
// TinyGo（testutil.TinygoPath 找不到即 Skip）。

import (
	stdctx "context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xdenny218/stc-agent/internal/guest"
	"github.com/0xdenny218/stc-agent/internal/prompt"
	"github.com/0xdenny218/stc-agent/internal/skills"
	"github.com/0xdenny218/stc-agent/internal/testutil"
	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
)

func ready(t *testing.T, f *stc.Fiber) {
	t.Helper()
	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 10*time.Second)
	defer cancel()
	if err := f.Ready(ctx); err != nil {
		t.Fatalf("fiber %s: %v", f.Name(), err)
	}
}

// waitFor 轮询条件直至成立或超时（fsnotify 事件是异步的）。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func writeSkill(t *testing.T, root, name, body string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// setup 装配段落注册表 + 工具表 + wasm 运行时（skill fiber 的全部
// inject 依赖）。
func setup(t *testing.T) (*stc.Context, *prompt.Segments, *tools.Toolset) {
	t.Helper()
	root := stc.New()
	t.Cleanup(func() { root.Close() })
	ready(t, root.Load(prompt.Component()))
	ready(t, root.Load(tools.ToolsetComponent()))
	ready(t, root.Load(guest.RuntimeComponent()))
	segments, err := stc.Service[*prompt.Segments](root, prompt.KeyPrompt)
	if err != nil {
		t.Fatal(err)
	}
	ts, err := stc.Service[*tools.Toolset](root, tools.KeyTools)
	if err != nil {
		t.Fatal(err)
	}
	return root, segments, ts
}

func TestParse(t *testing.T) {
	t.Run("frontmatter", func(t *testing.T) {
		sk, err := skills.Parse([]byte("---\nname: greeter\ndescription: says hi\n---\nBe friendly.\n"), "fallback")
		if err != nil {
			t.Fatal(err)
		}
		if sk.Name != "greeter" || sk.Description != "says hi" || sk.Body != "Be friendly." {
			t.Fatalf("parsed: %+v", sk)
		}
	})
	t.Run("no frontmatter, name defaults", func(t *testing.T) {
		sk, err := skills.Parse([]byte("Just do it.\n"), "dirname")
		if err != nil {
			t.Fatal(err)
		}
		if sk.Name != "dirname" || sk.Description != "" || sk.Body != "Just do it." {
			t.Fatalf("parsed: %+v", sk)
		}
	})
	t.Run("unclosed frontmatter", func(t *testing.T) {
		if _, err := skills.Parse([]byte("---\nname: x\n"), "d"); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("empty body", func(t *testing.T) {
		if _, err := skills.Parse([]byte("---\nname: x\n---\n"), "d"); err == nil {
			t.Fatal("expected error")
		}
	})
}

// skill fiber 的效应：段落注册、编辑热生效、SKILL.md 消失上报、卸载注销。
func TestSkillComponentEffect(t *testing.T) {
	root, segments, _ := setup(t)
	dir := t.TempDir()
	writeSkill(t, dir, "greeter", "---\ndescription: says hi\n---\nBe friendly.")

	var goneFlag atomic.Bool
	f := root.Load(skills.Component(filepath.Join(dir, "greeter"), nil, func(string) { goneFlag.Store(true) }))
	ready(t, f)

	if got, ok := segments.Lookup("skill:greeter"); !ok || got != "Be friendly." {
		t.Fatalf("segment registered: %q, %v", got, ok)
	}

	// 编辑 → 段落热更新（registry 同名覆盖）。
	if err := os.WriteFile(filepath.Join(dir, "greeter", "SKILL.md"), []byte("Be terse."), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "segment hot-update", func() bool {
		got, _ := segments.Lookup("skill:greeter")
		return got == "Be terse."
	})

	// SKILL.md 消失 → onGone 上报（supervisor 会据此撤退 fiber）。
	if err := os.Remove(filepath.Join(dir, "greeter", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "onGone callback", goneFlag.Load)

	// 卸载 fiber → 段落注销。
	f.Dispose()
	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()
	if err := f.Gone(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := segments.Lookup("skill:greeter"); ok {
		t.Fatal("segment must be unregistered on unload")
	}
}

// skill 带 wasm 工具：目录里的 *.wasm 装成工具子集（吃 hmr 红利）。
func TestSkillWithGuestTool(t *testing.T) {
	wasmPath := testutil.BuildGuest(t, "examples/guests/dice", filepath.Join(t.TempDir(), "dice.wasm"))
	root, segments, ts := setup(t)
	dir := t.TempDir()
	skillDir := writeSkill(t, dir, "dicer", "You can roll dice.")
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "dice.wasm"), wasmBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	f := root.Load(skills.Component(skillDir, nil, nil))
	ready(t, f)

	if _, ok := segments.Lookup("skill:dicer"); !ok {
		t.Fatal("segment registered")
	}
	tool, ok := ts.Lookup("dice")
	if !ok {
		t.Fatal("skill tool registered")
	}
	if _, err := tool.Invoke(stdctx.Background(), []byte(`{}`)); err != nil {
		t.Fatalf("invoke skill tool: %v", err)
	}

	f.Dispose()
	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()
	if err := f.Gone(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := ts.Lookup("dice"); ok {
		t.Fatal("skill tool must vanish on unload")
	}
}

// supervisor 热装载：落盘即装（段落出现 + fiber 入注册表）、坏 skill 只
// 上报不拖垮、删除即卸（段落消失 + fiber 出注册表）。
func TestSupervisorHotLoad(t *testing.T) {
	root, segments, _ := setup(t)
	dir := t.TempDir()

	// skill fiber 的可见性经 stc-go 注册表枚举观察（stc-go#4）。
	inTree := func(name string) bool {
		for _, f := range root.Fibers() {
			if f.Name() == "skill:"+name {
				return true
			}
		}
		return false
	}
	errCh := make(chan error, 4)
	sup := root.Load(skills.SupervisorComponent(root, dir, func(name string, err error) {
		errCh <- err
	}, nil))
	ready(t, sup)

	// 落盘一个好 skill → 段落出现、fiber 入册。
	writeSkill(t, dir, "greeter", "Be friendly.")
	waitFor(t, "skill loaded after drop", func() bool {
		_, ok := segments.Lookup("skill:greeter")
		return ok && inTree("greeter")
	})

	// 落盘一个坏 skill（空正文）→ onError 上报，supervisor 与好 skill 无恙。
	writeSkill(t, dir, "broken", "---\nname: broken\n---\n")
	select {
	case err := <-errCh:
		if !strings.Contains(err.Error(), "empty skill body") {
			t.Fatalf("onError: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for bad-skill error")
	}
	if got, _ := segments.Lookup("skill:greeter"); got != "Be friendly." {
		t.Fatalf("good skill unaffected: %q", got)
	}

	// 删除 skill 目录 → 段落消失、fiber 出册。
	if err := os.RemoveAll(filepath.Join(dir, "greeter")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "skill unloaded after delete", func() bool {
		_, ok := segments.Lookup("skill:greeter")
		return !ok && !inTree("greeter")
	})
}
