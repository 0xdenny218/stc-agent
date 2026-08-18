package prompt_test

import (
	stdctx "context"
	"testing"
	"time"

	"github.com/0xdenny218/stc-agent/internal/prompt"
	stc "github.com/0xdenny218/stc-go"
)

func load(t *testing.T, root *stc.Context, c stc.Component) *stc.Fiber {
	t.Helper()
	f := root.Load(c)
	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()
	if err := f.Ready(ctx); err != nil {
		t.Fatalf("fiber %s: %v", f.Name(), err)
	}
	return f
}

func gone(t *testing.T, f *stc.Fiber) {
	t.Helper()
	f.Dispose()
	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()
	if err := f.Gone(ctx); err != nil {
		t.Fatalf("fiber %s gone: %v", f.Name(), err)
	}
}

// Contract/PromptAssembly（spec M7 验收）：段落随 fiber 增删反应式变化，
// 顺序按段落名排序、稳定。
func TestPromptAssembly(t *testing.T) {
	root := stc.New()
	defer root.Close()

	load(t, root, prompt.Component())
	reg, err := stc.Service[*prompt.Segments](root, prompt.KeyPrompt)
	if err != nil {
		t.Fatalf("resolve prompt: %v", err)
	}
	if got := prompt.Assemble(reg); got != "" {
		t.Fatalf("empty registry assembles to empty: %q", got)
	}

	// 乱序装载两段：组装按名字序（注册表序），与装载先后无关。
	fExtra := load(t, root, prompt.SegmentComponent("50-extra", "be terse"))
	load(t, root, prompt.SegmentComponent("10-identity", "you are stc-agent"))
	if got, want := prompt.Assemble(reg), "you are stc-agent\n\nbe terse"; got != want {
		t.Fatalf("assembled: %q, want %q", got, want)
	}

	gone(t, fExtra) // 卸载即摘除（注册即可逆效应）
	if got := prompt.Assemble(reg); got != "you are stc-agent" {
		t.Fatalf("after segment unload: %q", got)
	}
}
