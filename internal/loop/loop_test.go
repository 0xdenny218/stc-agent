package loop

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/0xdenny218/stc-agent/internal/model"
	"github.com/0xdenny218/stc-agent/internal/session"
	"github.com/0xdenny218/stc-agent/internal/tools"
	"github.com/0xdenny218/stc-go/registry"
)

// stubChat 按队列应答并记录每次请求。
type stubChat struct {
	mu      sync.Mutex
	replies []model.Message
	reqs    []model.ChatRequest
}

func (s *stubChat) Chat(_ stdctx.Context, req model.ChatRequest) (*model.ChatResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, req)
	if len(s.replies) == 0 {
		return nil, errors.New("stub chat: no reply queued")
	}
	m := s.replies[0]
	s.replies = s.replies[1:]
	return &model.ChatResponse{Message: m}, nil
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
	r := &runner{chat: chat, sess: sess, ts: ts, maxTurns: 5}

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
	r := &runner{chat: chat, sess: sess, ts: ts, maxTurns: 3}

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
	r := &runner{chat: chat, sess: sess, ts: ts, maxTurns: 5}

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
