package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xdenny218/stc-agent/internal/model"
	"github.com/0xdenny218/stc-agent/internal/testutil"
)

func TestParseOptions(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // 隔离默认配置文件路径
	env := func(kv map[string]string) func(string) string {
		return func(k string) string { return kv[k] }
	}

	t.Run("defaults", func(t *testing.T) {
		opts, err := parseOptions(nil, env(nil))
		if err != nil {
			t.Fatalf("parseOptions: %v", err)
		}
		if opts.cfg.BaseURL != "https://api.deepseek.com" || opts.cfg.Model != "deepseek-chat" || opts.cfg.Timeout != 60*time.Second {
			t.Fatalf("defaults: %+v", opts.cfg)
		}
	})

	t.Run("flag beats env beats default", func(t *testing.T) {
		opts, err := parseOptions([]string{"--model", "flagm"}, env(map[string]string{
			"STC_AGENT_MODEL":   "envm",
			"STC_AGENT_API_KEY": "envkey",
		}))
		if err != nil {
			t.Fatalf("parseOptions: %v", err)
		}
		if opts.cfg.Model != "flagm" {
			t.Fatalf("flag should win: %q", opts.cfg.Model)
		}
		if opts.cfg.APIKey != "envkey" {
			t.Fatalf("env should fill api key: %q", opts.cfg.APIKey)
		}

		opts, err = parseOptions(nil, env(map[string]string{"STC_AGENT_MODEL": "envm"}))
		if err != nil {
			t.Fatalf("parseOptions: %v", err)
		}
		if opts.cfg.Model != "envm" {
			t.Fatalf("env should win over default: %q", opts.cfg.Model)
		}
	})

	t.Run("deepseek env fallback", func(t *testing.T) {
		opts, err := parseOptions(nil, env(map[string]string{"DEEPSEEK_API_KEY": "dk"}))
		if err != nil {
			t.Fatalf("parseOptions: %v", err)
		}
		if opts.cfg.APIKey != "dk" {
			t.Fatalf("DEEPSEEK_API_KEY fallback: %q", opts.cfg.APIKey)
		}
	})

	t.Run("resume sets transcript", func(t *testing.T) {
		opts, err := parseOptions([]string{"--resume", "x.jsonl"}, env(nil))
		if err != nil {
			t.Fatalf("parseOptions: %v", err)
		}
		if opts.transcript != "x.jsonl" {
			t.Fatalf("transcript: %q", opts.transcript)
		}
	})

	t.Run("config file", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(p, []byte(`{"model":"filem","api_key":"filekey"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		opts, err := parseOptions([]string{"--config", p}, env(nil))
		if err != nil {
			t.Fatalf("parseOptions: %v", err)
		}
		if opts.cfg.Model != "filem" || opts.cfg.APIKey != "filekey" {
			t.Fatalf("file config: %+v", opts.cfg)
		}
	})
}

// syncBuffer 给 REPL goroutine 与断言共用一个输出缓冲。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *syncBuffer) Contains(sub string) bool { return strings.Contains(s.String(), sub) }

// E2E：脚本化一轮对话 → /model 换模型 → 再一轮 → /quit。断言：
//   - mock 服务器看到的模型序列 [alpha, beta]（级联重载生效）；
//   - 第二次请求带 3 条消息（换模型后历史逐字保留）；
//   - transcript 逐字记录 4 条消息。
func TestE2EModelSwitchKeepsHistory(t *testing.T) {
	var mu sync.Mutex
	var models []string
	var counts []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string            `json:"model"`
			Messages []json.RawMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		models = append(models, req.Model)
		counts = append(counts, len(req.Messages))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"reply-from-%s"}}]}`, req.Model)
	}))
	defer srv.Close()

	transcript := filepath.Join(t.TempDir(), "chat.jsonl")
	args := []string{
		"--api-key", "test",
		"--base-url", srv.URL,
		"--model", "alpha",
		"--transcript", transcript,
	}
	inR, inW := io.Pipe()
	defer inW.Close()
	out := &syncBuffer{}
	exit := make(chan int, 1)
	go func() { exit <- run(args, inR, out, func(string) string { return "" }) }()

	write := func(s string) {
		t.Helper()
		if _, err := io.WriteString(inW, s); err != nil {
			t.Fatalf("feed stdin: %v", err)
		}
	}
	waitFor := func(what string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !out.Contains(what) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %q; output:\n%s", what, out.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	write("hello\n")
	waitFor("reply-from-alpha")
	write("/model beta\n")
	waitFor("model switched to beta")
	write("again\n")
	waitFor("reply-from-beta")
	write("/quit\n")

	select {
	case code := <-exit:
		if code != 0 {
			t.Fatalf("exit code %d; output:\n%s", code, out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("run did not exit; output:\n%s", out.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(models, []string{"alpha", "beta"}) {
		t.Fatalf("models seen by server: %v", models)
	}
	if !reflect.DeepEqual(counts, []int{1, 3}) {
		t.Fatalf("message counts per request: %v (history lost across model switch?)", counts)
	}

	f, err := os.Open(transcript)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	defer f.Close()
	var msgs []model.Message
	dec := json.NewDecoder(f)
	for dec.More() {
		var m model.Message
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("decode transcript: %v", err)
		}
		msgs = append(msgs, m)
	}
	want := []model.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "reply-from-alpha"},
		{Role: "user", Content: "again"},
		{Role: "assistant", Content: "reply-from-beta"},
	}
	if !reflect.DeepEqual(msgs, want) {
		t.Fatalf("transcript:\n got %+v\nwant %+v", msgs, want)
	}
}

func TestRunRequiresAPIKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	out := &syncBuffer{}
	code := run([]string{"--base-url", "http://127.0.0.1"}, strings.NewReader(""), out, func(string) string { return "" })
	if code != 2 {
		t.Fatalf("exit code: %d", code)
	}
	if !out.Contains("API key is required") {
		t.Fatalf("output: %s", out.String())
	}
}

