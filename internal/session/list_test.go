package session_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xdenny218/stc-agent/internal/session"
)

func TestListSessions(t *testing.T) {
	dir := t.TempDir()
	mk := func(name, content string, age time.Duration) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		n := time.Now().Add(-age)
		if err := os.Chtimes(p, n, n); err != nil {
			t.Fatal(err)
		}
	}

	mk("old.jsonl",
		`{"type":"message","message":{"role":"user","content":"first question here"}}`+"\n", 2*time.Hour)
	mk("new.jsonl",
		`{"type":"message","message":{"role":"user","content":"hello\nsecond line"}}`+"\n"+
			`{"type":"title","title":"titled session"}`+"\n"+
			`{"type":"title","title":"newer title wins"}`+"\n", time.Hour)
	// 非 jsonl 与子目录不算会话。
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := session.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 sessions, got %d: %+v", len(got), got)
	}
	if filepath.Base(got[0].Path) != "new.jsonl" || filepath.Base(got[1].Path) != "old.jsonl" {
		t.Fatalf("order by mtime desc: %+v", got)
	}
	if got[0].Display() != "newer title wins" {
		t.Fatalf("title last-wins: %q", got[0].Display())
	}
	if got[1].Display() != "first question here" {
		t.Fatalf("first-user fallback: %q", got[1].Display())
	}
}

func TestListSessionsEmpty(t *testing.T) {
	for _, dir := range []string{t.TempDir(), filepath.Join(t.TempDir(), "missing")} {
		got, err := session.List(dir)
		if err != nil || len(got) != 0 {
			t.Fatalf("empty/missing dir: %+v, %v", got, err)
		}
	}
}

func TestListSessionsToleratesCorrupt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.jsonl"), []byte("not json\n{\"type\":\"title\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := session.List(dir)
	if err != nil {
		t.Fatalf("corrupt lines must not fail the listing: %v", err)
	}
	if len(got) != 1 || got[0].Display() != "bad.jsonl" {
		t.Fatalf("corrupt session falls back to filename: %+v", got)
	}
}
