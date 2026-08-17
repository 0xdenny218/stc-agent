package model

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Contract/ChatClient：请求格式、auth、错误分类（spec M1 里程碑级场景）。
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
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi there"}}]}`)
		}))
		defer srv.Close()

		c := NewClient(srv.URL, "k", "m1", time.Second)
		resp, err := c.Chat(stdctx.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if resp.Message.Role != "assistant" || resp.Message.Content != "hi there" {
			t.Fatalf("unexpected reply: %+v", resp.Message)
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
		if gotBody.Model != "m1" || gotBody.Stream {
			t.Fatalf("body: %+v", gotBody)
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
		_, err := c.Chat(stdctx.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
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
		_, err := c.Chat(stdctx.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
		var ce *ChatError
		if !errors.As(err, &ce) || ce.Kind != KindTimeout {
			t.Fatalf("want KindTimeout, got %v", err)
		}
	})

	t.Run("no choices", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[]}`)
		}))
		defer srv.Close()

		c := NewClient(srv.URL, "k", "m1", time.Second)
		_, err := c.Chat(stdctx.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}})
		var ce *ChatError
		if !errors.As(err, &ce) || ce.Kind != KindProtocol {
			t.Fatalf("want KindProtocol, got %v", err)
		}
	})
}

// Contract/ChatTools：tools 请求的线格式与 tool_calls 响应的解析
// （spec M2；OpenAI 工具调用协议）。
func TestChatClientTools(t *testing.T) {
	var gotBody wireRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"/tmp/x\"}"}}]}}]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m1", time.Second)
	resp, err := c.Chat(stdctx.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "read /tmp/x"}},
		Tools: []ToolSpec{{
			Name: "read_file", Description: "read a file",
			Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}},
	})
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
