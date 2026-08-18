package cli

import (
	stdctx "context"
	"fmt"
	"io"

	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
)

// ToolsCommandComponent 注册 /tools：列出当前注册的工具。
func ToolsCommandComponent() stc.Component {
	return stc.Component{
		Name:   "cmd:tools",
		Inject: []stc.Key{KeyCommands, tools.KeyTools},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			reg, err := stc.Service[*Registry](c, KeyCommands)
			if err != nil {
				return nil, err
			}
			ts, err := stc.Service[*tools.Toolset](c, tools.KeyTools)
			if err != nil {
				return nil, err
			}
			return reg.Register("tools", func(_ stdctx.Context, w io.Writer, _ string) error {
				list := ts.List()
				if len(list) == 0 {
					fmt.Fprintln(w, "no tools registered")
					return nil
				}
				for _, t := range list {
					fmt.Fprintf(w, "%s — %s\n", t.Name, t.Description)
				}
				return nil
			}), nil
		},
	}
}

// HelpCommandComponent 注册 /help：列出已注册命令。
func HelpCommandComponent() stc.Component {
	return stc.Component{
		Name:   "cmd:help",
		Inject: []stc.Key{KeyCommands},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			reg, err := stc.Service[*Registry](c, KeyCommands)
			if err != nil {
				return nil, err
			}
			return reg.Register("help", func(_ stdctx.Context, w io.Writer, _ string) error {
				fmt.Fprintln(w, "commands:")
				for _, n := range reg.Names() {
					fmt.Fprintf(w, "  /%s\n", n)
				}
				fmt.Fprintln(w, "  /quit (or plain quit/exit)")
				return nil
			}), nil
		},
	}
}
