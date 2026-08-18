package tools_test

import (
	stdctx "context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
)

// loadTools 装载 toolset + 指定工具，返回按名查找器。
func loadTools(t *testing.T, comps ...stc.Component) func(string) tools.Tool {
	t.Helper()
	root := stc.New()
	t.Cleanup(func() { root.Close() })
	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()
	for _, c := range append([]stc.Component{tools.ToolsetComponent()}, comps...) {
		f := root.Load(c)
		if err := f.Ready(ctx); err != nil {
			t.Fatalf("fiber %s: %v", f.Name(), err)
		}
	}
	ts, err := stc.Service[*tools.Toolset](root, tools.KeyTools)
	if err != nil {
		t.Fatalf("resolve toolset: %v", err)
	}
	return func(name string) tools.Tool {
		t.Helper()
		tool, ok := ts.Lookup(name)
		if !ok {
			t.Fatalf("tool %q not registered", name)
		}
		return tool
	}
}

func jsonArgs(t *testing.T, kv map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(kv)
	if err != nil {
		t.Fatal(err)
	}
	return json.RawMessage(b)
}

func TestEdit(t *testing.T) {
	lookup := loadTools(t, tools.EditComponent())
	edit := lookup("edit")
	dir := t.TempDir()
	p := filepath.Join(dir, "doc.txt")
	if err := os.WriteFile(p, []byte("hello world\nhello again\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bg := stdctx.Background()

	t.Run("unique replace", func(t *testing.T) {
		out, err := edit.Invoke(bg, jsonArgs(t, map[string]any{"path": p, "old_string": "world", "new_string": "mars"}))
		if err != nil {
			t.Fatalf("edit: %v", err)
		}
		if !strings.Contains(out, "replaced 1 occurrence") {
			t.Fatalf("edit output: %q", out)
		}
		b, _ := os.ReadFile(p)
		if !strings.Contains(string(b), "hello mars") || strings.Contains(string(b), "world") {
			t.Fatalf("content after edit: %q", b)
		}
	})

	t.Run("multiple without replace_all errors", func(t *testing.T) {
		if err := os.WriteFile(p, []byte("foo\nfoo\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := edit.Invoke(bg, jsonArgs(t, map[string]any{"path": p, "old_string": "foo", "new_string": "bar"})); err == nil ||
			!strings.Contains(err.Error(), "appears 2 times") {
			t.Fatalf("expected ambiguity error, got %v", err)
		}
	})

	t.Run("replace_all", func(t *testing.T) {
		out, err := edit.Invoke(bg, jsonArgs(t, map[string]any{"path": p, "old_string": "foo", "new_string": "bar", "replace_all": true}))
		if err != nil {
			t.Fatalf("edit replace_all: %v", err)
		}
		if !strings.Contains(out, "replaced 2 occurrence") {
			t.Fatalf("edit output: %q", out)
		}
		b, _ := os.ReadFile(p)
		if string(b) != "bar\nbar\n" {
			t.Fatalf("content after replace_all: %q", b)
		}
	})

	t.Run("not found", func(t *testing.T) {
		if _, err := edit.Invoke(bg, jsonArgs(t, map[string]any{"path": p, "old_string": "nope", "new_string": "x"})); err == nil ||
			!strings.Contains(err.Error(), "old_string not found") {
			t.Fatalf("expected not-found error, got %v", err)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		if _, err := edit.Invoke(bg, jsonArgs(t, map[string]any{"path": filepath.Join(dir, "nope"), "old_string": "a", "new_string": "b"})); err == nil {
			t.Fatal("expected error for missing file")
		}
	})

	t.Run("bad args", func(t *testing.T) {
		if _, err := edit.Invoke(bg, jsonArgs(t, map[string]any{"path": p, "new_string": "b"})); err == nil ||
			!strings.Contains(err.Error(), "old_string are required") {
			t.Fatalf("expected missing-old error, got %v", err)
		}
	})
}

func TestGlob(t *testing.T) {
	lookup := loadTools(t, tools.GlobComponent())
	glob := lookup("glob")
	root := t.TempDir()
	files := []string{
		"a.go",
		"sub/b.go",
		"sub/deep/c.go",
		"sub/deep/d.txt",
		"e.txt",
	}
	for _, rel := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bg := stdctx.Background()

	rel := func(lines string) map[string]bool {
		t.Helper()
		out := map[string]bool{}
		for _, l := range strings.Split(lines, "\n") {
			if l != "" {
				r, err := filepath.Rel(root, l)
				if err != nil {
					t.Fatal(err)
				}
				out[r] = true
			}
		}
		return out
	}
	invoke := func(pattern string) map[string]bool {
		t.Helper()
		out, err := glob.Invoke(bg, jsonArgs(t, map[string]any{"pattern": pattern, "root": root}))
		if err != nil {
			t.Fatalf("glob %q: %v", pattern, err)
		}
		return rel(out)
	}

	t.Run("** recursion", func(t *testing.T) {
		got := invoke("**/*.go")
		want := map[string]bool{"a.go": true, "sub/b.go": true, "sub/deep/c.go": true}
		if !mapEq(got, want) {
			t.Fatalf("glob **/*.go: got %v, want %v", got, want)
		}
	})

	t.Run("single star no slash", func(t *testing.T) {
		got := invoke("*.go")
		want := map[string]bool{"a.go": true}
		if !mapEq(got, want) {
			t.Fatalf("glob *.go: got %v, want %v", got, want)
		}
	})

	t.Run("no matches", func(t *testing.T) {
		if out, err := glob.Invoke(bg, jsonArgs(t, map[string]any{"pattern": "*.rs", "root": root})); err != nil || out != "(no matches)" {
			t.Fatalf("glob no-match: %q, %v", out, err)
		}
	})

	t.Run("bad args", func(t *testing.T) {
		if _, err := glob.Invoke(bg, jsonArgs(t, map[string]any{"root": root})); err == nil ||
			!strings.Contains(err.Error(), "pattern is required") {
			t.Fatalf("expected missing-pattern error, got %v", err)
		}
	})
}

func TestGrep(t *testing.T) {
	lookup := loadTools(t, tools.GrepComponent())
	grep := lookup("grep")
	root := t.TempDir()
	// 一个正常文本文件 + 一个二进制文件（内容含 NUL）。
	a := filepath.Join(root, "a.txt")
	if err := os.WriteFile(a, []byte("alpha beta\ngamma delta\nalpha gamma\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "blob.bin")
	if err := os.WriteFile(bin, []byte("alpha\x00beta\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "sub", "b.txt")
	if err := os.MkdirAll(filepath.Dir(sub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sub, []byte("only beta here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bg := stdctx.Background()

	t.Run("single file line numbers", func(t *testing.T) {
		out, err := grep.Invoke(bg, jsonArgs(t, map[string]any{"pattern": "alpha", "path": a}))
		if err != nil {
			t.Fatalf("grep: %v", err)
		}
		if !strings.Contains(out, "a.txt:1: alpha beta") || !strings.Contains(out, "a.txt:3: alpha gamma") ||
			strings.Contains(out, "gamma delta") {
			t.Fatalf("grep output: %q", out)
		}
	})

	t.Run("recursive skips binary", func(t *testing.T) {
		out, err := grep.Invoke(bg, jsonArgs(t, map[string]any{"pattern": "beta", "path": root, "recursive": true}))
		if err != nil {
			t.Fatalf("grep recursive: %v", err)
		}
		if !strings.Contains(out, "a.txt:1: alpha beta") || !strings.Contains(out, "b.txt:1: only beta here") ||
			strings.Contains(out, "blob.bin") {
			t.Fatalf("grep recursive output: %q", out)
		}
	})

	t.Run("dir without recursive errors", func(t *testing.T) {
		if _, err := grep.Invoke(bg, jsonArgs(t, map[string]any{"pattern": "alpha", "path": root})); err == nil ||
			!strings.Contains(err.Error(), "recursive=true") {
			t.Fatalf("expected dir-no-recursive error, got %v", err)
		}
	})

	t.Run("no matches", func(t *testing.T) {
		if out, err := grep.Invoke(bg, jsonArgs(t, map[string]any{"pattern": "zzz", "path": a})); err != nil || out != "(no matches)" {
			t.Fatalf("grep no-match: %q, %v", out, err)
		}
	})

	t.Run("bad pattern", func(t *testing.T) {
		if _, err := grep.Invoke(bg, jsonArgs(t, map[string]any{"pattern": "(", "path": a})); err == nil ||
			!strings.Contains(err.Error(), "bad pattern") {
			t.Fatalf("expected bad-pattern error, got %v", err)
		}
	})

	t.Run("bad args", func(t *testing.T) {
		if _, err := grep.Invoke(bg, jsonArgs(t, map[string]any{"pattern": "x"})); err == nil ||
			!strings.Contains(err.Error(), "path are required") {
			t.Fatalf("expected missing-path error, got %v", err)
		}
	})
}

func mapEq(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
