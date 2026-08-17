package cli

import (
	stdctx "context"
	"fmt"
	"io"
	"strings"

	"github.com/0xdenny218/stc-agent/internal/config"
	stc "github.com/0xdenny218/stc-go"
)

// ModelCommandComponent 注册 /model <name>：换模型 = 重提供 config →
// 级联重载（spec D4）。注册本身是效应，fiber 卸载即注销。
func ModelCommandComponent() stc.Component {
	return stc.Component{
		Name:   "cmd:model",
		Inject: []stc.Key{KeyCommands, config.KeyConfigCtl},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			reg, err := stc.Service[*Registry](c, KeyCommands)
			if err != nil {
				return nil, err
			}
			ctl, err := stc.Service[*config.Control](c, config.KeyConfigCtl)
			if err != nil {
				return nil, err
			}
			return reg.Register("model", func(ctx stdctx.Context, w io.Writer, args string) error {
				name := strings.TrimSpace(args)
				if name == "" {
					fmt.Fprintln(w, "usage: /model <name>")
					return nil
				}
				if err := ctl.SetModel(ctx, name); err != nil {
					return err
				}
				fmt.Fprintf(w, "model switched to %s\n", name)
				return nil
			}), nil
		},
	}
}