// E2E/ToolCallLoop：mock 服务器先要求 read_file，再给出最终答复。断言：
//   - stdout 出现工具轨迹 "→ read_file(...)" 与最终答复；
//   - 第二次请求携带 3 条消息（user/assistant/tool），tool 消息内容为文件
//     内容（真实工具执行，非 mock）；
//   - 首个请求带全部三个工具；transcript 逐字记录 4 条消息。
func TestE2EToolCallLoop(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(target, []byte("hello-e2e"), 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var counts []int
	var toolNames []string
	var toolMsg model.Message
	callN := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []model.Message `json:"messages"`
			Tools    []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		callN++
		n := callN
		counts = append(counts, len(req.Messages))
		if n == 1 {
			for _, tool := range req.Tools {
				toolNames = append(toolNames, tool.Function.Name)
			}
		}
		if n == 2 {
			for _, m := range req.Messages {
				if m.Role == "tool" {
					toolMsg = m
				}
			}
		}
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		var msg model.Message
		if n == 1 {
			args, _ := json.Marshal(map[string]string{"path": target})
			msg = model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{
				ID: "call_1", Type: "function",
				Function: model.ToolCallFunction{Name: "read_file", Arguments: string(args)},
			}}}
		} else {
			msg = model.Message{Role: "assistant", Content: "The file says hello-e2e"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": msg}}})
	}))
	defer srv.Close()

	transcript := filepath.Join(t.TempDir(), "chat.jsonl")
	args := []string{
		"--api-key", "test",
		"--base-url", srv.URL,
		"--model", "m",
		"--transcript", transcript,
	}
	inR, inW := io.Pipe()
	defer inW.Close()
	out := &syncBuffer{}
	exit := make(chan int, 1)
	go func() { exit <- run(args, inR, out, func(string) string { return "" }) }()

	waitFor := func(what string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !out.Contains(what) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %q; output:\n%s", what, out.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	if _, err := io.WriteString(inW, "please read the note\n"); err != nil {
		t.Fatalf("feed stdin: %v", err)
	}
	waitFor("The file says hello-e2e")
	if !out.Contains("→ read_file(") {
		t.Fatalf("tool trace missing; output:\n%s", out.String())
	}
	if _, err := io.WriteString(inW, "/quit\n"); err != nil {
		t.Fatalf("feed stdin: %v", err)
	}

	select {
	case code := <-exit:
		if code != 0 {
			t.Fatalf("exit code %d; output:\n%s", code, out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("run did not exit; output:\n%s", out.String())
	}

	mu.Lock()
	if !reflect.DeepEqual(counts, []int{1, 3}) {
		t.Fatalf("message counts per request: %v (tool round-trip lost?)", counts)
	}
	if !reflect.DeepEqual(toolNames, []string{"read_file", "shell", "write_file"}) {
		t.Fatalf("tools advertised: %v", toolNames)
	}
	if toolMsg.ToolCallID != "call_1" || !strings.Contains(toolMsg.Content, "hello-e2e") {
		t.Fatalf("tool message in second request: %+v", toolMsg)
	}
	mu.Unlock()

	f, err := os.Open(transcript)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	defer f.Close()
	var msgs []model.Message
	dec := json.NewDecoder(f)
	for dec.More() {
		var m model.Message
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("decode transcript: %v", err)
		}
		msgs = append(msgs, m)
	}
	if len(msgs) != 4 {
		t.Fatalf("transcript messages: %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || msgs[1].Role != "assistant" || msgs[2].Role != "tool" || msgs[3].Role != "assistant" {
		t.Fatalf("transcript roles: %+v", msgs)
	}
	if len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].Function.Name != "read_file" {
		t.Fatalf("transcript assistant tool_calls: %+v", msgs[1])
	}
	if msgs[2].ToolCallID != "call_1" || !strings.Contains(msgs[2].Content, "hello-e2e") {
		t.Fatalf("transcript tool message: %+v", msgs[2])
	}
}

