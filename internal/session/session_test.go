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

func TestReplayCorruptFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.jsonl")
	if err := os.WriteFile(path, []byte("not json\n"), 0o600); err != nil {
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
