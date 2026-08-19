package instructions_test

import (
	stdctx "context"
	"os"
	"path/filepath"

	"testing"
	"time"

	"github.com/0xdenny218/stc-agent/internal/instructions"
	"github.com/0xdenny218/stc-agent/internal/prompt"
	stc "github.com/0xdenny218/stc-go"
)

// load 装载段落注册表 + instructions fiber；每个用例独立目录，状态互不
// 泄漏。返回段落表视图与目录路径。
func load(t *testing.T) (*prompt.Segments, string) {
	t.Helper()
	dir := t.TempDir()
	root := stc.New()
	t.Cleanup(func() { root.Close() })
	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()
	for _, c := range []stc.Component{prompt.Component(), instructions.Component(dir)} {
		if err := root.Load(c).Ready(ctx); err != nil {
			t.Fatalf("fiber: %v", err)
		}
	}
	segs, err := stc.Service[*prompt.Segments](root, prompt.KeyPrompt)
	if err != nil {
		t.Fatal(err)
	}
	return segs, dir
}

// md 写/删 dir 下的 AGENTS.md。
func md(t *testing.T, dir string, content string) {
	t.Helper()
	if content == "" {
		if err := os.Remove(filepath.Join(dir, "AGENTS.md")); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// waitSegment 等段落表收敛到 want（boot 读取是同步的，热更新经防抖）。
func waitSegment(t *testing.T, segs *prompt.Segments, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if prompt.Assemble(segs) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("segment not synced; got %q want %q", prompt.Assemble(segs), want)
}

// loadAt 对指定目录装载（present-at-boot 用：先写文件再装 fiber）。
func loadAt(t *testing.T, dir string) *prompt.Segments {
	t.Helper()
	root := stc.New()
	t.Cleanup(func() { root.Close() })
	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()
	for _, c := range []stc.Component{prompt.Component(), instructions.Component(dir)} {
		if err := root.Load(c).Ready(ctx); err != nil {
			t.Fatalf("fiber: %v", err)
		}
	}
	segs, err := stc.Service[*prompt.Segments](root, prompt.KeyPrompt)
	if err != nil {
		t.Fatal(err)
	}
	return segs
}

func TestInstructionsSegment(t *testing.T) {
	t.Run("present at boot", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Use Go 1.24."), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := prompt.Assemble(loadAt(t, dir)); got != "Use Go 1.24." {
			t.Fatalf("segment: %q", got)
		}
	})

	t.Run("absent at boot", func(t *testing.T) {
		segs, _ := load(t)
		if got := prompt.Assemble(segs); got != "" {
			t.Fatalf("no AGENTS.md, no segment; got %q", got)
		}
	})

	t.Run("edit hot-updates", func(t *testing.T) {
		segs, dir := load(t)
		md(t, dir, "v1")
		waitSegment(t, segs, "v1")
		md(t, dir, "v2")
		waitSegment(t, segs, "v2")
	})

	t.Run("delete retracts", func(t *testing.T) {
		segs, dir := load(t)
		md(t, dir, "v2")
		waitSegment(t, segs, "v2")
		md(t, dir, "")
		waitSegment(t, segs, "")
	})

	t.Run("appear mid-session", func(t *testing.T) {
		segs, dir := load(t)
		waitSegment(t, segs, "")
		md(t, dir, "late")
		waitSegment(t, segs, "late")
	})

	t.Run("blank file is no segment", func(t *testing.T) {
		segs, dir := load(t)
		md(t, dir, "   \n")
		waitSegment(t, segs, "")
	})
}
