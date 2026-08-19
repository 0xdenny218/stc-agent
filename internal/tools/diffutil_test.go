package tools

import (
	"strings"
	"testing"
)

func TestUnifiedDiff(t *testing.T) {
	for _, tc := range []struct {
		name      string
		old, new  string
		wantLines []string // 依序包含
		banLines  []string // 不得包含
		empty     bool
	}{
		{"identical", "a\nb\n", "a\nb\n", nil, nil, true},
		{"insertion", "a\nc\n", "a\nb\nc\n", []string{"+b"}, nil, false},
		{"deletion", "a\nb\nc\n", "a\nc\n", []string{"-b"}, nil, false},
		{"replacement", "a\nx\nc\n", "a\ny\nc\n", []string{"-x", "+y"}, nil, false},
		{"new file", "", "hello\nworld\n", []string{"+hello", "+world"}, nil, false},
		{"crlf normalized", "a\r\nb\r\n", "a\nb\n", nil, nil, true},
		{"trailing newline only", "a", "a\n", nil, nil, true},
	} {
		got := unifiedDiff("f", tc.old, tc.new)
		if tc.empty {
			if got != "" {
				t.Errorf("%s: want empty, got %q", tc.name, got)
			}
			continue
		}
		if got == "" {
			t.Errorf("%s: unexpected empty diff", tc.name)
			continue
		}
		for _, w := range tc.wantLines {
			if !strings.Contains(got, w) {
				t.Errorf("%s: missing %q in:\n%s", tc.name, w, got)
			}
		}
		for _, b := range tc.banLines {
			if strings.Contains(got, b) {
				t.Errorf("%s: must not contain %q in:\n%s", tc.name, b, got)
			}
		}
		if !strings.Contains(got, "@@") {
			t.Errorf("%s: missing hunk header in:\n%s", tc.name, got)
		}
	}
}

func TestUnifiedDiffTruncates(t *testing.T) {
	var b strings.Builder
	for i := 0; i < previewMaxLines*2; i++ {
		b.WriteString("line\n")
	}
	got := unifiedDiff("f", "", b.String())
	if !strings.Contains(got, "... (diff truncated)") {
		t.Fatalf("expected truncation marker, got %d bytes", len(got))
	}
}

func TestUnifiedDiffHugeFallsBack(t *testing.T) {
	// 行数乘积远超 LCS 上限：走"全删全加"退化路径，仍出合法 diff。
	mk := func(n int, line string) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString(line + "\n")
		}
		return b.String()
	}
	got := unifiedDiff("f", mk(2000, "x"), mk(2000, "y"))
	if got == "" || !strings.Contains(got, "@@") {
		t.Fatal("fallback path must still emit a diff")
	}
	// 4000 行改动在 200 行预览上限内截断：只见得到 -x，但必须是合法的
	// 统一 diff 且带截断标记。
	if !strings.Contains(got, "-x") || !strings.Contains(got, "... (diff truncated)") {
		t.Fatalf("fallback diff should show -x and truncate; got %.80s", got)
	}
}
