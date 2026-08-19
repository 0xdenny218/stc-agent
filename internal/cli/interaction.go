package cli

import (
	stdctx "context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/0xdenny218/stc-agent/internal/interaction"
	"github.com/peterh/liner"
)

// terminalInteraction 是 interaction 的终端 provider（spec D18），与
// Console 共享输入通道：TTY 走 liner（轮次期间 pumpTTY 停在 gate 上，
// 不会并发 Prompt）；管道读 console.lines（轮次期间 serve 阻塞在
// RunTurn，lines 无人竞争）。
type terminalInteraction struct{ c *Console }

// TerminalInteraction 返回绑定 console 的提问服务。
func TerminalInteraction(c *Console) interaction.Service {
	return terminalInteraction{c}
}

// Ask 渲染问题并挂起等回答。空回答与 EOF 取 q.Default（审批门应把默认
// 设为拒绝向）；TTY 处 Ctrl-C 由 liner 消化为 ErrPromptAborted，映射为
// interaction.ErrAborted（轮次中断，不是拒绝）；无效回答重新提问。
func (ti terminalInteraction) Ask(ctx stdctx.Context, q interaction.Question) (string, error) {
	w := ti.c.out
	fmt.Fprintf(w, "\n! %s\n", q.Title)
	if q.Detail != "" {
		// 逐行缩进两格：diff 预览是多行的，缩进对齐问题标题。
		for _, line := range strings.Split(q.Detail, "\n") {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}
	labels := make([]string, len(q.Options))
	for i, o := range q.Options {
		labels[i] = "[" + o.Key + "] " + o.Label
	}
	fmt.Fprintf(w, "  %s\n", strings.Join(labels, "  "))
	for {
		ans, err := ti.prompt(ctx, "? ")
		switch {
		case errors.Is(err, liner.ErrPromptAborted):
			return "", interaction.ErrAborted
		case errors.Is(err, io.EOF):
			return q.Default, nil
		case err != nil:
			return "", err
		}
		ans = strings.ToLower(strings.TrimSpace(ans))
		if ans == "" {
			return q.Default, nil
		}
		for _, o := range q.Options {
			if ans == o.Key {
				return ans, nil
			}
		}
		keys := make([]string, len(q.Options))
		for i, o := range q.Options {
			keys[i] = `"` + o.Key + `"`
		}
		fmt.Fprintf(w, "  please answer %s\n", strings.Join(keys, "/"))
	}
}

func (ti terminalInteraction) prompt(ctx stdctx.Context, p string) (string, error) {
	if ti.c.tty {
		return ti.c.lnr.Prompt(p)
	}
	select {
	case line, ok := <-ti.c.lines:
		if !ok {
			return "", io.EOF
		}
		return line, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