// E2E/HotSwapKeepsSession（spec M3 验收）：对话进行中热替换 guest 工具。
// 第一轮模型调用 dice（v1 结果）→ 进程内重建 dice.wasm 为 v2 →
// 第二轮即走 v2；同一会话进程，历史逐字保留。需要 TinyGo（缺失即 Skip）。
func TestE2EHotSwapKeepsSession(t *testing.T) {
	toolsDir := t.TempDir()
	dicePath := testutil.BuildGuest(t, "examples/guests/dice", filepath.Join(toolsDir, "dice.wasm"))

	var mu sync.Mutex
	callN := 0
	toolNames := map[int][]string{} // 每次请求广告的工具表
	lastToolMsg := map[int]string{} // 每次请求历史中最后一条 tool 消息
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []model.Message `json:"messages"`
			Tools    []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		callN++
		n := callN
		for _, tool := range req.Tools {
			toolNames[n] = append(toolNames[n], tool.Function.Name)
		}
		for _, m := range req.Messages {
			if m.Role == "tool" {
				lastToolMsg[n] = m.Content
			}
		}
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		var msg model.Message
		switch n {
		case 1, 3: // 两轮都要求掷骰子
			msg = model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{
				ID: fmt.Sprintf("call_%d", n), Type: "function",
				Function: model.ToolCallFunction{Name: "dice", Arguments: "{}"},
			}}}
		case 2:
			msg = model.Message{Role: "assistant", Content: "first roll done"}
		default:
			msg = model.Message{Role: "assistant", Content: "second roll done"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": msg}}})
	}))
	defer srv.Close()

	transcript := filepath.Join(t.TempDir(), "chat.jsonl")
	args := []string{
		"--api-key", "test",
		"--base-url", srv.URL,
		"--model", "m",
		"--transcript", transcript,
		"--tools-dir", toolsDir,
	}
	inR, inW := io.Pipe()
	defer inW.Close()
	out := &syncBuffer{}
	exit := make(chan int, 1)
	go func() { exit <- run(args, inR, out, func(string) string { return "" }) }()

	write := func(s string) {
		t.Helper()
		if _, err := io.WriteString(inW, s); err != nil {
			t.Fatalf("feed stdin: %v", err)
		}
	}
	waitFor := func(what string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !out.Contains(what) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %q; output:\n%s", what, out.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	write("roll\n")
	waitFor("first roll done")
	if !out.Contains("→ dice(") {
		t.Fatalf("guest tool trace missing; output:\n%s", out.String())
	}

	// 对话进行中把 dice.wasm 重建为 v2；等热替换落定。
	testutil.BuildGuest(t, "examples/guests/dice", dicePath, "v2")
	waitFor("[guest] dice reloaded")

	write("again\n")
	waitFor("second roll done")
	write("/quit\n")

	select {
	case code := <-exit:
		if code != 0 {
			t.Fatalf("exit code %d; output:\n%s", code, out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("run did not exit; output:\n%s", out.String())
	}

	mu.Lock()
	if !reflect.DeepEqual(toolNames[1], toolNames[3]) {
		t.Fatalf("tool list changed across hot-swap: %v vs %v", toolNames[1], toolNames[3])
	}
	if !strings.Contains(strings.Join(toolNames[1], ","), "dice") {
		t.Fatalf("guest tool not advertised: %v", toolNames[1])
	}
	if !strings.Contains(lastToolMsg[2], `"version":"v1"`) {
		t.Fatalf("turn 1 tool result should be v1: %q", lastToolMsg[2])
	}
	if !strings.Contains(lastToolMsg[4], `"version":"v2"`) {
		t.Fatalf("turn 2 tool result should be v2: %q", lastToolMsg[4])
	}
	mu.Unlock()

	// 历史逐字：8 条消息、角色序、v1 结果在前 v2 在后。
	f, err := os.Open(transcript)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	defer f.Close()
	var msgs []model.Message
	dec := json.NewDecoder(f)
	for dec.More() {
		var m model.Message
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("decode transcript: %v", err)
		}
		msgs = append(msgs, m)
	}
	if len(msgs) != 8 {
		t.Fatalf("transcript messages: %d: %+v", len(msgs), msgs)
	}
	roles := make([]string, len(msgs))
	for i, m := range msgs {
		roles[i] = m.Role
	}
	wantRoles := []string{"user", "assistant", "tool", "assistant", "user", "assistant", "tool", "assistant"}
	if !reflect.DeepEqual(roles, wantRoles) {
		t.Fatalf("transcript roles: %v", roles)
	}
	if !strings.Contains(msgs[2].Content, `"version":"v1"`) || !strings.Contains(msgs[6].Content, `"version":"v2"`) {
		t.Fatalf("transcript tool results: %q / %q", msgs[2].Content, msgs[6].Content)
	}
}
