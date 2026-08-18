package jobs

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xdenny218/stc-agent/internal/approval"
	"github.com/0xdenny218/stc-agent/internal/hooks"
	"github.com/0xdenny218/stc-agent/internal/interaction"
	"github.com/0xdenny218/stc-agent/internal/loop"
	"github.com/0xdenny218/stc-agent/internal/model"
	"github.com/0xdenny218/stc-agent/internal/prompt"
	"github.com/0xdenny218/stc-agent/internal/session"
	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
)

// stubChat 与 task 包的桩同构：按队列应答。
type stubChat struct {
	mu      sync.Mutex
	replies []model.Message
	reqs    []model.ChatRequest
}

func (s *stubChat) Chat(_ stdctx.Context, req model.ChatRequest, onDelta func(string)) (*model.ChatResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, req)
	if len(s.replies) == 0 {
		return nil, errors.New("stub chat: no reply queued")
	}
	m := s.replies[0]
	s.replies = s.replies[1:]
	if onDelta != nil && m.Content != "" {
		onDelta(m.Content)
	}
	return &model.ChatResponse{Message: m, Usage: model.Usage{Model: "stub", TotalTokens: 1}}, nil
}

func (s *stubChat) Model() string { return "stub" }

func provide[T any](name string, key stc.Key, v T) stc.Component {
	return stc.Component{
		Name:    name,
		Provide: []stc.Key{key},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			_, err := c.Provide(key, v)
			return nil, err
		},
	}
}

// load 装配 jobs 及其依赖（kind=task 需要父域有 chat/approval/hooks/
// prompt 供子 loop 解析），返回工具表、模型桩与 notices 服务。
func load(t *testing.T, chat model.ChatService) (*tools.Toolset, *stubChat, loop.Notices) {
	t.Helper()
	root := stc.New()
	t.Cleanup(func() { root.Close() })
	comps := []stc.Component{
		provide("chat", model.KeyChat, chat),
		session.Component(""),
		tools.ToolsetComponent(),
		hooks.Component(),
		prompt.Component(),
		approval.Component(approval.Policy{Allow: []string{"*"}}, interaction.Deny()),
		Component(Options{MaxTurns: 5}),
	}
	boot, cancel := stdctx.WithTimeout(stdctx.Background(), 10*time.Second)
	defer cancel()
	for _, c := range comps {
		f := root.Load(c)
		if err := f.Ready(boot); err != nil {
			t.Fatalf("load %s: %v", c.Name, err)
		}
	}
	ts, err := stc.Service[*tools.Toolset](root, tools.KeyTools)
	if err != nil {
		t.Fatal(err)
	}
	notices, err := stc.Service[loop.Notices](root, loop.KeyNotices)
	if err != nil {
		t.Fatal(err)
	}
	return ts, chat.(*stubChat), notices
}

func invoke(t *testing.T, ts *tools.Toolset, name, args string) string {
	t.Helper()
	tool, ok := ts.Lookup(name)
	if !ok {
		t.Fatalf("%s not registered", name)
	}
	out, err := tool.Invoke(stdctx.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}

// waitFor 轮询直到 cond 为真或超时。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// jobsSeen 从 job_list 输出解析每个任务的状态行。
func jobsSeen(t *testing.T, ts *tools.Toolset) string {
	t.Helper()
	return invoke(t, ts, "job_list", `{}`)
}

// Contract/TodoJobs 之 jobs 面（spec M9）：shell 与 task 两种后台任务
// 统一可列可杀，完成通知经 notices 入会话。
func TestJobsShellLifecycle(t *testing.T) {
	ts, _, notices := load(t, &stubChat{})

	// shell：启动即回，完成后通知入队。
	out := invoke(t, ts, "job_start", `{"kind":"shell","command":"echo hello from job"}`)
	if !strings.Contains(out, "[job 1 started]") {
		t.Fatalf("start: %q", out)
	}
	waitFor(t, "job 1 to finish", func() bool {
		return strings.Contains(jobsSeen(t, ts), "1\tshell\tdone")
	})

	ns := notices.Drain()
	if len(ns) != 1 || !strings.Contains(ns[0], "[job 1 done]") || !strings.Contains(ns[0], "hello from job") {
		t.Fatalf("notice: %+v", ns)
	}
	if again := notices.Drain(); len(again) != 0 {
		t.Fatalf("drain must be one-shot: %+v", again)
	}

	// kill：sleep 任务启动后杀掉，终态 killed（非 done/failed）。
	invoke(t, ts, "job_start", `{"kind":"shell","command":"sleep 30"}`)
	waitFor(t, "job 2 running", func() bool {
		return strings.Contains(jobsSeen(t, ts), "2\tshell\trunning")
	})
	if out := invoke(t, ts, "job_kill", `{"id":2}`); !strings.Contains(out, "kill signal") {
		t.Fatalf("kill: %q", out)
	}
	waitFor(t, "job 2 killed", func() bool {
		return strings.Contains(jobsSeen(t, ts), "2\tshell\tkilled")
	})

	// 未知 ID：错误回灌模型语义。
	tool, _ := ts.Lookup("job_kill")
	if _, err := tool.Invoke(stdctx.Background(), json.RawMessage(`{"id":99}`)); err == nil ||
		!strings.Contains(err.Error(), "no job 99") {
		t.Fatalf("unknown id: %v", err)
	}
}

// kind=task：后台子 agent 走 task.Run 同一条通路，终答进通知。
func TestJobsTaskKind(t *testing.T) {
	chat := &stubChat{replies: []model.Message{{Role: "assistant", Content: "background answer"}}}
	ts, _, notices := load(t, chat)

	out := invoke(t, ts, "job_start", `{"kind":"task","prompt":"do background research"}`)
	if !strings.Contains(out, "[job 1 started] task") {
		t.Fatalf("start: %q", out)
	}
	waitFor(t, "job 1 done", func() bool {
		return strings.Contains(jobsSeen(t, ts), "1\ttask\tdone")
	})
	ns := notices.Drain()
	if len(ns) != 1 || !strings.Contains(ns[0], "background answer") {
		t.Fatalf("notice: %+v", ns)
	}

	// 子 agent 确实收到独立请求（父会话零消息之外的那一路）。
	chat.mu.Lock()
	defer chat.mu.Unlock()
	if len(chat.reqs) != 1 || chat.reqs[0].Messages[0].Content != "do background research" {
		t.Fatalf("child request: %+v", chat.reqs)
	}
}

// 参数校验：坏 kind、缺 command/prompt 不产生任务。
func TestJobsBadArgs(t *testing.T) {
	ts, _, _ := load(t, &stubChat{})
	cases := []struct{ name, args, want string }{
		{"bad kind", `{"kind":"cron"}`, "unknown kind"},
		{"shell no command", `{"kind":"shell"}`, "command is required"},
		{"task no prompt", `{"kind":"task"}`, "prompt is required"},
		{"bad json", `{`, "bad arguments"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool, _ := ts.Lookup("job_start")
			if _, err := tool.Invoke(stdctx.Background(), json.RawMessage(tc.args)); err == nil ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
	if got := jobsSeen(t, ts); got != "no jobs" {
		t.Fatalf("bad args must not create jobs: %q", got)
	}
}
