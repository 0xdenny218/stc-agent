package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xdenny218/stc-agent/internal/model"
	"github.com/0xdenny218/stc-agent/internal/session"
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

	t.Run("print mode", func(t *testing.T) {
		for _, args := range [][]string{{"-p", "hi"}, {"--print", "hi"}} {
			opts, err := parseOptions(args, env(nil))
			if err != nil {
				t.Fatalf("parseOptions %v: %v", args, err)
			}
			if opts.print != "hi" {
				t.Fatalf("print: %q", opts.print)
			}
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

	t.Run("approval policy", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(p, []byte(`{"approval":{"allow":["read_file","dice"],"deny":["shell"]}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		opts, err := parseOptions([]string{"--config", p}, env(nil))
		if err != nil {
			t.Fatalf("parseOptions: %v", err)
		}
		if !reflect.DeepEqual(opts.policy.Allow, []string{"read_file", "dice"}) ||
			!reflect.DeepEqual(opts.policy.Deny, []string{"shell"}) {
			t.Fatalf("file policy replaces default: %+v", opts.policy)
		}

		opts, err = parseOptions([]string{"--config", p, "--allow", "dice2,dice3"}, env(nil))
		if err != nil {
			t.Fatalf("parseOptions: %v", err)
		}
		if !reflect.DeepEqual(opts.policy.Allow, []string{"read_file", "dice", "dice2", "dice3"}) {
			t.Fatalf("--allow appends to policy: %+v", opts.policy)
		}

		opts, err = parseOptions(nil, env(nil))
		if err != nil {
			t.Fatalf("parseOptions: %v", err)
		}
		if !reflect.DeepEqual(opts.policy.Allow, []string{"read_file", "inspect_agent", "glob", "grep"}) || len(opts.policy.Deny) != 0 {
			t.Fatalf("default policy: %+v", opts.policy)
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

// readTranscript 解码事件日志 transcript，返回消息投影、用量与审批事件。
func readTranscript(t *testing.T, path string) (msgs []model.Message, usages []model.Usage, approvals []session.Approval) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	for dec.More() {
		var ev session.Event
		if err := dec.Decode(&ev); err != nil {
			t.Fatalf("decode transcript event: %v", err)
		}
		switch ev.Type {
		case session.EventMessage:
			msgs = append(msgs, *ev.Message)
		case session.EventUsage:
			usages = append(usages, *ev.Usage)
		case session.EventApproval:
			approvals = append(approvals, *ev.Approval)
		case session.EventTodo, session.EventCompaction, session.EventTitle:
			// M9/M10 事件：合法载荷，直接断言它们的测试自行读文件。
		default:
			t.Fatalf("unknown event type %q", ev.Type)
		}
	}
	return msgs, usages, approvals
}

// serveIn 启动 agent（脚本化 stdin），返回喂输入、等输出、等退出的助手。
func serveIn(t *testing.T, args []string) (write func(string), waitFor func(string), waitExit func() int) {
	t.Helper()
	inR, inW := io.Pipe()
	t.Cleanup(func() { inW.Close() })
	out := &syncBuffer{}
	exit := make(chan int, 1)
	go func() { exit <- run(args, inR, out, func(string) string { return "" }) }()

	write = func(s string) {
		t.Helper()
		if _, err := io.WriteString(inW, s); err != nil {
			t.Fatalf("feed stdin: %v", err)
		}
	}
	waitFor = func(what string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !out.Contains(what) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %q; output:\n%s", what, out.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitExit = func() int {
		t.Helper()
		select {
		case code := <-exit:
			return code
		case <-time.After(5 * time.Second):
			t.Fatalf("run did not exit")
			return -1
		}
	}
	return write, waitFor, waitExit
}

// E2E：脚本化一轮对话 → /model 换模型 → 再一轮 → /quit。断言：
//   - mock 服务器看到的模型序列 [alpha, beta]（级联重载生效）；
//   - 第二次请求带 3 条消息（换模型后历史逐字保留）；
//   - 请求恒为流式且声明 include_usage；transcript 事件日志逐字投影 4 条消息。
func TestE2EModelSwitchKeepsHistory(t *testing.T) {
	mock := testutil.NewMockChat(func(_ int, r testutil.RecordedRequest) model.Message {
		return model.Message{Role: "assistant", Content: "reply-from-" + r.Model}
	})
	defer mock.Close()

	transcript := filepath.Join(t.TempDir(), "chat.jsonl")
	write, waitFor, waitExit := serveIn(t, []string{
		"--api-key", "test",
		"--base-url", mock.URL(),
		"--model", "alpha",
		"--transcript", transcript,
	})

	write("hello\n")
	waitFor("reply-from-alpha")
	write("/model beta\n")
	waitFor("model switched to beta")
	write("again\n")
	waitFor("reply-from-beta")
	write("/quit\n")

	if code := waitExit(); code != 0 {
		t.Fatalf("exit code %d", code)
	}

	reqs := mock.Requests()
	if len(reqs) != 2 {
		t.Fatalf("requests: %d", len(reqs))
	}
	if reqs[0].Model != "alpha" || reqs[1].Model != "beta" {
		t.Fatalf("models seen by server: %q / %q", reqs[0].Model, reqs[1].Model)
	}
	if len(reqs[0].Messages) != 1 || len(reqs[1].Messages) != 3 {
		t.Fatalf("message counts per request: %d / %d (history lost across model switch?)",
			len(reqs[0].Messages), len(reqs[1].Messages))
	}
	for i, r := range reqs {
		if !r.Stream || !r.IncludeUsage {
			t.Fatalf("request %d not streaming or missing include_usage: %+v", i+1, r)
		}
	}

	msgs, usages, _ := readTranscript(t, transcript)
	want := []model.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "reply-from-alpha"},
		{Role: "user", Content: "again"},
		{Role: "assistant", Content: "reply-from-beta"},
	}
	if !reflect.DeepEqual(msgs, want) {
		t.Fatalf("transcript projection:\n got %+v\nwant %+v", msgs, want)
	}
	if len(usages) != 2 || usages[0].Model != "alpha" || usages[1].Model != "beta" {
		t.Fatalf("usage events: %+v", usages)
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

// E2E/ToolCallLoop：mock 先要求 read_file，再给出最终答复。断言：
//   - stdout 出现工具轨迹 "→ read_file(...)" 与流式组装出的最终答复；
//   - 第二次请求携带 3 条消息（user/assistant/tool），tool 消息内容为文件
//     内容（真实工具执行，非 mock）；
//   - 首个请求带全部三个工具；transcript 投影逐字 4 条消息。
func TestE2EToolCallLoop(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(target, []byte("hello-e2e"), 0o600); err != nil {
		t.Fatal(err)
	}

	mock := testutil.NewMockChat(func(n int, _ testutil.RecordedRequest) model.Message {
		if n == 1 {
			args, _ := json.Marshal(map[string]string{"path": target})
			return model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{
				ID: "call_1", Type: "function",
				Function: model.ToolCallFunction{Name: "read_file", Arguments: string(args)},
			}}}
		}
		return model.Message{Role: "assistant", Content: "The file says hello-e2e"}
	})
	defer mock.Close()

	transcript := filepath.Join(t.TempDir(), "chat.jsonl")
	write, waitFor, waitExit := serveIn(t, []string{
		"--api-key", "test",
		"--base-url", mock.URL(),
		"--model", "m",
		"--transcript", transcript,
	})

	write("please read the note\n")
	waitFor("The file says hello-e2e")
	write("/quit\n")
	if code := waitExit(); code != 0 {
		t.Fatalf("exit code %d", code)
	}

	reqs := mock.Requests()
	if len(reqs) != 2 {
		t.Fatalf("requests: %d", len(reqs))
	}
	if len(reqs[0].Messages) != 1 || len(reqs[1].Messages) != 3 {
		t.Fatalf("message counts per request: %d / %d (tool round-trip lost?)",
			len(reqs[0].Messages), len(reqs[1].Messages))
	}
	if !reflect.DeepEqual(reqs[0].ToolNames, []string{"define_guest", "edit", "exit_plan_mode", "glob", "grep", "inspect_agent", "job_kill", "job_list", "job_start", "read_file", "session_title", "shell", "spill", "task", "todo_write", "web_fetch", "web_search", "write_file"}) {
		t.Fatalf("tools advertised: %v", reqs[0].ToolNames)
	}
	var toolMsg model.Message
	for _, m := range reqs[1].Messages {
		if m.Role == "tool" {
			toolMsg = m
		}
	}
	if toolMsg.ToolCallID != "call_1" || !strings.Contains(toolMsg.Content, "hello-e2e") {
		t.Fatalf("tool message in second request: %+v", toolMsg)
	}

	msgs, _, _ := readTranscript(t, transcript)
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

// E2E/SubagentTask（spec M9 验收）：父模型调 task 工具，子 agent 在子
// 作用域用工具子集独立跑轮，终答以工具消息回流父会话事件。请求计数
// 全局共享（父子同走一个 mock）：n1 父请求 → task 调用；n2 子请求 →
// 终答；n3 父续轮 → 最终答复。
func TestE2ESubagentTask(t *testing.T) {
	mock := testutil.NewMockChat(func(n int, _ testutil.RecordedRequest) model.Message {
		switch n {
		case 1:
			return model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{
				ID: "call_t", Type: "function",
				Function: model.ToolCallFunction{
					Name: "task", Arguments: `{"prompt":"count the widgets in the warehouse"}`,
				},
			}}}
		case 2: // 子 agent 的独立一轮：直接给终答
			return model.Message{Role: "assistant", Content: "child digest: 7 widgets"}
		default:
			return model.Message{Role: "assistant", Content: "parent done: warehouse counted"}
		}
	})
	defer mock.Close()

	transcript := filepath.Join(t.TempDir(), "chat.jsonl")
	write, waitFor, waitExit := serveIn(t, []string{
		"--api-key", "test",
		"--base-url", mock.URL(),
		"--model", "m",
		"--transcript", transcript,
		"--allow", "task",
	})

	write("delegate the count\n")
	waitFor("parent done: warehouse counted")
	write("/quit\n")

	if code := waitExit(); code != 0 {
		t.Fatalf("exit code: %d", code)
	}

	reqs := mock.Requests()
	if len(reqs) != 3 {
		t.Fatalf("requests (parent + child + parent): %d", len(reqs))
	}
	// 子请求：全新会话——仅一条 user 消息（子 prompt），父历史不可见。
	if len(reqs[1].Messages) != 1 || reqs[1].Messages[0].Role != "user" ||
		reqs[1].Messages[0].Content != "count the widgets in the warehouse" {
		t.Fatalf("child request messages: %+v", reqs[1].Messages)
	}
	// 子工具集：父工具去 task（不递归），其余照常可用。
	for _, name := range reqs[1].ToolNames {
		if name == "task" {
			t.Fatalf("task must not recurse into the sub-agent: %v", reqs[1].ToolNames)
		}
	}
	if !reflect.DeepEqual(reqs[1].ToolNames,
		[]string{"define_guest", "edit", "exit_plan_mode", "glob", "grep", "inspect_agent", "job_kill", "job_list", "job_start", "read_file", "session_title", "shell", "spill", "todo_write", "web_fetch", "web_search", "write_file"}) {
		t.Fatalf("child tools: %v", reqs[1].ToolNames)
	}
	// 终答回流：父第三请求里的 tool 消息 = 子终答。
	var toolMsg model.Message
	for _, m := range reqs[2].Messages {
		if m.Role == "tool" {
			toolMsg = m
		}
	}
	if toolMsg.ToolCallID != "call_t" || toolMsg.Content != "child digest: 7 widgets" {
		t.Fatalf("child answer must flow back as tool result: %+v", toolMsg)
	}

	msgs, _, _ := readTranscript(t, transcript)
	if len(msgs) != 4 || msgs[2].Role != "tool" || msgs[2].Content != "child digest: 7 widgets" {
		t.Fatalf("transcript must carry the child answer as a tool message: %+v", msgs)
	}
}

// E2E/StreamTurn（spec M5）：一轮纯答复对话，mock 把答复分片流式发出。
// 断言 stdout 呈现按序组装的完整答复；transcript 是类型化事件日志
// （消息 + 用量），用量值与 mock 发出的完全一致。
func TestE2EStreamTurn(t *testing.T) {
	mock := testutil.NewMockChat(func(_ int, _ testutil.RecordedRequest) model.Message {
		return model.Message{Role: "assistant", Content: "streamed answer, assembled in order"}
	})
	defer mock.Close()

	transcript := filepath.Join(t.TempDir(), "chat.jsonl")
	write, waitFor, waitExit := serveIn(t, []string{
		"--api-key", "test",
		"--base-url", mock.URL(),
		"--model", "m",
		"--transcript", transcript,
	})

	write("say hi\n")
	waitFor("streamed answer, assembled in order")
	write("/quit\n")
	if code := waitExit(); code != 0 {
		t.Fatalf("exit code %d", code)
	}

	// 事件日志逐行是类型化 JSON；消息投影 + 一条用量事件（n=1 →
	// prompt 11 / completion 5 / total 16，模型名由客户端回填）。
	raw, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	for i, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var ev session.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %d is not a typed event: %v", i+1, err)
		}
	}
	msgs, usages, _ := readTranscript(t, transcript)
	want := []model.Message{
		{Role: "user", Content: "say hi"},
		{Role: "assistant", Content: "streamed answer, assembled in order"},
	}
	if !reflect.DeepEqual(msgs, want) {
		t.Fatalf("transcript projection:\n got %+v\nwant %+v", msgs, want)
	}
	if len(usages) != 1 || usages[0].TotalTokens != 16 || usages[0].Model != "m" {
		t.Fatalf("usage events: %+v", usages)
	}
}

// M5：`-p` headless 一次性模式——跑一轮打印答案退出（exit 0），不读
// stdin、不进 REPL。
func TestPrintMode(t *testing.T) {
	mock := testutil.NewMockChat(func(_ int, _ testutil.RecordedRequest) model.Message {
		return model.Message{Role: "assistant", Content: "one-shot answer"}
	})
	defer mock.Close()

	transcript := filepath.Join(t.TempDir(), "chat.jsonl")
	out := &syncBuffer{}
	code := run([]string{
		"--api-key", "test",
		"--base-url", mock.URL(),
		"--model", "m",
		"--transcript", transcript,
		"-p", "hello",
	}, strings.NewReader(""), out, func(string) string { return "" })
	if code != 0 {
		t.Fatalf("exit code %d; output:\n%s", code, out.String())
	}
	if !out.Contains("one-shot answer") {
		t.Fatalf("answer missing; output:\n%s", out.String())
	}
	msgs, usages, _ := readTranscript(t, transcript)
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Content != "one-shot answer" {
		t.Fatalf("transcript: %+v", msgs)
	}
	if len(usages) != 1 {
		t.Fatalf("usage events: %+v", usages)
	}
}

// E2E/HotSwapKeepsSession（spec M3 验收）：对话进行中热替换 guest 工具。
// 第一轮模型调用 dice（v1 结果）→ 进程内重建 dice.wasm 为 v2 →
// 第二轮即走 v2；同一会话进程，历史逐字保留。需要 TinyGo（缺失即 Skip）。
func TestE2EHotSwapKeepsSession(t *testing.T) {
	toolsDir := t.TempDir()
	dicePath := testutil.BuildGuest(t, "examples/guests/dice", filepath.Join(toolsDir, "dice.wasm"))

	mock := testutil.NewMockChat(func(n int, _ testutil.RecordedRequest) model.Message {
		switch n {
		case 1, 3: // 两轮都要求掷骰子
			return model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{
				ID: fmt.Sprintf("call_%d", n), Type: "function",
				Function: model.ToolCallFunction{Name: "dice", Arguments: "{}"},
			}}}
		case 2:
			return model.Message{Role: "assistant", Content: "first roll done"}
		default:
			return model.Message{Role: "assistant", Content: "second roll done"}
		}
	})
	defer mock.Close()

	transcript := filepath.Join(t.TempDir(), "chat.jsonl")
	write, waitFor, waitExit := serveIn(t, []string{
		"--api-key", "test",
		"--base-url", mock.URL(),
		"--model", "m",
		"--transcript", transcript,
		"--tools-dir", toolsDir,
		"--allow", "dice", // M6 起 guest 工具默认要审批；本测试与审批策略无关
	})

	write("roll\n")
	waitFor("first roll done")
	waitFor("→ dice(")

	// 对话进行中把 dice.wasm 重建为 v2；等热替换落定。
	testutil.BuildGuest(t, "examples/guests/dice", dicePath, "v2")
	waitFor("[guest] dice reloaded")

	write("again\n")
	waitFor("second roll done")
	write("/quit\n")

	if code := waitExit(); code != 0 {
		t.Fatalf("exit code %d", code)
	}

	reqs := mock.Requests()
	if len(reqs) != 4 {
		t.Fatalf("requests: %d", len(reqs))
	}
	if !reflect.DeepEqual(reqs[0].ToolNames, reqs[2].ToolNames) {
		t.Fatalf("tool list changed across hot-swap: %v vs %v", reqs[0].ToolNames, reqs[2].ToolNames)
	}
	if !strings.Contains(strings.Join(reqs[0].ToolNames, ","), "dice") {
		t.Fatalf("guest tool not advertised: %v", reqs[0].ToolNames)
	}
	lastToolMsg := func(n int) string {
		last := ""
		for _, m := range reqs[n-1].Messages {
			if m.Role == "tool" {
				last = m.Content
			}
		}
		return last
	}
	if got := lastToolMsg(2); !strings.Contains(got, `"version":"v1"`) {
		t.Fatalf("turn 1 tool result should be v1: %q", got)
	}
	if got := lastToolMsg(4); !strings.Contains(got, `"version":"v2"`) {
		t.Fatalf("turn 2 tool result should be v2: %q", got)
	}

	// 历史逐字：8 条消息、角色序、v1 结果在前 v2 在后。
	msgs, _, _ := readTranscript(t, transcript)
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

// E2E/ApprovalGate（spec M6 验收）：默认策略下 write_file/shell 需批准、
// read_file 直接放行。模型先调 write_file（用户拒）→ 拒绝回灌且工具不
// 执行；再调 shell（用户准）→ 执行；两个决定都入事件日志；第二轮
// read_file 无询问、无事件。
func TestE2EApprovalGate(t *testing.T) {
	dir := t.TempDir()
	readTarget := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(readTarget, []byte("approval-e2e"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeTarget := filepath.Join(dir, "out.txt")

	toolCall := func(id, name, args string) model.Message {
		return model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{
			ID: id, Type: "function",
			Function: model.ToolCallFunction{Name: name, Arguments: args},
		}}}
	}
	mock := testutil.NewMockChat(func(n int, _ testutil.RecordedRequest) model.Message {
		switch n {
		case 1:
			return toolCall("call_w", "write_file",
				fmt.Sprintf(`{"path":%q,"content":"x"}`, writeTarget))
		case 2: // write_file 被拒后改调 shell
			return toolCall("call_s", "shell", `{"command":"echo ran"}`)
		case 3:
			return model.Message{Role: "assistant", Content: "gate turn done"}
		case 4:
			return toolCall("call_r", "read_file", fmt.Sprintf(`{"path":%q}`, readTarget))
		default:
			return model.Message{Role: "assistant", Content: "read turn done"}
		}
	})
	defer mock.Close()

	transcript := filepath.Join(t.TempDir(), "chat.jsonl")
	write, waitFor, waitExit := serveIn(t, []string{
		"--api-key", "test",
		"--base-url", mock.URL(),
		"--model", "m",
		"--transcript", transcript,
	})

	write("create the file\n")
	waitFor(`! allow "write_file" to run?`)
	write("n\n") // 拒绝
	waitFor(`! allow "shell" to run?`)
	write("y\n") // 批准
	waitFor("gate turn done")
	write("read it back\n")
	waitFor("read turn done")
	write("/quit\n")
	if code := waitExit(); code != 0 {
		t.Fatalf("exit code %d", code)
	}

	// write_file 被拒绝 → 从未执行；shell 经批准执行，结果回灌。
	if _, err := os.Stat(writeTarget); !os.IsNotExist(err) {
		t.Fatalf("write_file executed despite denial")
	}
	reqs := mock.Requests()
	if len(reqs) != 5 {
		t.Fatalf("requests: %d", len(reqs))
	}
	var denial, shellResult string
	for _, m := range reqs[1].Messages {
		if m.Role == "tool" {
			denial = m.Content
		}
	}
	if !strings.Contains(denial, "denied by user") {
		t.Fatalf("denial must feed back to the model: %q", denial)
	}
	for _, m := range reqs[2].Messages {
		if m.Role == "tool" && m.ToolCallID == "call_s" {
			shellResult = m.Content
		}
	}
	if shellResult != "ran\n" {
		t.Fatalf("approved shell should have run: %q", shellResult)
	}

	// 审批事件：恰两条——write_file deny/user、shell allow/user；
	// read_file 直接放行（无询问、无事件），其工具结果照常回灌。
	_, _, approvals := readTranscript(t, transcript)
	if len(approvals) != 2 {
		t.Fatalf("approval events: %+v", approvals)
	}
	if approvals[0].Tool != "write_file" || approvals[0].Decision != "deny" || approvals[0].Source != "user" {
		t.Fatalf("first decision: %+v", approvals[0])
	}
	if approvals[1].Tool != "shell" || approvals[1].Decision != "allow" || approvals[1].Source != "user" {
		t.Fatalf("second decision: %+v", approvals[1])
	}
	var readResult string
	for _, m := range reqs[4].Messages {
		if m.Role == "tool" && m.ToolCallID == "call_r" {
			readResult = m.Content
		}
	}
	if readResult != "approval-e2e" {
		t.Fatalf("read_file should pass without asking: %q", readResult)
	}
}

// E2E/SkillHotLoad（spec M8 验收）：对话进行中落盘一个 skill（SKILL.md），
// 其 prompt 段落即时拼进 system prompt（不重启）；删除 skill 目录后段落
// 消失。三轮对话：装前无段落 → 装后有段落（identity 段在前）→ 卸后无段落。
func TestE2ESkillHotLoad(t *testing.T) {
	skillsDir := t.TempDir()

	mock := testutil.NewMockChat(func(n int, _ testutil.RecordedRequest) model.Message {
		return model.Message{Role: "assistant", Content: fmt.Sprintf("turn-%d", n)}
	})
	defer mock.Close()

	transcript := filepath.Join(t.TempDir(), "chat.jsonl")
	write, waitFor, waitExit := serveIn(t, []string{
		"--api-key", "test",
		"--base-url", mock.URL(),
		"--model", "m",
		"--transcript", transcript,
		"--skills-dir", skillsDir,
	})

	write("hi\n")
	waitFor("turn-1")

	// 对话进行中落盘 skill → 段落即时注册（loaded 消息在 Ready 之后打印，
	// 看到它即段落已在注册表）。
	skillDir := filepath.Join(skillsDir, "greeter")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\ndescription: greet politely\n---\nBe friendly."), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor("[skill] greeter loaded")

	write("hi again\n")
	waitFor("turn-2")

	// 删除 skill 目录 → 段落即时注销。
	if err := os.RemoveAll(skillDir); err != nil {
		t.Fatal(err)
	}
	waitFor("[skill] greeter unloaded")

	write("third\n")
	waitFor("turn-3")
	write("/quit\n")
	if code := waitExit(); code != 0 {
		t.Fatalf("exit code %d", code)
	}

	reqs := mock.Requests()
	if len(reqs) != 3 {
		t.Fatalf("requests: %d", len(reqs))
	}
	if strings.Contains(reqs[0].System, "Be friendly.") {
		t.Fatalf("turn 1 system must not contain the skill segment: %q", reqs[0].System)
	}
	idIdx := strings.Index(reqs[1].System, "You are stc-agent")
	skIdx := strings.Index(reqs[1].System, "Be friendly.")
	if idIdx < 0 || skIdx < 0 || idIdx > skIdx {
		t.Fatalf("turn 2 system = identity + skill segment in name order: %q", reqs[1].System)
	}
	if strings.Contains(reqs[2].System, "Be friendly.") {
		t.Fatalf("turn 3 system must drop the unloaded skill segment: %q", reqs[2].System)
	}
}

// E2E/MCPToolCall（spec M8 验收）：MCP stdio server 的工具经 agent 循环
// 被调用（模型点名 mcp__echo__echo，结果回灌）；server 断开后工具从
// toolset 消失（下一轮请求的 ToolNames 里不再有它）。echo server 配
// ECHO_DIE_AFTER_CALLS=1：答完第一次 tools/call 即退出（确定性断开）。
func TestE2EMCPToolCall(t *testing.T) {
	echoBin := filepath.Join(t.TempDir(), "echo-server")
	if b, err := exec.Command("go", "build", "-o", echoBin, "../../examples/mcp/echo").CombinedOutput(); err != nil {
		t.Fatalf("build echo server: %v\n%s", err, b)
	}
	// MCP 子进程继承 agent 的环境变量；echo 答完 1 次 tools/call 即退出。
	t.Setenv("ECHO_DIE_AFTER_CALLS", "1")

	mock := testutil.NewMockChat(func(n int, _ testutil.RecordedRequest) model.Message {
		switch n {
		case 1:
			return model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{
				ID: "call_e", Type: "function",
				Function: model.ToolCallFunction{Name: "mcp__echo__echo", Arguments: `{"text":"ping"}`},
			}}}
		case 2:
			return model.Message{Role: "assistant", Content: "mcp answered"}
		default:
			return model.Message{Role: "assistant", Content: "after disconnect"}
		}
	})
	defer mock.Close()

	transcript := filepath.Join(t.TempDir(), "chat.jsonl")
	write, waitFor, waitExit := serveIn(t, []string{
		"--api-key", "test",
		"--base-url", mock.URL(),
		"--model", "m",
		"--transcript", transcript,
		"--mcp", "echo=" + echoBin,
		"--allow", "mcp__echo__echo", // M6 起 MCP 工具默认要审批；本测试与审批策略无关
	})

	write("use mcp\n")
	waitFor("mcp answered")

	// echo 答完即退出 → 断开协程注销工具并上报（状态消息在注销之后打印）。
	waitFor("[mcp] echo disconnected")

	write("again\n")
	waitFor("after disconnect")
	write("/quit\n")
	if code := waitExit(); code != 0 {
		t.Fatalf("exit code %d", code)
	}

	reqs := mock.Requests()
	if len(reqs) != 3 {
		t.Fatalf("requests: %d", len(reqs))
	}
	if !strings.Contains(strings.Join(reqs[0].ToolNames, ","), "mcp__echo__echo") {
		t.Fatalf("mcp tool not advertised: %v", reqs[0].ToolNames)
	}
	var toolResult string
	for _, m := range reqs[1].Messages {
		if m.Role == "tool" && m.ToolCallID == "call_e" {
			toolResult = m.Content
		}
	}
	if !strings.Contains(toolResult, "ping") {
		t.Fatalf("mcp tool result should echo back: %q", toolResult)
	}
	if strings.Contains(strings.Join(reqs[2].ToolNames, ","), "mcp__echo__echo") {
		t.Fatalf("tool must vanish after server disconnect: %v", reqs[2].ToolNames)
	}
}

// E2E/AuthoredGuestTool（spec M10 验收）：模型经 define_guest 写一份 Go
// guest 源码，宿主用 TinyGo 编译成 wasm 并装载为普通工具；下一轮模型直接
// 调用这个新工具，结果真实来自 wasm（非 mock）。断言 define 结果回灌、
// echo 工具执行、源码与产物落在 authored 目录。需要 TinyGo（缺失即 Skip）。
func TestE2EAuthoredGuestTool(t *testing.T) {
	tg := testutil.TinygoPath(t) // 本地无 tinygo 则跳过；CI 有
	authoredDir := t.TempDir()

	// echoSource 提供 tool.echo，invoke 回显前缀（与 define 包单测同构）。
	echoSource := `package main

import "github.com/0xdenny218/stc-go/guest"

func init() {
	guest.OnInvoke(func(args string) string {
		return "authored:" + args
	})
}

//export start
func start() {
	_ = guest.Provide("tool.echo", ` + "`" + `{"name":"echo","description":"echo back","parameters":{"type":"object","properties":{}}}` + "`" + `)
}

func main() {}
`
	defineArgs, err := json.Marshal(map[string]string{"name": "echo", "source": echoSource})
	if err != nil {
		t.Fatal(err)
	}
	mock := testutil.NewMockChat(func(n int, _ testutil.RecordedRequest) model.Message {
		switch n {
		case 1: // 模型写源码 → define_guest
			return model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{
				ID: "call_d", Type: "function",
				Function: model.ToolCallFunction{Name: "define_guest", Arguments: string(defineArgs)},
			}}}
		case 2: // 新工具已就位：直接调用它
			return model.Message{Role: "assistant", ToolCalls: []model.ToolCall{{
				ID: "call_e", Type: "function",
				Function: model.ToolCallFunction{Name: "echo", Arguments: `{"hi":1}`},
			}}}
		default:
			return model.Message{Role: "assistant", Content: "authored guest works"}
		}
	})
	defer mock.Close()

	transcript := filepath.Join(t.TempDir(), "chat.jsonl")
	write, waitFor, waitExit := serveIn(t, []string{
		"--api-key", "test",
		"--base-url", mock.URL(),
		"--model", "m",
		"--transcript", transcript,
		"--authored-dir", authoredDir,
		"--tinygo", tg,
		"--allow", "define_guest,echo", // M6 起二者默认要审批；本测试与审批策略无关
	})

	write("build a tool\n")
	waitFor("authored guest works")
	write("/quit\n")
	if code := waitExit(); code != 0 {
		t.Fatalf("exit code %d", code)
	}

	reqs := mock.Requests()
	if len(reqs) != 3 {
		t.Fatalf("requests: %d", len(reqs))
	}
	// 定义结果回灌：define_guest 真实执行（TinyGo 编译 + guest.Load）。
	var defineResult string
	for _, m := range reqs[1].Messages {
		if m.Role == "tool" && m.ToolCallID == "call_d" {
			defineResult = m.Content
		}
	}
	if !strings.Contains(defineResult, `guest tool "echo" defined`) {
		t.Fatalf("define_guest result must report the loaded guest: %q", defineResult)
	}
	// 新工具真实执行（wasm 调用，非 mock 代答）。
	var echoResult string
	for _, m := range reqs[2].Messages {
		if m.Role == "tool" && m.ToolCallID == "call_e" {
			echoResult = m.Content
		}
	}
	if echoResult != `authored:{"hi":1}` {
		t.Fatalf("echo tool result: %q", echoResult)
	}
	// 源码与产物落在 authored 目录。
	if _, err := os.Stat(filepath.Join(authoredDir, "echo.go")); err != nil {
		t.Fatalf("authored source missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(authoredDir, "echo.wasm")); err != nil {
		t.Fatalf("authored wasm missing: %v", err)
	}
}
