// Package testutil 是测试共用的辅助：定位 TinyGo、构建示例 guest。
// guest 与宿主共用模块上下文（无独立 go.mod），stc-go 版本自动锁定。
package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TinygoPath 定位 tinygo 可执行文件：$TINYGO → PATH → ~/.local/opt/tinygo。
// 都找不到则 Skip（CI 上 TinyGo 由 workflow 安装并进 PATH）。
func TinygoPath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("TINYGO"); p != "" {
		return p
	}
	if p, err := exec.LookPath("tinygo"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".local", "opt", "tinygo", "bin", "tinygo")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("tinygo not found (set TINYGO, put it in PATH, or install to ~/.local/opt/tinygo)")
	return ""
}

// RepoRoot 返回仓库根目录（由本文件位置推导，与测试运行目录无关）。
func RepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// BuildGuest 用 TinyGo 把 srcDir（仓库相对路径）构建到 out 并返回 out。
// tags 传给 -tags（如 "v2" 构建热替换演示的下一版）。
func BuildGuest(t *testing.T, srcDir, out string, tags ...string) string {
	t.Helper()
	args := []string{"build", "-target", "wasip1", "-buildmode=c-shared", "-o", out}
	if len(tags) > 0 {
		args = append(args, "-tags", strings.Join(tags, ","))
	}
	args = append(args, ".")
	cmd := exec.Command(TinygoPath(t), args...)
	cmd.Dir = filepath.Join(RepoRoot(t), srcDir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tinygo build %s: %v\n%s", srcDir, err, output)
	}
	return out
}
