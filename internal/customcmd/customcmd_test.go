package customcmd_test

import (
	stdctx "context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/0xdenny218/stc-agent/internal/cli"
	"github.com/0xdenny218/stc-agent/internal/customcmd"
	"github.com/0xdenny218/stc-agent/internal/loop"
	stc "github.com/0xdenny218/stc-go"
)

// stubRunner 记录收到的 prompt。
type stubRunner struct {
	mu      sync.Mutex
	prompts []string
}

func (r *stubRunner) RunTurn(_ stdctx.Context, input string, _ io.Writer) error {
	r.mu.Lock()
	r.prompts = append(r.prompts, input)
	r.mu.Unlock()
	return nil
}

func (r *stubRunner) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.prompts) == 0 {
		return ""
	}
	return r.prompts[len(r.prompts)-1]
}

// waitCond 轮询直到 cond 为真或超时。
func waitCond(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// runnerProvider 把 stubRunner 提供为 loop.KeyRunner。
func runnerProvider(r loop.Runner) stc.Component {
	return stc.Component{
		Name:    "runner-provider",
		Provide: []stc.Key{loop.KeyRunner},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			_, err := c.Provide(loop.KeyRunner, r)
			return nil, err
		},
	}
}

// setup 装载命令注册表 + runner 提供者 + supervisor。
func setup(t *testing.T) (*cli.Registry, *stubRunner, string) {
	t.Helper()
	dir := t.TempDir()
	root := stc.New()
	t.Cleanup(func() { root.Close() })
	r := &stubRunner{}
	comps := []stc.Component{
		cli.RegistryComponent(),
		runnerProvider(r),
		customcmd.SupervisorComponent(root, dir,
			func(name string, err error) { t.Errorf("[cmd] %s: %v", name, err) },
			func(name string, loaded bool) {}),
	}
	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()
	for _, c := range comps {
		if err := root.Load(c).Ready(ctx); err != nil {
			t.Fatalf("fiber: %v", err)
		}
	}
	reg, err := stc.Service[*cli.Registry](root, cli.KeyCommands)
	if err != nil {
		t.Fatal(err)
	}
	return reg, r, dir
}

// dispatch 发一条斜杠命令。
func dispatch(t *testing.T, reg *cli.Registry, line string) {
	t.Helper()
	handled, err := cli.Dispatch(stdctx.Background(), io.Discard, line, reg)
	if !handled {
		t.Fatalf("command not registered: %s", line)
	}
	if err != nil {
		t.Fatalf("dispatch %s: %v", line, err)
	}
}

func TestCustomCmdHotLoad(t *testing.T) {
	reg, r, dir := setup(t)
	p := filepath.Join(dir, "greet.md")

	// 落盘即装：frontmatter 剥离，$ARGUMENTS 替换。
	if err := os.WriteFile(p, []byte("---\ndescription: say hi\n---\nSay hello to $ARGUMENTS."), 0o644); err != nil {
		t.Fatal(err)
	}
	waitCond(t, func() bool { _, ok := reg.Lookup("greet"); return ok })
	dispatch(t, reg, "/greet world")
	if r.last() != "Say hello to world." {
		t.Fatalf("$ARGUMENTS substitution: %q", r.last())
	}

	// 原地修改即重载：无占位符时参数追加在末尾。
	if err := os.WriteFile(p, []byte("Fixed prompt body."), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		dispatch(t, reg, "/greet extra")
		if r.last() == "Fixed prompt body.\nextra" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if r.last() != "Fixed prompt body.\nextra" {
		t.Fatalf("hot-reload with appended args: %q", r.last())
	}

	// 删除即卸载。
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	waitCond(t, func() bool { _, ok := reg.Lookup("greet"); return !ok })
}

func TestCustomCmdBootFailFast(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.md"), []byte("   "), 0o644); err != nil {
		t.Fatal(err)
	}
	root := stc.New()
	defer root.Close()
	comps := []stc.Component{
		cli.RegistryComponent(),
		runnerProvider(&stubRunner{}),
		customcmd.SupervisorComponent(root, dir, nil, nil),
	}
	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()
	for i, c := range comps[:2] {
		if err := root.Load(c).Ready(ctx); err != nil {
			t.Fatalf("fiber %d: %v", i, err)
		}
	}
	if err := root.Load(comps[2]).Ready(ctx); err == nil {
		t.Fatal("empty command body must fail the boot")
	}
}
