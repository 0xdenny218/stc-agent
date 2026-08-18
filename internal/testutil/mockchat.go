package testutil

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/0xdenny218/stc-agent/internal/model"
)

// MockChat 是脚本化的 OpenAI 兼容 SSE mock（spec D11 的流式版）：解码每个
// 请求交给脚本决定应答消息，按流式分片发出（content 切半、tool_calls
// 增量、finish_reason、usage 收尾、[DONE]），并记录请求供断言。分片是
// 刻意的——客户端的增量组装必须经得起碎化。
type MockChat struct {
	srv    *httptest.Server
	mu     sync.Mutex
	reqs   []RecordedRequest
	script func(n int, r RecordedRequest) model.Message
}

// RecordedRequest 是 mock 看到的一次请求。
type RecordedRequest struct {
	Model        string
	Messages     []model.Message
	ToolNames    []string
	Stream       bool
	IncludeUsage bool
}

// NewMockChat 启动 mock；n 从 1 起计数（第 n 次请求）。
func NewMockChat(script func(n int, r RecordedRequest) model.Message) *MockChat {
	m := &MockChat{script: script}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	return m
}

func (m *MockChat) URL() string { return m.srv.URL }

func (m *MockChat) Close() { m.srv.Close() }

// Requests 返回迄今收到的请求副本。
func (m *MockChat) Requests() []RecordedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]RecordedRequest(nil), m.reqs...)
}

func (m *MockChat) handle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model         string          `json:"model"`
		Messages      []model.Message `json:"messages"`
		Stream        bool            `json:"stream"`
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rec := RecordedRequest{Model: req.Model, Messages: req.Messages, Stream: req.Stream}
	if req.StreamOptions != nil {
		rec.IncludeUsage = req.StreamOptions.IncludeUsage
	}
	for _, t := range req.Tools {
		rec.ToolNames = append(rec.ToolNames, t.Function.Name)
	}
	m.mu.Lock()
	m.reqs = append(m.reqs, rec)
	n := len(m.reqs)
	m.mu.Unlock()

	WriteSSE(w, m.script(n, rec), n)
}

// WriteSSE 把一条 assistant 消息按流式分片写出：role 先行，content 切两
// 半，每个 tool_call 先报 id/name 再把 arguments 切两半，finish_reason
// 收尾，usage（与请求序号挂钩的非零值）+ [DONE]。
func WriteSSE(w http.ResponseWriter, msg model.Message, n int) {
	w.Header().Set("Content-Type", "text/event-stream")
	flush, _ := w.(http.Flusher)
	emit := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flush != nil {
			flush.Flush()
		}
	}
	chunk := func(delta map[string]any, finishReason string) map[string]any {
		return map[string]any{"choices": []any{
			map[string]any{"delta": delta, "finish_reason": finishReason},
		}}
	}

	emit(chunk(map[string]any{"role": "assistant"}, ""))
	if c := msg.Content; c != "" {
		half := len(c) / 2
		emit(chunk(map[string]any{"content": c[:half]}, ""))
		emit(chunk(map[string]any{"content": c[half:]}, ""))
	}
	for i, tc := range msg.ToolCalls {
		emit(chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": i, "id": tc.ID, "type": "function",
			"function": map[string]any{"name": tc.Function.Name},
		}}}, ""))
		args := tc.Function.Arguments
		half := len(args) / 2
		emit(chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": i, "function": map[string]any{"arguments": args[:half]},
		}}}, ""))
		emit(chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": i, "function": map[string]any{"arguments": args[half:]},
		}}}, ""))
	}
	finishReason := "stop"
	if len(msg.ToolCalls) > 0 {
		finishReason = "tool_calls"
	}
	emit(chunk(map[string]any{}, finishReason))
	emit(map[string]any{"choices": []any{}, "usage": map[string]any{
		"prompt_tokens": 10 + n, "completion_tokens": 5, "total_tokens": 15 + n,
	}})
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flush != nil {
		flush.Flush()
	}
}
