package tools_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
)

// preview 取工具并调 Preview（参数经 jsonArgs 组装）。
func preview(t *testing.T, comp stc.Component, name string, kv map[string]any) string {
	t.Helper()
	lookup := loadTools(t, comp)
	tool := lookup(name)
	if tool.Preview == nil {
		t.Fatalf("tool %q has no Preview", name)
	}
	return tool.Preview(jsonArgs(t, kv))
}

func TestWriteFilePreview(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")

	got := preview(t, tools.WriteFileComponent(), "write_file", map[string]any{"path": p, "content": "hello"})
	if !strings.Contains(got, "(new file)") || !strings.Contains(got, "+hello") {
		t.Fatalf("new-file preview: %q", got)
	}

	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	got = preview(t, tools.WriteFileComponent(), "write_file", map[string]any{"path": p, "content": "goodbye"})
	if !strings.Contains(got, "-hello") || !strings.Contains(got, "+goodbye") {
		t.Fatalf("overwrite preview: %q", got)
	}
}

func TestEditPreview(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := preview(t, tools.EditComponent(), "edit",
		map[string]any{"path": p, "old_string": "beta", "new_string": "gamma"})
	if !strings.Contains(got, "-beta") || !strings.Contains(got, "+gamma") {
		t.Fatalf("edit preview: %q", got)
	}
	// 未命中：预览直接亮出将要报的错（用户在 y/n 时就能看到问题）。
	got = preview(t, tools.EditComponent(), "edit",
		map[string]any{"path": p, "old_string": "nope", "new_string": "x"})
	if !strings.Contains(got, "not found") {
		t.Fatalf("miss preview: %q", got)
	}
}

func TestSpillPreview(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spill")
	got := preview(t, tools.SpillComponent(dir), "spill", map[string]any{"name": "notes.md", "content": "draft"})
	if !strings.Contains(got, "(new file)") || !strings.Contains(got, "+draft") {
		t.Fatalf("spill preview: %q", got)
	}
}
