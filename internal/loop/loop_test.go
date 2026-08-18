package loop

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/0xdenny218/stc-agent/internal/approval"
	"github.com/0xdenny218/stc-agent/internal/hooks"
	"github.com/0xdenny218/stc-agent/internal/interaction"
	"github.com/0xdenny218/stc-agent/internal/model"
	"github.com/0xdenny218/stc-agent/internal/session"
	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
	"github.com/0xdenny218/stc-go/registry"
)

// stubChat 按队列应答并记录每次请求；内容经 onDelta 一次性"流"出，
// 并附非零用量（断言用量事件的来源）。
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
	return &model.ChatResponse{
		Message: m,
		Usage:   model.Usage{Model: "stub", PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
	}, nil
}

func (s *stubChat) Model() string { return "stub" }

// allowGate 是全放行审批门桩。
type allowGate struct{}

func (allowGate) Check(stdctx.Context, model.ToolCall) error { return nil }

// denyGate 是对指定工具返回审批拒绝的桩。
type denyGate struct{ tool, reason string }

func (g denyGate) Check(_ stdctx.Context, tc model.ToolCall) error {
	if tc.Function.Name == g.tool {
		return &approval.DeniedError{Tool: g.tool, Reason: g.reason}
	}
	return nil
}

// abortGate 是在审批处中断轮次的桩（提问处 Ctrl-C）。
type abortGate struct{}

func (abortGate) Check(stdctx.Context, model.ToolCall) error { return interaction.ErrAborted }

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

// newTestRunner 补齐 runner 的 M7 依赖：空拦截表、空段落表、独立的根
// context（hooks.Emit 需要）。测试要装拦截 hook / 段落时直接改
// r.ic / r.segments（注册表可原地增删）。
func newTestRunner(t *testing.T, chat model.ChatService, sess *session.Session,
	ts *tools.Toolset, gate approval.Gate, maxTurns int) *runner {
	t.Helper()
	root := stc.New()
	t.Cleanup(func() { root.Close() })
	return &runner{
		chat: chat, sess: sess, ts: ts, gate: gate,
		ic: registry.New[hooks.Interceptor](), segments: registry.New[string](),
		fctx: root, maxTurns: maxTurns,
	}
}

// Contract/ToolCallLoop：一轮输入驱动 [模型→工具]* 直到最终答复；历史为
// user/assistant(tool_calls)/tool/assistant，工具结果回灌进下一次请求。
func TestRunTurnToolCallLoop(t *testing.T) {
	sess := &session.Session{}
	ts := registry.New[tools.Tool]()
	ts.Register("echo", echoTool())
	chat := &stubChat{replies: []model.Message{
		{Role: "assistant", ToolCalls: []model.ToolCall{fnCall("c1", "echo", `{"x":1}`)}},
		{Role: "assistant", Content: "done"},
	}}
	r := newTestRunner(t, chat, sess, ts, allowGate{}, 5)

	var out strings.Builder
	if err := r.RunTurn(stdctx.Background(), "hi", &out); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if !strings.Contains(out.String(), `→ echo({"x":1})`) {
		t.Fatalf("tool trace missing: %q", out.String())
	}
	if !strings.Contains(out.String(), "done") {
		t.Fatalf("final answer missing: %q", out.String())
	}

	hist := sess.History()
	if len(hist) != 4 {
		t.Fatalf("history length: %d: %+v", len(hist), hist)
	}
	if hist[0].Role != "user" || hist[1].Role != "assistant" || hist[2].Role != "tool" || hist[3].Role != "assistant" {
		t.Fatalf("history roles: %+v", hist)
	}
	if hist[2].ToolCallID != "c1" || hist[2].Content != `{"x":1}` {
		t.Fatalf("tool message: %+v", hist[2])
	}

	chat.mu.Lock()
	defer chat.mu.Unlock()
	if len(chat.reqs) != 2 {
		t.Fatalf("chat calls: %d", len(chat.reqs))
	}
	if len(chat.reqs[0].Tools) != 1 || chat.reqs[0].Tools[0].Name != "echo" {
		t.Fatalf("request tools: %+v", chat.reqs[0].Tools)
	}
	msgs2 := chat.reqs[1].Messages
	if len(msgs2) != 3 || msgs2[2].Role != "tool" || msgs2[2].Content != `{"x":1}` {
		t.Fatalf("second request must carry the tool round-trip: %+v", msgs2)
	}

	// 每次模型请求落一条用量事件（spec D13）。
	var usages []model.Usage
	for _, ev := range sess.Events() {
		if ev.Type == session.EventUsage {
			usages = append(usages, *ev.Usage)
		}
	}
	if len(usages) != 2 || usages[0].Model != "stub" || usages[0].TotalTokens != 3 {
		t.Fatalf("usage events: %+v", usages)
	}
}

