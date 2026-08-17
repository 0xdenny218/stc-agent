package guest_test

// guest 工具 fiber 的契约测试（spec M3）：
// 装载→注册→调用往返、坏构建旧版本服役、Update 与在途调用互斥（-race）。
// 全部需要 TinyGo（testutil.TinygoPath 找不到即 Skip）。

import (
	stdctx "context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xdenny218/stc-agent/internal/guest"
	"github.com/0xdenny218/stc-agent/internal/testutil"
	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
	"github.com/0xdenny218/stc-go/wasm"
)

func bg() stdctx.Context { return stdctx.Background() }

func ready(t *testing.T, f *stc.Fiber) {
	t.Helper()
	ctx, cancel := stdctx.WithTimeout(bg(), 5*time.Second)
	defer cancel()
	if err := f.Ready(ctx); err != nil {
		t.Fatalf("fiber %s: %v", f.Name(), err)
	}
}

// setup 装配稳定 toolset 与共享 WASM 运行时。
func setup(t *testing.T) (*stc.Context, *tools.Toolset) {
	t.Helper()
	root := stc.New()
	t.Cleanup(func() { root.Close() })
	ready(t, root.Load(tools.ToolsetComponent()))
	ready(t, root.Load(guest.RuntimeComponent()))
	ts, err := stc.Service[*tools.Toolset](root, tools.KeyTools)
	if err != nil {
		t.Fatal(err)
	}
	return root, ts
}

func loadTool(t *testing.T, root *stc.Context, path string, onReload func(string, error)) {
	t.Helper()
	ready(t, root.Load(guest.GuestToolComponent(path, onReload)))
}

type rollResult struct {
	Roll    int    `json:"roll"`
	Sides   int    `json:"sides"`
	Version string `json:"version"`
}

func invokeDice(t *testing.T, ts *tools.Toolset) rollResult {
	t.Helper()
	tool, ok := ts.Lookup("dice")
	if !ok {
		t.Fatal("dice not registered")
	}
	out, err := tool.Invoke(bg(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	var r rollResult
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("bad result %q: %v", out, err)
	}
	return r
}

// guest 工具端到端：装载 dice.wasm → 描述注册进 toolset → 调用往返。
func TestGuestToolInvoke(t *testing.T) {
	path := testutil.BuildGuest(t, "examples/guests/dice", filepath.Join(t.TempDir(), "dice.wasm"))
	root, ts := setup(t)
	loadTool(t, root, path, nil)

	tool, ok := ts.Lookup("dice")
	if !ok {
		t.Fatal("dice not registered")
	}
	if tool.Description != "roll a six-sided die" {
		t.Fatalf("description from descriptor: %q", tool.Description)
	}
	r := invokeDice(t, ts)
	if r.Version != "v1" || r.Sides != 6 || r.Roll < 1 || r.Roll > 6 {
		t.Fatalf("roll: %+v", r)
	}
}

// Contract/BadBuildKeepsServing：重构建产出坏字节 → OnReload 报错，
// 旧版本继续服役。
func TestBadBuildKeepsServing(t *testing.T) {
	dir := t.TempDir()
	path := testutil.BuildGuest(t, "examples/guests/dice", filepath.Join(dir, "dice.wasm"))
	root, ts := setup(t)

	reloaded := make(chan error, 8)
	loadTool(t, root, path, func(_ string, err error) { reloaded <- err })

	if r := invokeDice(t, ts); r.Version != "v1" {
		t.Fatalf("v1: %+v", r)
	}

	if err := os.WriteFile(path, []byte("not a wasm module"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-reloaded:
		if err == nil {
			t.Fatal("bad build must report an error via OnReload")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("OnReload never fired after overwriting with garbage")
	}

	if r := invokeDice(t, ts); r.Version != "v1" {
		t.Fatalf("old version should keep serving: %+v", r)
	}
}

// Contract/UpdateWaitsInflight（-race）：进行中的 guest 调用在旧版本上
// 完整跑完，热替换等待；替换落定后新调用走新版本。
func TestUpdateWaitsInflight(t *testing.T) {
	dir := t.TempDir()
	path := testutil.BuildGuest(t, "internal/guest/testdata/spin", filepath.Join(dir, "spin.wasm"))
	root, ts := setup(t)
	rt, err := stc.Service[*wasm.Runtime](root, guest.KeyRuntime)
	if err != nil {
		t.Fatal(err)
	}

	reloaded := make(chan error, 8)
	loadTool(t, root, path, func(_ string, err error) { reloaded <- err })

	tool, ok := ts.Lookup("spin")
	if !ok {
		t.Fatal("spin not registered")
	}

	type callResult struct {
		out string
		err error
	}
	callDone := make(chan callResult, 1)
	go func() {
		out, err := tool.Invoke(bg(), json.RawMessage(`{}`))
		callDone <- callResult{out, err}
	}()
	// 等 guest 确已在飞（宿主日志出现 "spinning"）。
	deadline := time.Now().Add(5 * time.Second)
	for {
		seen := false
		for _, l := range rt.Logs() {
			if l == "spinning" {
				seen = true
				break
			}
		}
		if seen {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("guest never started spinning; logs=%v", rt.Logs())
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 换 v2 字节：防抖后 Update 阻塞在在途调用上（断言落定前无回调；
	// 窗口 = 防抖 200ms + 充分余量）。
	testutil.BuildGuest(t, "internal/guest/testdata/spin", path, "v2")
	select {
	case err := <-reloaded:
		t.Fatalf("reload completed while a call was in flight: %v", err)
	case <-time.After(800 * time.Millisecond):
	}

	// 放行在途调用：guest 的自旋条件是根上出现字符串服务 "release"。
	if _, err := root.Provide(rt.Key("release"), "1"); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-callDone:
		if res.err != nil || !strings.Contains(res.out, `"version":"v1"`) {
			t.Fatalf("in-flight call: %q, %v (torn?)", res.out, res.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight call never returned after release")
	}
	select {
	case err := <-reloaded:
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reload never completed after the in-flight call finished")
	}

	out, err := tool.Invoke(bg(), json.RawMessage(`{}`))
	if err != nil || !strings.Contains(out, `"version":"v2"`) {
		t.Fatalf("post-reload call: %q, %v", out, err)
	}
}
