package hooks_test

import (
	stdctx "context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xdenny218/stc-agent/internal/hooks"
	stc "github.com/0xdenny218/stc-go"
)

// loadShell 装载 hooks 注册表 + ShellComponent，返回拦截注册表与输出。
func loadShell(t *testing.T, spec map[string]string) (*hooks.Interceptors, *strings.Builder) {
	t.Helper()
	out := &strings.Builder{}
	root := stc.New()
	t.Cleanup(func() { root.Close() })
	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()
	for _, c := range []stc.Component{hooks.Component(), hooks.ShellComponent(spec, out, 5*time.Second)} {
		if err := root.Load(c).Ready(ctx); err != nil {
			t.Fatalf("fiber: %v", err)
		}
	}
	reg, err := stc.Service[*hooks.Interceptors](root, hooks.KeyHooks)
	if err != nil {
		t.Fatal(err)
	}
	return reg, out
}

func TestShellHookPreExecuteIntercepts(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "saw")
	spec := map[string]string{
		"tools/pre-execute": `echo "$STC_HOOK_TOOL" >> ` + marker + `
[ "$STC_HOOK_TOOL" = "shell" ] && exit 1
exit 0`,
	}
	reg, _ := loadShell(t, spec)
	bg := stdctx.Background()

	// shell 被阻断（退出码 1），理由带 stderr。
	err := hooks.Check(bg, reg, hooks.ToolPreExecute, hooks.Payload{Tool: "shell", Arguments: `{}`})
	if err == nil || !strings.Contains(err.Error(), "hook tools/pre-execute blocked") {
		t.Fatalf("expected block, got %v", err)
	}

	// 其他工具放行；环境变量注入过。
	if err := hooks.Check(bg, reg, hooks.ToolPreExecute, hooks.Payload{Tool: "read_file"}); err != nil {
		t.Fatalf("read_file must pass: %v", err)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "shell") || !strings.Contains(string(b), "read_file") {
		t.Fatalf("env injection: %q", string(b))
	}
}

func TestShellHookNotify(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "done")
	spec := map[string]string{
		"agent/turn-end": `echo "$STC_HOOK_TEXT" > ` + marker,
	}
	root := stc.New()
	defer root.Close()
	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()
	for _, c := range []stc.Component{hooks.Component(), hooks.ShellComponent(spec, os.Stdout, 5*time.Second)} {
		if err := root.Load(c).Ready(ctx); err != nil {
			t.Fatalf("fiber: %v", err)
		}
	}
	// 通知经核心 On/Emit 派发（同步串行）。
	root.Emit(hooks.TurnEnd, hooks.Payload{Text: "all done"})
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("notify hook did not run: %v", err)
	}
	if strings.TrimSpace(string(b)) != "all done" {
		t.Fatalf("payload text: %q", string(b))
	}
}

func TestBellRingsOnTurnEnd(t *testing.T) {
	var b strings.Builder
	root := stc.New()
	defer root.Close()
	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()
	if err := root.Load(hooks.BellComponent(&b)).Ready(ctx); err != nil {
		t.Fatalf("fiber: %v", err)
	}
	root.Emit(hooks.TurnEnd, hooks.Payload{})
	if !strings.Contains(b.String(), "\a") {
		t.Fatalf("bell must ring on turn end: %q", b.String())
	}
}