// Contract/ToolPipelineApproval（spec D15 验收的一半）：审批拒绝归一化为
// 工具结果回灌模型（被拒绝的工具不执行，同批其余工具照常执行）。
func TestRunTurnApprovalDeniedFeedsBack(t *testing.T) {
	sess := &session.Session{}
	ts := registry.New[tools.Tool]()
	ts.Register("rm", tools.Tool{
		Name: "rm", Description: "never runs in this test",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Invoke: func(stdctx.Context, json.RawMessage) (string, error) {
			return "EXECUTED", nil
		},
	})
	ts.Register("echo", echoTool())
	chat := &stubChat{replies: []model.Message{
		{Role: "assistant", ToolCalls: []model.ToolCall{
			fnCall("c1", "rm", `{"path":"/x"}`),
			fnCall("c2", "echo", `{"ok":true}`),
		}},
		{Role: "assistant", Content: "done"},
	}}
	r := newTestRunner(t, chat, sess, ts, denyGate{tool: "rm", reason: "denied by user"}, 5)

	var out strings.Builder
	if err := r.RunTurn(stdctx.Background(), "hi", &out); err != nil {
		t.Fatalf("denial is not a turn error: %v", err)
	}

	hist := sess.History()
	if len(hist) != 5 { // user + assistant + 2×tool + assistant
		t.Fatalf("history length: %d: %+v", len(hist), hist)
	}
	if got := hist[2].Content; !strings.Contains(got, "denied by user") {
		t.Fatalf("denial should feed back as tool result: %q", got)
	}
	if strings.Contains(hist[2].Content, "EXECUTED") {
		t.Fatalf("denied tool must not execute: %q", hist[2].Content)
	}
	if got := hist[3].Content; got != `{"ok":true}` {
		t.Fatalf("sibling tool unaffected: %q", got)
	}

	chat.mu.Lock()
	defer chat.mu.Unlock()
	if len(chat.reqs) != 2 || chat.reqs[1].Messages[2].Content != hist[2].Content {
		t.Fatalf("denial must reach the next model request: %+v", chat.reqs)
	}
}

// Contract/ToolPipelineApproval 的另一半：审批提问处 Ctrl-C 是轮次级
// 中断——RunTurn 原样上抛，当前与后续 tool_call 补中断标记（spec D15/M5
// 取消安全的合并语义）。
func TestRunTurnApprovalAbortInterrupts(t *testing.T) {
	sess := &session.Session{}
	ts := registry.New[tools.Tool]()
	ts.Register("echo", echoTool())
	chat := &stubChat{replies: []model.Message{
		{Role: "assistant", ToolCalls: []model.ToolCall{
			fnCall("c1", "echo", `{}`),
			fnCall("c2", "echo", `{}`),
		}},
	}}
	r := newTestRunner(t, chat, sess, ts, abortGate{}, 5)

	var out strings.Builder
	err := r.RunTurn(stdctx.Background(), "hi", &out)
	if !errors.Is(err, interaction.ErrAborted) {
		t.Fatalf("want ErrAborted, got %v", err)
	}

	hist := sess.History()
	if len(hist) != 4 { // user + assistant + 2×tool(中断标记)
		t.Fatalf("history length: %d: %+v", len(hist), hist)
	}
	for _, m := range hist[2:] {
		if m.Role != "tool" || !strings.Contains(m.Content, "turn interrupted") {
			t.Fatalf("unanswered tool_calls must be filled: %+v", m)
		}
	}

	chat.mu.Lock()
	defer chat.mu.Unlock()
	if len(chat.reqs) != 1 {
		t.Fatalf("turn interrupted before the follow-up request: %d", len(chat.reqs))
	}
}

