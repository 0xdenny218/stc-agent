package model

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// sseLines 把每行作为一个 SSE data: 事件写出（行尾补空行分帧）。
func sseLines(w http.ResponseWriter, lines ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, l := range lines {
		_, _ = io.WriteString(w, "data: "+l+"\n\n")
	}
}

// Contract/ChatClient：请求格式、auth、错误分类（spec M1 里程碑级场景；
// M5 起请求恒为流式）。
func TestChatClientContract(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		var gotMethod, gotPath, gotAuth, gotCT string
		var gotBody wireRequest
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			gotCT = r.Header.Get("Content-Type")
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Errorf("decode request: %v", err)
			}
			sseLines(w,
				`{"choices":[{"delta":{"role":"assistant"}}]}`,
				`{"choices":[{"delta":{"content":"hi "}}]}`,
				`{"choices":[{"delta":{"content":"there"}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
				`{"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}}`,
				`[DONE]`,
			)
		}))
		defer srv.Close()

		var deltas []string
		c := NewClient(srv.URL, "k", "m1", time.Second)
		resp, err := c.Chat(stdctx.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}},
			func(d string) { deltas = append(deltas, d) })
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if resp.Message.Role != "assistant" || resp.Message.Content != "hi there" {
			t.Fatalf("unexpected reply: %+v", resp.Message)
		}
		if !reflect.DeepEqual(deltas, []string{"hi ", "there"}) {
			t.Fatalf("deltas out of order: %q", deltas)
		}
		if resp.FinishReason != "stop" {
			t.Fatalf("finish reason: %q", resp.FinishReason)
		}
		if resp.Usage.TotalTokens != 11 || resp.Usage.PromptTokens != 9 || resp.Usage.Model != "m1" {
			t.Fatalf("usage (Model backfilled by client): %+v", resp.Usage)
		}
		if gotMethod != http.MethodPost || gotPath != "/chat/completions" {
			t.Fatalf("request line: %s %s", gotMethod, gotPath)
		}
		if gotAuth != "Bearer k" {
			t.Fatalf("auth header: %q", gotAuth)
		}
		if gotCT != "application/json" {
			t.Fatalf("content-type: %q", gotCT)
		}
		if gotBody.Model != "m1" || !gotBody.Stream {
			t.Fatalf("body: %+v", gotBody)
		}
		if gotBody.StreamOptions == nil || !gotBody.StreamOptions.IncludeUsage {
			t.Fatalf("stream_options.include_usage missing: %+v", gotBody)
		}
		if len(gotBody.Messages) != 1 || gotBody.Messages[0].Content != "hi" {
			t.Fatalf("messages: %+v", gotBody.Messages)
		}
		if c.Model() != "m1" {
			t.Fatalf("Model(): %q", c.Model())
		}
	})

	t.Run("http error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "boom")
		}))
		defer srv.Close()

		c := NewClient(srv.URL, "k", "m1", time.Second)
		_, err := c.Chat(stdctx.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
		var ce *ChatError
		if !errors.As(err, &ce) {
			t.Fatalf("want *ChatError, got %T: %v", err, err)
		}
		if ce.Kind != KindHTTP || ce.Status != http.StatusInternalServerError || !strings.Contains(ce.Body, "boom") {
			t.Fatalf("want KindHTTP/500/boom, got %+v", ce)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(300 * time.Millisecond)
		}))
		defer srv.Close()

		c := NewClient(srv.URL, "k", "m1", 20*time.Millisecond)
		_, err := c.Chat(stdctx.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
		var ce *ChatError
		if !errors.As(err, &ce) || ce.Kind != KindTimeout {
			t.Fatalf("want KindTimeout, got %v", err)
		}
	})

	t.Run("stream without chunks", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			sseLines(w, `[DONE]`)
		}))
		defer srv.Close()

		c := NewClient(srv.URL, "k", "m1", time.Second)
		_, err := c.Chat(stdctx.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
		var ce *ChatError
		if !errors.As(err, &ce) || ce.Kind != KindProtocol {
			t.Fatalf("want KindProtocol, got %v", err)
		}
	})

	t.Run("bad chunk", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			sseLines(w, `{"choices":[`, `[DONE]`)
		}))
		defer srv.Close()

		c := NewClient(srv.URL, "k", "m1", time.Second)
		_, err := c.Chat(stdctx.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
		var ce *ChatError
		if !errors.As(err, &ce) || ce.Kind != KindProtocol {
			t.Fatalf("want KindProtocol, got %v", err)
		}
	})
}

