package inspect_test

import (
	stdctx "context"
	"encoding/json"
	"testing"
	"time"

	"github.com/0xdenny218/stc-agent/internal/inspect"
	"github.com/0xdenny218/stc-agent/internal/tools"
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

type report struct {
	Fibers []inspect.FiberInfo `json:"fibers"`
	Tools  []struct {
		Name string `json:"name"`
	} `json:"tools"`
}

func invokeInspect(t *testing.T, ts *tools.Toolset) report {
	t.Helper()
	tool, ok := ts.Lookup("inspect_agent")
	if !ok {
		t.Fatal("inspect_agent not registered")
	}
	out, err := tool.Invoke(stdctx.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	var rep report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("report is not JSON: %v\n%s", err, out)
	}
	return rep
}

func fiberNames(rep report) map[string]string {
	m := map[string]string{}
	for _, f := range rep.Fibers {
		m[f.Name] = f.State
	}
	return m
}

func toolNames(rep report) []string {
	var out []string
	for _, tl := range rep.Tools {
		out = append(out, tl.Name)
	}
	return out
}

// Contract/Inspect（spec M7 验收）：inspect 报告的 fiber 状态/工具目录
// 与实际一致——fiber 目录直接读 stc-go 注册表快照（stc-go#4）：装载即
// 入册（无需登记）、卸载（Gone）即从报告消失。
func TestInspect(t *testing.T) {
	root := stc.New()
	defer root.Close()

	load(t, root, inspect.DirectoryComponent())
	load(t, root, tools.ToolsetComponent())
	load(t, root, inspect.ToolComponent())

	dir, err := stc.Service[*inspect.Directory](root, inspect.KeyDirectory)
	if err != nil {
		t.Fatalf("resolve directory: %v", err)
	}
	ts, err := stc.Service[*tools.Toolset](root, tools.KeyTools)
	if err != nil {
		t.Fatalf("resolve toolset: %v", err)
	}

	// 目录=全树注册表：已装载的三个 fiber（含目录与 inspect 工具自身）
	// 全部可见，无需任何登记。
	if got := fiberNames(report{Fibers: dir.Snapshot()}); len(got) != 3 ||
		got["inspect-dir"] != "active" || got["tool:inspect_agent"] != "active" || got["toolset"] != "active" {
		t.Fatalf("snapshot must list every loaded fiber: %+v", got)
	}

	// 装载 read_file（无登记调用）→ 出现在报告里。
	rf := load(t, root, tools.ReadFileComponent())
	rep := invokeInspect(t, ts)
	names := fiberNames(rep)
	if names["tool:read_file"] != "active" {
		t.Fatalf("read_file fiber should be active: %+v", names)
	}
	found := false
	for _, n := range toolNames(rep) {
		if n == "read_file" {
			found = true
		}
	}
	if !found {
		t.Fatalf("tool catalog must list read_file: %v", toolNames(rep))
	}

	// 工具 fiber 卸载（Gone 出册）→ 报告同步消失。
	goneCtx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()
	rf.Dispose()
	if err := rf.Gone(goneCtx); err != nil {
		t.Fatalf("read_file gone: %v", err)
	}

	rep = invokeInspect(t, ts)
	for _, n := range toolNames(rep) {
		if n == "read_file" {
			t.Fatalf("read_file must vanish after unload: %v", toolNames(rep))
		}
	}
	if _, ok := fiberNames(rep)["tool:read_file"]; ok {
		t.Fatalf("read_file fiber must vanish from directory: %+v", fiberNames(rep))
	}
	// 其余 fiber 不受影响（出册的只是 read_file）。
	if got := fiberNames(rep); len(got) != 3 {
		t.Fatalf("remaining fibers: %+v", got)
	}
	if n := toolNames(rep); len(n) != 1 || n[0] != "inspect_agent" {
		t.Fatalf("remaining tools: %v", n)
	}
}
