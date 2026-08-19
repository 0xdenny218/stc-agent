package cli

import (
	stdctx "context"
	"fmt"
	"io"

	"github.com/0xdenny218/stc-agent/internal/session"
	stc "github.com/0xdenny218/stc-go"
)

// ResumeCommandComponent 注册 /resume：列出历史会话（带标题）并给出
// 恢复方式。会话切换不支持进程内热换（session fiber 是稳定键、resume
// 是装载期重放），故以重启命令的形式给出——与 Claude Code /resume 的
// 差异是刻意最小化。list 由装配方注入（读默认会话目录）。
func ResumeCommandComponent(list func() []session.Info) stc.Component {
	return stc.Component{
		Name:   "cmd:resume",
		Inject: []stc.Key{KeyCommands},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			reg, err := stc.Service[*Registry](c, KeyCommands)
			if err != nil {
				return nil, err
			}
			return reg.Register("resume", func(_ stdctx.Context, w io.Writer, _ string) error {
				infos := list()
				if len(infos) == 0 {
					fmt.Fprintln(w, "no past sessions found")
					return nil
				}
				fmt.Fprintf(w, "sessions (newest first):\n")
				for i, info := range infos {
					fmt.Fprintf(w, "  %d. %s  %s  %s\n", i+1, info.ModTime.Format("2006-01-02 15:04"),
						info.Display(), info.Path)
				}
				fmt.Fprintln(w, "resume one with: stc-agent --resume <path>")
				return nil
			}), nil
		},
	}
}
