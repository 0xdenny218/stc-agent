package tools_test

import (
	stdctx "context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xdenny218/stc-agent/internal/session"
	"github.com/0xdenny218/stc-agent/internal/tools"
)

func TestSpill(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spill")
	lookup := loadTools(t, tools.SpillComponent(dir))
	spill := lookup("spill")
	bg := stdctx.Background()

	t.Run("write file", func(t *testing.T) {
		out, err := spill.Invoke(bg, jsonArgs(t, map[string]any{"name": "notes.md", "content": "draft"}))
		if err != nil {
			t.Fatalf("spill: %v", err)
		}
		if !strings.Contains(out, "wrote 5 bytes") || !strings.Contains(out, dir) {
			t.Fatalf("spill output: %q", out)
		}
		b, err := os.ReadFile(filepath.Join(dir, "notes.md"))
		if err != nil {
			t.Fatalf("read spill file: %v", err)
		}
		if string(b) != "draft" {
			t.Fatalf("spill content: %q", b)
		}
	})

	t.Run("path traversal rejected", func(t *testing.T) {
		for _, name := range []string{"../evil.txt", "a/b.txt", "..", "."} {
			if _, err := spill.Invoke(bg, jsonArgs(t, map[string]any{"name": name, "content": "x"})); err == nil ||
				!strings.Contains(err.Error(), "invalid name") {
				t.Fatalf("name %q: expected invalid-name error, got %v", name, err)
			}
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil.txt")); !os.IsNotExist(err) {
			t.Fatal("traversal wrote outside spill dir")
		}
	})

	t.Run("bad args", func(t *testing.T) {
		if _, err := spill.Invoke(bg, jsonArgs(t, map[string]any{"content": "x"})); err == nil ||
			!strings.Contains(err.Error(), "name is required") {
			t.Fatalf("expected missing-name error, got %v", err)
		}
	})
}

func TestSessionTitle(t *testing.T) {
	lookup := loadTools(t, session.Component(""), tools.SessionTitleComponent())
	title := lookup("session_title")
	bg := stdctx.Background()

	t.Run("set title", func(t *testing.T) {
		out, err := title.Invoke(bg, jsonArgs(t, map[string]any{"title": "  fix login bug  "}))
		if err != nil {
			t.Fatalf("session_title: %v", err)
		}
		if !strings.Contains(out, "session title set to \"fix login bug\"") {
			t.Fatalf("session_title output: %q", out)
		}
	})

	t.Run("empty title rejected", func(t *testing.T) {
		if _, err := title.Invoke(bg, jsonArgs(t, map[string]any{"title": "   "})); err == nil ||
			!strings.Contains(err.Error(), "title is required") {
			t.Fatalf("expected empty-title error, got %v", err)
		}
	})

	t.Run("bad args", func(t *testing.T) {
		if _, err := title.Invoke(bg, jsonArgs(t, map[string]any{})); err == nil ||
			!strings.Contains(err.Error(), "title is required") {
			t.Fatalf("expected missing-title error, got %v", err)
		}
	})
}