// Contract/MaxTurns：连续工具调用不收敛时熔断（spec M2 验收）。
func TestRunTurnMaxTurns(t *testing.T) {
	sess := &session.Session{}
	ts := registry.New[tools.Tool]()
	ts.Register("echo", echoTool())
	chat := &stubChat{replies: []model.Message{
		{Role: "assistant", ToolCalls: []model.ToolCall{fnCall("c1", "echo", `{}`)}},
		{Role: "assistant", ToolCalls: []model.ToolCall{fnCall("c2", "echo", `{}`)}},
		{Role: "assistant", ToolCalls: []model.ToolCall{fnCall("c3", "echo", `{}`)}},
	}}
	r := newTestRunner(t, chat, sess, ts, allowGate{}, 3)

	var out strings.Builder
	err := r.RunTurn(stdctx.Background(), "spin", &out)
	var mte *MaxTurnsError
	if !errors.As(err, &mte) || mte.Max != 3 {
		t.Fatalf("want MaxTurnsError(3), got %v", err)
	}
	// user + 3×(assistant + tool)
	if got := len(sess.History()); got != 7 {
		t.Fatalf("history length: %d", got)
	}
}

// 取消安全：工具循环中途取消时，未应答的 tool_call 补 aborted 结果，
// 历史保持线格式合法（每个 tool_call 恰有一条 tool 应答）。
func TestRunTurnAbortFillsToolResults(t *testing.T) {
	sess := &session.Session{}
	ts := registry.New[tools.Tool]()
	ctx, cancel := stdctx.WithCancel(stdctx.Background())
	defer cancel()
	ts.Register("block", tools.Tool{
		Name: "block", Description: "cancels the turn mid-loop",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Invoke: func(stdctx.Context, json.RawMessage) (string, error) {
			cancel()
			return "interrupted", nil
		},
	})
	ts.Register("echo", echoTool())
	chat := &stubChat{replies: []model.Message{
		{Role: "assistant", ToolCalls: []model.ToolCall{fnCall("c1", "block", `{}`), fnCall("c2", "echo", `{}`)}},
	}}
	r := newTestRunner(t, chat, sess, ts, allowGate{}, 5)

	var out strings.Builder
	err := r.RunTurn(ctx, "hi", &out)
	if !errors.Is(err, stdctx.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}

	hist := sess.History()
	if len(hist) != 4 {
		t.Fatalf("history length: %d: %+v", len(hist), hist)
	}
	answered := map[string]string{}
	for _, m := range hist {
		if m.Role == "tool" {
			answered[m.ToolCallID] = m.Content
		}
	}
	for _, tc := range hist[1].ToolCalls {
		if _, ok := answered[tc.ID]; !ok {
			t.Fatalf("tool_call %s unanswered — history is not wire-valid", tc.ID)
		}
	}
	if answered["c1"] != "interrupted" {
		t.Fatalf("answered tool_call: %q", answered["c1"])
	}
	if !strings.Contains(answered["c2"], "turn aborted") {
		t.Fatalf("aborted fill: %q", answered["c2"])
	}
}

// Contract/HookBail（spec M7 验收）：tools/pre-execute 拦截 hook 以 bail
// 语义阻断执行——原因作为工具结果回灌模型，被拦工具不执行，同批其余
// 工具照常执行；审批门在 hook 之后（被拦的调用不再打扰用户）。
func TestHookBail(t *testing.T) {
	sess := &session.Session{}
	ts := registry.New[tools.Tool]()
	ts.Register("rm", tools.Tool{
		Name: "rm", Description: "never runs in this test",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Invoke: func(stdctx.Context, json.RawMessage) (string, error) {
			return "EXECUTED", nil
		},
	})
	ts.Register("echo", echoTool())
	chat := &stubChat{replies: []model.Message{
		{Role: "assistant", ToolCalls: []model.ToolCall{
			fnCall("c1", "rm", `{"path":"/x"}`),
			fnCall("c2", "echo", `{"ok":true}`),
		}},
		{Role: "assistant", Content: "done"},
	}}
	r := newTestRunner(t, chat, sess, ts, allowGate{}, 5)
	r.ic.Register("guard", hooks.Interceptor{
		Event: hooks.ToolPreExecute,
		Check: func(_ stdctx.Context, p hooks.Payload) error {
			if p.Tool == "rm" {
				return errors.New("blocked: destructive command")
			}
			return nil
		},
	})

	var out strings.Builder
	if err := r.RunTurn(stdctx.Background(), "hi", &out); err != nil {
		t.Fatalf("bail is not a turn error: %v", err)
	}

	hist := sess.History()
	if len(hist) != 5 { // user + assistant + 2×tool + assistant
		t.Fatalf("history length: %d: %+v", len(hist), hist)
	}
	if got := hist[2].Content; !strings.Contains(got, "blocked: destructive command") {
		t.Fatalf("bail reason should feed back as tool result: %q", got)
	}
	if strings.Contains(hist[2].Content, "EXECUTED") {
		t.Fatalf("bailed tool must not execute: %q", hist[2].Content)
	}
	if got := hist[3].Content; got != `{"ok":true}` {
		t.Fatalf("sibling tool unaffected: %q", got)
	}

	chat.mu.Lock()
	defer chat.mu.Unlock()
	if len(chat.reqs) != 2 || chat.reqs[1].Messages[2].Content != hist[2].Content {
		t.Fatalf("bail reason must reach the next model request: %+v", chat.reqs)
	}
}