// Contract/StreamAssembly（spec M5）：tool_calls 增量分片按 index 组装，
// 分片的字段碎片（id/name/arguments）各自拼接完整。
func TestStreamAssembly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseLines(w,
			`{"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"file","arguments":"{\"path\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"shell","arguments":"{}"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"/tmp/x\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`,
			`[DONE]`,
		)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m1", time.Second)
	resp, err := c.Chat(stdctx.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	want := []ToolCall{
		{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "read_file", Arguments: `{"path":"/tmp/x"}`}},
		{ID: "call_2", Type: "function", Function: ToolCallFunction{Name: "shell", Arguments: `{}`}},
	}
	if !reflect.DeepEqual(resp.Message.ToolCalls, want) {
		t.Fatalf("assembled tool_calls:\n got %+v\nwant %+v", resp.Message.ToolCalls, want)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("finish reason: %q", resp.FinishReason)
	}
	if resp.Usage.TotalTokens != 7 {
		t.Fatalf("usage: %+v", resp.Usage)
	}
}

// TestThinkFilter 直测思考段过滤器：标记被分片边界劈开也不泄漏，孤立
// </think> 剥除，字面尾巴收尾放出（GLM 冒烟实录的两种泄漏形态）。
func TestThinkFilter(t *testing.T) {
	run := func(chunks []string) string {
		var f thinkFilter
		var got strings.Builder
		for _, c := range chunks {
			got.WriteString(f.feed(c))
		}
		got.WriteString(f.flush())
		return got.String()
	}
	for _, tc := range []struct {
		name   string
		chunks []string
		want   string
	}{
		{"plain", []string{"hello world"}, "hello world"},
		{"full block in one chunk", []string{"<think>draft</think>answer"}, "answer"},
		{"marker split across chunks", []string{"<th", "ink>dr", "aft</th", "ink>answer"}, "answer"},
		{"orphan close marker", []string{"第一段</th", "ink>第二段"}, "第一段第二段"},
		{"orphan close in one chunk", []string{"a</think>b"}, "ab"},
		{"multiple blocks", []string{"a<think>1</think>b", "<thi", "nk>2</think>c"}, "abc"},
		{"literal tail held then flushed", []string{"x<thi"}, "x<thi"},
		{"unterminated think suppressed", []string{"a<think>reasoning never ends"}, "a"},
		{"reopen after orphan close", []string{"</think>a<think>x</think>b"}, "ab"},
	} {
		if got := run(tc.chunks); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Contract/ThinkStripped：内联思考段在客户端层就被净化——入库消息与
// onDelta 流都只含可见文本。
func TestStreamThinkStripped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sseLines(w,
			`{"choices":[{"delta":{"role":"assistant"}}]}`,
			`{"choices":[{"delta":{"content":"<think>hid"}}]}`,
			`{"choices":[{"delta":{"content":"den</think>ans"}}]}`,
			`{"choices":[{"delta":{"content":"wer"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`[DONE]`,
		)
	}))
	defer srv.Close()

	var deltas []string
	c := NewClient(srv.URL, "k", "m1", time.Second)
	resp, err := c.Chat(stdctx.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}},
		func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Message.Content != "answer" {
		t.Fatalf("content must be stripped of the think block: %q", resp.Message.Content)
	}
	if got := strings.Join(deltas, ""); got != "answer" {
		t.Fatalf("onDelta stream must be stripped too: %q (deltas %q)", got, deltas)
	}
}

// Contract/ChatTools：tools 请求的线格式（spec M2；OpenAI 工具调用协议）。
func TestChatClientTools(t *testing.T) {
	var gotBody wireRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		sseLines(w,
			`{"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"/tmp/x\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`[DONE]`,
		)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m1", time.Second)
	resp, err := c.Chat(stdctx.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "read /tmp/x"}},
		Tools: []ToolSpec{{
			Name: "read_file", Description: "read a file",
			Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}},
	}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if len(gotBody.Tools) != 1 {
		t.Fatalf("request tools: %+v", gotBody.Tools)
	}
	wt := gotBody.Tools[0]
	if wt.Type != "function" || wt.Function.Name != "read_file" || wt.Function.Description != "read a file" {
		t.Fatalf("wire tool: %+v", wt)
	}
	if string(wt.Function.Parameters) != `{"type":"object","properties":{"path":{"type":"string"}}}` {
		t.Fatalf("wire tool parameters: %s", wt.Function.Parameters)
	}

	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("response tool_calls: %+v", resp.Message)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Type != "function" || tc.Function.Name != "read_file" || tc.Function.Arguments != `{"path":"/tmp/x"}` {
		t.Fatalf("tool_call: %+v", tc)
	}

	// 无工具时线格式省略 tools 字段。
	b, err := json.Marshal(wireRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "tools") {
		t.Fatalf("tools field must be omitted when empty: %s", b)
	}
}
