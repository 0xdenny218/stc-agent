package session

import (
	stdctx "context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/0xdenny218/stc-agent/internal/model"
	stc "github.com/0xdenny218/stc-go"
)

// Contract/TranscriptEffect（spec M1 里程碑级场景）：写穿、卸载关句柄、
// --resume 逐字 replay。
func TestTranscriptEffect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.jsonl")
	root := stc.New()
	defer root.Close()

	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()

	fib := root.Load(Component(path))
	if err := fib.Ready(ctx); err != nil {
		t.Fatalf("session fiber: %v", err)
	}
	sess, err := stc.Service[*Session](root, KeySession)
	if err != nil {
		t.Fatalf("resolve session: %v", err)
	}
	if err := sess.Add(model.Message{Role: "user", Content: "hi"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := sess.Add(model.Message{Role: "assistant", Content: "yo"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// 写穿：卸载前文件已落两行。
	if got := readTranscriptLines(t, path); len(got) != 2 {
		t.Fatalf("transcript should have 2 lines before unload, got %d", len(got))
	}

	fib.Dispose()
	if err := fib.Gone(ctx); err != nil {
		t.Fatalf("waiting session gone: %v", err)
	}

	// 卸载后：会话关闭，不再写入。
	before := readTranscriptRaw(t, path)
	if err := sess.Add(model.Message{Role: "user", Content: "late"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Add after unload: want ErrClosed, got %v", err)
	}
	if after := readTranscriptRaw(t, path); after != before {
		t.Fatal("transcript grew after session unload")
	}

	// --resume：同路径新 fiber 逐字恢复历史。
	fib2 := root.Load(Component(path))
	if err := fib2.Ready(ctx); err != nil {
		t.Fatalf("resume fiber: %v", err)
	}
	sess2, err := stc.Service[*Session](root, KeySession)
	if err != nil {
		t.Fatalf("resolve resumed session: %v", err)
	}
	want := []model.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "yo"},
	}
	if got := sess2.History(); !reflect.DeepEqual(got, want) {
		t.Fatalf("resumed history:\n got %+v\nwant %+v", got, want)
	}
}

// Contract/SessionEventLog（spec M5）：replay 投影 ≡ 内存终态；token 用量
// 事件在场；落盘的是类型化事件而非裸消息。
func TestSessionEventLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.jsonl")
	root := stc.New()
	defer root.Close()

	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()

	fib := root.Load(Component(path))
	if err := fib.Ready(ctx); err != nil {
		t.Fatalf("session fiber: %v", err)
	}
	sess, err := stc.Service[*Session](root, KeySession)
	if err != nil {
		t.Fatalf("resolve session: %v", err)
	}

	_ = sess.Add(model.Message{Role: "user", Content: "q"})
	_ = sess.AddUsage(model.Usage{Model: "m1", PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15})
	_ = sess.Add(model.Message{Role: "assistant", Content: "a"})

	// 落盘为类型化事件行：type 字段在场，消息嵌在载荷里。
	lines := readTranscriptLines(t, path)
	if len(lines) != 3 {
		t.Fatalf("transcript lines: %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], `"type":"message"`) || !strings.Contains(lines[1], `"type":"usage"`) {
		t.Fatalf("transcript is not a typed event log: %v", lines)
	}

	// replay ≡ 投影：撤掉原 fiber，新 fiber 的历史与事件日志和它的内存
	// 终态逐字一致。
	wantHist, wantEvents := sess.History(), sess.Events()
	fib.Dispose()
	if err := fib.Gone(ctx); err != nil {
		t.Fatalf("waiting session gone: %v", err)
	}
	fib2 := root.Load(Component(path))
	if err := fib2.Ready(ctx); err != nil {
		t.Fatalf("resume fiber: %v", err)
	}
	sess2, err := stc.Service[*Session](root, KeySession)
	if err != nil {
		t.Fatalf("resolve resumed session: %v", err)
	}
	if got := sess2.History(); !reflect.DeepEqual(got, wantHist) {
		t.Fatalf("replayed projection differs from in-memory:\n got %+v\nwant %+v", got, wantHist)
	}
	if got := sess2.Events(); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("replayed events differ from in-memory:\n got %+v\nwant %+v", got, wantEvents)
	}

	// 用量事件在 replay 后仍在场。
	var usages []model.Usage
	for _, ev := range sess2.Events() {
		if ev.Type == EventUsage {
			usages = append(usages, *ev.Usage)
		}
	}
	if len(usages) != 1 || usages[0].Model != "m1" || usages[0].TotalTokens != 15 {
		t.Fatalf("usage events after replay: %+v", usages)
	}
}

// v0.1 兼容：裸 message 行（无 type 字段）按消息事件 replay。
func TestReplayLegacyMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.jsonl")
	legacy := `{"role":"user","content":"old"}
{"role":"assistant","content":"format"}
`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	root := stc.New()
	defer root.Close()
	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()
	fib := root.Load(Component(path))
	if err := fib.Ready(ctx); err != nil {
		t.Fatalf("session fiber: %v", err)
	}
	sess, err := stc.Service[*Session](root, KeySession)
	if err != nil {
		t.Fatalf("resolve session: %v", err)
	}
	want := []model.Message{
		{Role: "user", Content: "old"},
		{Role: "assistant", Content: "format"},
	}
	if got := sess.History(); !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy replay:\n got %+v\nwant %+v", got, want)
	}
}

func TestReplayCorruptFails(t *testing.T) {
	for name, content := range map[string]string{
		"not json":           "not json\n",
		"unknown event type": `{"type":"approval"}` + "\n",
		"no type no role":    `{"foo":1}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "chat.jsonl")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			root := stc.New()
			defer root.Close()
			ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
			defer cancel()
			fib := root.Load(Component(path))
			if err := fib.Ready(ctx); err == nil || !strings.Contains(err.Error(), "corrupt transcript") {
				t.Fatalf("want corrupt transcript error, got %v", err)
			}
		})
	}
}

func readTranscriptRaw(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	return string(b)
}

func readTranscriptLines(t *testing.T, path string) []string {
	t.Helper()
	raw := strings.TrimSuffix(readTranscriptRaw(t, path), "\n")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}