// 通知型事件：轮次边界（turn-start/end）与 tools/post-execute 都经
// On/Emit 派发到监听者；负载逐字段对应。
func TestHookNotify(t *testing.T) {
	sess := &session.Session{}
	ts := registry.New[tools.Tool]()
	ts.Register("echo", echoTool())
	chat := &stubChat{replies: []model.Message{
		{Role: "assistant", ToolCalls: []model.ToolCall{fnCall("c1", "echo", `{"x":1}`)}},
		{Role: "assistant", Content: "done"},
	}}
	r := newTestRunner(t, chat, sess, ts, allowGate{}, 5)

	type got struct {
		event string
		p     hooks.Payload
	}
	var mu sync.Mutex
	var events []got
	listen := func(event string) {
		if err := hooks.Listen(r.fctx, event, func(p hooks.Payload) {
			mu.Lock()
			events = append(events, got{event, p})
			mu.Unlock()
		}); err != nil {
			t.Fatalf("listen %s: %v", event, err)
		}
	}
	listen(hooks.TurnStart)
	listen(hooks.TurnEnd)
	listen(hooks.ToolPostExecute)

	var out strings.Builder
	if err := r.RunTurn(stdctx.Background(), "hi", &out); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 3 {
		t.Fatalf("events: %+v", events)
	}
	if events[0].event != hooks.TurnStart || events[0].p.Text != "hi" {
		t.Fatalf("turn-start: %+v", events[0])
	}
	if events[1].event != hooks.ToolPostExecute ||
		events[1].p.Tool != "echo" || events[1].p.Result != `{"x":1}` {
		t.Fatalf("post-execute: %+v", events[1])
	}
	if events[2].event != hooks.TurnEnd || events[2].p.Text != "done" {
		t.Fatalf("turn-end: %+v", events[2])
	}
}

// 段落组装进请求：system prompt 每次请求前现装，随段落注册表变化。
func TestRunTurnSystemPrompt(t *testing.T) {
	sess := &session.Session{}
	ts := registry.New[tools.Tool]()
	chat := &stubChat{replies: []model.Message{
		{Role: "assistant", Content: "one"},
		{Role: "assistant", Content: "two"},
	}}
	r := newTestRunner(t, chat, sess, ts, allowGate{}, 5)
	r.segments.Register("10-identity", "you are stc-agent")

	var out strings.Builder
	if err := r.RunTurn(stdctx.Background(), "hi", &out); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	r.segments.Register("50-extra", "be terse") // 轮次之间落一段
	if err := r.RunTurn(stdctx.Background(), "again", &out); err != nil {
		t.Fatalf("turn 2: %v", err)
	}

	chat.mu.Lock()
	defer chat.mu.Unlock()
	if len(chat.reqs) != 2 {
		t.Fatalf("chat calls: %d", len(chat.reqs))
	}
	if got := chat.reqs[0].System; got != "you are stc-agent" {
		t.Fatalf("turn 1 system: %q", got)
	}
	if got, want := chat.reqs[1].System, "you are stc-agent\n\nbe terse"; got != want {
		t.Fatalf("turn 2 system: %q, want %q", got, want)
	}
	// system 不进会话历史。
	for _, m := range sess.History() {
		if m.Role == "system" {
			t.Fatalf("system must stay out of history: %+v", sess.History())
		}
	}
}
