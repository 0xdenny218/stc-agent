package hooks_test

import (
	stdctx "context"
	"errors"
	"testing"
	"time"

	"github.com/0xdenny218/stc-agent/internal/hooks"
	stc "github.com/0xdenny218/stc-go"
	"github.com/0xdenny218/stc-go/registry"
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

// 拦截 hook 的注册是可逆效应：fiber 装载即拦截、卸载即放行。
func TestInterceptorComponentEffect(t *testing.T) {
	root := stc.New()
	defer root.Close()

	load(t, root, hooks.Component())
	reg, err := stc.Service[*hooks.Interceptors](root, hooks.KeyHooks)
	if err != nil {
		t.Fatalf("resolve hooks: %v", err)
	}
	p := hooks.Payload{Tool: "rm"}

	f := load(t, root, hooks.InterceptorComponent("guard", hooks.Interceptor{
		Event: hooks.ToolPreExecute,
		Check: func(stdctx.Context, hooks.Payload) error { return errors.New("bail") },
	}))
	if err := hooks.Check(stdctx.Background(), reg, hooks.ToolPreExecute, p); err == nil {
		t.Fatal("hook fiber loaded: pre-execute must bail")
	}

	gctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()
	f.Dispose()
	if err := f.Gone(gctx); err != nil {
		t.Fatalf("hook gone: %v", err)
	}
	if err := hooks.Check(stdctx.Background(), reg, hooks.ToolPreExecute, p); err != nil {
		t.Fatalf("hook fiber disposed: %v", err)
	}
}

// Check 语义：只问本事件的 hook，按注册表名序串行，第一个错误即 bail。
func TestCheckOrder(t *testing.T) {
	reg := registry.New[hooks.Interceptor]()
	var order []string
	reg.Register("b-other-event", hooks.Interceptor{
		Event: hooks.TurnStart, // 不是本事件——跳过
		Check: func(stdctx.Context, hooks.Payload) error {
			order = append(order, "other")
			return errors.New("must not run")
		},
	})
	reg.Register("a-first", hooks.Interceptor{
		Event: hooks.ToolPreExecute,
		Check: func(stdctx.Context, hooks.Payload) error {
			order = append(order, "first")
			return errors.New("bail here")
		},
	})
	reg.Register("c-second", hooks.Interceptor{
		Event: hooks.ToolPreExecute,
		Check: func(stdctx.Context, hooks.Payload) error {
			order = append(order, "second")
			return nil
		},
	})
	err := hooks.Check(stdctx.Background(), reg, hooks.ToolPreExecute, hooks.Payload{})
	if err == nil || err.Error() != "bail here" {
		t.Fatalf("bail: %v", err)
	}
	if len(order) != 1 || order[0] != "first" {
		t.Fatalf("a-first bails before c-second runs; other-event skipped: %v", order)
	}
}

// 通知型监听经核心 On/Emit 派发；context 回卷后监听自动撤销。
func TestListenEmitLifecycle(t *testing.T) {
	root := stc.New()
	defer root.Close()

	var got []string
	child := root.Child()
	if err := hooks.Listen(child, hooks.TurnStart, func(p hooks.Payload) {
		got = append(got, p.Text)
	}); err != nil {
		t.Fatalf("listen: %v", err)
	}
	hooks.Emit(root, hooks.TurnStart, hooks.Payload{Text: "one"})
	if len(got) != 1 || got[0] != "one" {
		t.Fatalf("emit -> listen: %v", got)
	}

	if err := child.Release(); err != nil { // 回卷 → 监听撤销
		t.Fatalf("release: %v", err)
	}
	hooks.Emit(root, hooks.TurnStart, hooks.Payload{Text: "two"})
	if len(got) != 1 {
		t.Fatalf("listener must be gone after unwind: %v", got)
	}
}
