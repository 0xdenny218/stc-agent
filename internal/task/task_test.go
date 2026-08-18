package task

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
	"github.com/0xdenny218/stc-agent/internal/model"
	"github.com/0xdenny218/stc-agent/internal/prompt"
	"github.com/0xdenny218/stc-agent/internal/session"
	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
	"github.com/0xdenny218/stc-go/registry"
)

// stubChat 按队列应答并记录每次请求（与 loop 测试的桩同构，小包各持一份）。
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
	return &model.ChatResponse{Message: m, Usage: model.Usage{Model: "stub", PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}}, nil
}

func (s *stubChat) Model() string { return "stub" }

func fnCall(id, name, args string) model.ToolCall {
	return model.ToolCall{ID: id, Type: "function", Function: model.ToolCallFunction{Name: name, Arguments: args}}
}

func echoTool() tools.Tool {
	return tools.Tool{
		Name: "echo", Description: "echo arguments back",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Invoke: func(_ stdctx.Context, args json.RawMessage) (string, error) {
			return string(args), nil
		},
	}
}

// provide 是给测试装配用的最小提供者组件。
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

// Contract/SubagentTask（spec M9）：task 工具在 Context.Child 子作用域里
// 搭独立子 agent——隔离工具表/会话/runner（子会话纯内存、不进父事件
// 日志），共享模型/审批/hooks；子 agent 用工具子集独立跑轮，终答作为
// 工具结果回流（落入父会话就是一条普通 tool 消息）。
func TestRunSubAgent(t *testing.T) {
	root := stc.New()
	t.Cleanup(func() { root.Close() })

	chat := &stubChat{replies: []model.Message{
		{Role: "assistant", ToolCalls: []model.ToolCall{fnCall("c1", "echo", `{"x":1}`)}},
		{Role: "assistant", Content: "sub done"},
		{Role: "assistant", Content: "done two"},
	}}
	comps := []stc.Component{
		provide("chat", model.KeyChat, model.ChatService(chat)),
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
	ts.Register("echo", echoTool())

	// 子域的 turn 事件经全局监听表抵达父域监听者（共享 hooks）。
	var mu sync.Mutex
	var turnEnds []string
	if err := hooks.Listen(root, hooks.TurnEnd, func(p hooks.Payload) {
		mu.Lock()
		turnEnds = append(turnEnds, p.Text)
		mu.Unlock()
	}); err != nil {
		t.Fatal(err)
	}

	taskTool, ok := ts.Lookup("task")
	if !ok {
		t.Fatal("task tool not registered")
	}

	// 第一次调用：子 agent 用默认子集（全部工具去掉 task）独立跑轮。
	out, err := taskTool.Invoke(stdctx.Background(), json.RawMessage(`{"prompt":"sub question"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if out != "sub done" {
		t.Fatalf("result must be the child's final answer: %q", out)
	}

	chat.mu.Lock()
	if len(chat.reqs) != 2 {
		t.Fatalf("child chat calls: %d", len(chat.reqs))
	}
	msgs0 := chat.reqs[0].Messages
	if len(msgs0) != 1 || msgs0[0].Role != "user" || msgs0[0].Content != "sub question" {
		t.Fatalf("child must start from a fresh session: %+v", msgs0)
	}
	if len(chat.reqs[0].Tools) != 1 || chat.reqs[0].Tools[0].Name != "echo" {
		t.Fatalf("default subset = all tools except task: %+v", chat.reqs[0].Tools)
	}
	msgs1 := chat.reqs[1].Messages
	if len(msgs1) != 3 || msgs1[2].Role != "tool" || msgs1[2].Content != `{"x":1}` {
		t.Fatalf("child tool round-trip: %+v", msgs1)
	}
	chat.mu.Unlock()

	// 父会话零泄漏：子 agent 的消息不进父事件日志。
	sess, err := stc.Service[*session.Session](root, session.KeySession)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(sess.History()); n != 0 {
		t.Fatalf("parent session must stay empty, got %d messages", n)
	}
	if n := len(sess.Events()); n != 0 {
		t.Fatalf("parent event log must stay empty, got %d events", n)
	}

	mu.Lock()
	if len(turnEnds) != 1 || turnEnds[0] != "sub done" {
		t.Fatalf("child turn-end hook must reach parent listeners: %+v", turnEnds)
	}
	mu.Unlock()

	// 第二次调用：又是全新子会话（不累积上一轮的消息）。
	out, err = taskTool.Invoke(stdctx.Background(), json.RawMessage(`{"prompt":"second"}`))
	if err != nil {
		t.Fatalf("invoke 2: %v", err)
	}
	if out != "done two" {
		t.Fatalf("result 2: %q", out)
	}
	chat.mu.Lock()
	defer chat.mu.Unlock()
	if len(chat.reqs) != 3 {
		t.Fatalf("chat calls after invoke 2: %d", len(chat.reqs))
	}
	if msgs := chat.reqs[2].Messages; len(msgs) != 1 || msgs[0].Content != "second" {
		t.Fatalf("each invocation must get a fresh child session: %+v", msgs)
	}
}

// Contract/SubagentTask 的参数面：显式子集、未知工具名、递归与空 prompt
// 都以错误文本回灌（在搭子域之前失败，不产生任何副作用）。
func TestRunSubAgentBadArgs(t *testing.T) {
	root := stc.New()
	t.Cleanup(func() { root.Close() })
	chat := &stubChat{replies: []model.Message{{Role: "assistant", Content: "ok"}}}
	comps := []stc.Component{
		provide("chat", model.KeyChat, model.ChatService(chat)),
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
	ts, _ := stc.Service[*tools.Toolset](root, tools.KeyTools)
	ts.Register("echo", echoTool())
	taskTool, _ := ts.Lookup("task")

	cases := []struct {
		name, args, want string
	}{
		{"empty prompt", `{"prompt":"  "}`, "prompt is required"},
		{"recursion", `{"prompt":"x","tools":["task"]}`, "no recursion"},
		{"unknown tool", `{"prompt":"x","tools":["nope"]}`, `unknown tool "nope"`},
		{"bad json", `{`, "bad arguments"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := taskTool.Invoke(stdctx.Background(), json.RawMessage(tc.args)); err == nil ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
	chat.mu.Lock()
	defer chat.mu.Unlock()
	if len(chat.reqs) != 0 {
		t.Fatalf("bad args must fail before any child chat call: %d", len(chat.reqs))
	}
}

// pickSubset：默认排除 task；显式名单按名取。
func TestPickSubset(t *testing.T) {
	ts := registry.New[tools.Tool]()
	ts.Register("echo", echoTool())
	ts.Register("task", tools.Tool{Name: "task"})
	ts.Register("read_file", tools.Tool{Name: "read_file"})

	def, err := pickSubset(ts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(def) != 2 {
		t.Fatalf("default subset must exclude task: %+v", def)
	}
	for _, tool := range def {
		if tool.Name == "task" {
			t.Fatalf("default subset must exclude task: %+v", def)
		}
	}

	sel, err := pickSubset(ts, []string{"echo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sel) != 1 || sel[0].Name != "echo" {
		t.Fatalf("explicit subset: %+v", sel)
	}
}
