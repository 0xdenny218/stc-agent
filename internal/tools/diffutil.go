package tools

import (
	"fmt"
	"strings"
)

// 本文件是写类工具（write_file/edit/spill）审批预览的统一 diff 引擎：
// LCS 行级对比 → 3 行上下文的 hunk。刻意不引第三方依赖；规模护栏——
// 行数乘积超限时退化为整段替换表示，输出行数超限截断（预览是给人看的，
// 不是给机器合并用的）。

const (
	previewMaxLines       = 200 // 预览最多展示的 diff 行数
	lcsCellLimit    int64 = 4e5 // DP 表格上限，超过走退化路径
)

// unifiedDiff 生成 old→new 的统一 diff；内容一致时返回空串。
func unifiedDiff(label, old, new string) string {
	if old == new {
		return ""
	}
	script := diffScript(splitLines(old), splitLines(new))
	var b strings.Builder
	fmt.Fprintf(&b, "--- %s\n+++ %s\n", label, label)
	lines := 2
	changed := false
	for _, h := range hunks(script, 3) {
		changed = true
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", h.oldStart, h.oldCount, h.newStart, h.newCount)
		lines++
		for _, op := range h.ops {
			if lines >= previewMaxLines {
				b.WriteString("... (diff truncated)\n")
				return b.String()
			}
			b.WriteString(string(op.kind) + op.line + "\n")
			lines++
		}
	}
	if !changed {
		return "" // 只有行尾差异等 split 后同形的极端情形
	}
	return b.String()
}

// splitLines 按行切分；结尾换行产生的空尾行不计（每行末尾隐含 \n）。
func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// diffOp 是编辑脚本的一步：'=' 保留、'-' 删旧、'+' 增新。
type diffOp struct {
	kind byte
	line string
}

// diffScript 用 LCS 生成编辑脚本。大输入（乘积超限）退化为"全删全加"
// ——正确性不受影响，只是失去最小性。
func diffScript(a, b []string) []diffOp {
	n, m := len(a), len(b)
	if int64(n)*int64(m) > lcsCellLimit {
		out := make([]diffOp, 0, n+m)
		for _, l := range a {
			out = append(out, diffOp{'-', l})
		}
		for _, l := range b {
			out = append(out, diffOp{'+', l})
		}
		return out
	}
	// dp[i][j] = a[i:] 与 b[j:] 的 LCS 长度（从尾部倒推）。
	dp := make([][]int32, n+1)
	for i := range dp {
		dp[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				dp[i][j] = dp[i+1][j+1] + 1
			case dp[i+1][j] >= dp[i][j+1]:
				dp[i][j] = dp[i+1][j]
			default:
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	out := make([]diffOp, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, diffOp{'=', a[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			out = append(out, diffOp{'-', a[i]})
			i++
		default:
			out = append(out, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		out = append(out, diffOp{'+', b[j]})
	}
	return out
}

// hunk 是一个带上下文的改动块；行号 1 起算。
type hunk struct {
	oldStart, oldCount int
	newStart, newCount int
	ops                []diffOp
}

// hunks 把编辑脚本按"间隔 ≤ 2×ctx 的改动归并"切成分组，每组前后各带
// ctx 行上下文。
func hunks(script []diffOp, ctx int) []hunk {
	var out []hunk
	i := 0
	for i < len(script) {
		if script[i].kind == '=' {
			i++
			continue
		}
		// 找到一组改动：向后扩展直到两个改动之间的 '=' 数 > 2*ctx。
		start, end := i, i
		gap := 0
		for j := i + 1; j < len(script); j++ {
			if script[j].kind != '=' {
				end = j
				gap = 0
				continue
			}
			gap++
			if gap > 2*ctx {
				break
			}
		}
		lo := start - ctx
		if lo < 0 {
			lo = 0
		}
		hi := end + ctx + 1
		if hi > len(script) {
			hi = len(script)
		}
		h := hunk{ops: script[lo:hi], oldStart: 1, newStart: 1}
		// 行号：块前的 '=' 数决定起始行；块内分别数旧/新行数。
		for k := 0; k < lo; k++ {
			if script[k].kind != '+' {
				h.oldStart++
			}
			if script[k].kind != '-' {
				h.newStart++
			}
		}
		for _, op := range h.ops {
			if op.kind != '+' {
				h.oldCount++
			}
			if op.kind != '-' {
				h.newCount++
			}
		}
		out = append(out, h)
		i = hi
	}
	return out
}
