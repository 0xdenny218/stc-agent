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
