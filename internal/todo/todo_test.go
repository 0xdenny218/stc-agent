package todo

import (
	stdctx "context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/0xdenny218/stc-agent/internal/prompt"
	"github.com/0xdenny218/stc-agent/internal/session"
	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
)

// load 装配 prompt/session/toolset/todo 四 fiber 并等就绪，返回句柄与
// todo fiber（重装载断言用）。
func load(t *testing.T, root *stc.Context) (*prompt.Segments, *session.Session, *tools.Toolset, *stc.Fiber) {
	t.Helper()
	comps := []stc.Component{
		prompt.Component(),
		session.Component(""),
		tools.ToolsetComponent(),
		Component(),
	}
	boot, cancel := stdctx.WithTimeout(stdctx.Background(), 10*time.Second)
	defer cancel()
	var todoFiber *stc.Fiber
	for _, c := range comps {
		f := root.Load(c)
		if c.Name == "todo" {
			todoFiber = f
		}
		if err := f.Ready(boot); err != nil {
			t.Fatalf("load %s: %v", c.Name, err)
		}
	}
	reg, err := stc.Service[*prompt.Segments](root, prompt.KeyPrompt)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := stc.Service[*session.Session](root, session.KeySession)
	if err != nil {
		t.Fatal(err)
	}
	ts, err := stc.Service[*tools.Toolset](root, tools.KeyTools)
	if err != nil {
		t.Fatal(err)
	}
	return reg, sess, ts, todoFiber
}

func callTool(t *testing.T, ts *tools.Toolset, args string) (string, error) {
	t.Helper()
	tool, ok := ts.Lookup("todo_write")
	if !ok {
		t.Fatal("todo_write not registered")
	}
	return tool.Invoke(stdctx.Background(), json.RawMessage(args))
}

func lastTodoEvent(t *testing.T, sess *session.Session) []session.Todo {
	t.Helper()
	for i := len(sess.Events()) - 1; i >= 0; i-- {
		if ev := sess.Events()[i]; ev.Type == session.EventTodo {
			return ev.Todos
		}
	}
	t.Fatal("no todo event in log")
	return nil
}

// Contract/TodoJobs 之 todo 面（spec M9）：todo_write 全量快照入 session
// 事件、段落随最新快照同步（空列表 = 摘除），校验失败不落日志。
func TestTodoWriteSnapshotAndSegment(t *testing.T) {
	root := stc.New()
	t.Cleanup(func() { root.Close() })
	reg, sess, ts, _ := load(t, root)

	// 空任务列表：段落不该在册。
	if got := prompt.Assemble(reg); strings.Contains(got, "Task list") {
		t.Fatalf("empty snapshot must not produce a segment: %q", got)
	}

	out, err := callTool(t, ts, `{"todos":[
		{"content":"write spec","status":"completed"},
		{"content":"implement loop","status":"in_progress"},
		{"content":"ship it","status":"pending"}]}`)
	if err != nil {
		t.Fatalf("todo_write: %v", err)
	}
	if out != "todo list updated: 3 items" {
		t.Fatalf("result: %q", out)
	}

	// 快照入事件日志（全量、保序）。
	ev := lastTodoEvent(t, sess)
	if len(ev) != 3 || ev[0].Content != "write spec" || ev[0].Status != "completed" ||
		ev[1].Status != "in_progress" || ev[2].Status != "pending" {
		t.Fatalf("todo event: %+v", ev)
	}
	if d := sess.Todos(); len(d) != 3 || d[1].Content != "implement loop" {
		t.Fatalf("projection todos: %+v", d)
	}

	// 段落随快照在场，且组装进 system prompt。
	seg, ok := reg.Lookup(segmentName)
	if !ok || !strings.Contains(seg, "- [in_progress] implement loop") {
		t.Fatalf("segment: %q ok=%v", seg, ok)
	}

	// 更新快照：段落被覆盖为最新渲染。
	if _, err := callTool(t, ts, `{"todos":[{"content":"only thing","status":"pending"}]}`); err != nil {
		t.Fatalf("todo_write update: %v", err)
	}
	seg, _ = reg.Lookup(segmentName)
	if strings.Contains(seg, "implement loop") || !strings.Contains(seg, "- [pending] only thing") {
		t.Fatalf("segment must track the latest snapshot: %q", seg)
	}

	// 清空：段落摘除，事件日志保留空快照。
	if out, err := callTool(t, ts, `{"todos":[]}`); err != nil || out != "todo list cleared" {
		t.Fatalf("clear: %q %v", out, err)
	}
	if _, ok := reg.Lookup(segmentName); ok {
		t.Fatal("clear must remove the segment")
	}
	if d := sess.Todos(); len(d) != 0 {
		t.Fatalf("cleared projection: %+v", d)
	}
	if ev := lastTodoEvent(t, sess); len(ev) != 0 {
		t.Fatalf("clear event must carry an empty snapshot: %+v", ev)
	}
}

// 校验失败：不落事件、不动段落。
func TestTodoWriteValidation(t *testing.T) {
	root := stc.New()
	t.Cleanup(func() { root.Close() })
	reg, sess, ts, _ := load(t, root)

	if _, err := callTool(t, ts, `{"todos":[{"content":"bad","status":"done"}]}`); err == nil ||
		!strings.Contains(err.Error(), "invalid status") {
		t.Fatalf("bad status: %v", err)
	}
	if _, err := callTool(t, ts, `{"todos":[{"content":"  ","status":"pending"}]}`); err == nil ||
		!strings.Contains(err.Error(), "content is required") {
		t.Fatalf("blank content: %v", err)
	}
	if _, err := callTool(t, ts, `{`); err == nil || !strings.Contains(err.Error(), "bad arguments") {
		t.Fatalf("bad json: %v", err)
	}
	if n := len(sess.Events()); n != 0 {
		t.Fatalf("invalid calls must not log events: %d", n)
	}
	if _, ok := reg.Lookup(segmentName); ok {
		t.Fatal("invalid calls must not touch the segment")
	}
}

// Contract/TodoJobs 之 replay 面：fiber 重装时从会话投影重建段落——
// 事件日志是唯一事实源，快照跨重装存活。
func TestTodoSegmentFoldsFromProjection(t *testing.T) {
	root := stc.New()
	t.Cleanup(func() { root.Close() })
	reg, sess, ts, todoFiber := load(t, root)

	if _, err := callTool(t, ts, `{"todos":[{"content":"survives reload","status":"in_progress"}]}`); err != nil {
		t.Fatalf("todo_write: %v", err)
	}

	// 卸载 todo fiber：工具与段落一起摘除（卸载异步，轮询等待）。
	todoFiber.Dispose()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, toolGone := ts.Lookup("todo_write")
		_, segGone := reg.Lookup(segmentName)
		if !toolGone && !segGone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("unload must remove both tool and segment (tool=%v seg=%v)", toolGone, segGone)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 重装：段落从投影重建（最新快照，非 fiber 内存状态）。
	f := root.Load(Component())
	boot, cancel := stdctx.WithTimeout(stdctx.Background(), 10*time.Second)
	defer cancel()
	if err := f.Ready(boot); err != nil {
		t.Fatalf("reload: %v", err)
	}
	seg, ok := reg.Lookup(segmentName)
	if !ok || !strings.Contains(seg, "- [in_progress] survives reload") {
		t.Fatalf("segment must fold from the session projection: %q ok=%v", seg, ok)
	}
	if len(sess.Todos()) != 1 {
		t.Fatalf("todos must survive the reload: %+v", sess.Todos())
	}
}
