package hooks

import (
	"fmt"
	"io"

	stc "github.com/0xdenny218/stc-go"
)

// BellComponent 是轮次结束响铃（spec M11 小件）：监听 agent/turn-end，
// 每轮结束（含错误轮）向终端写一声 BEL（\a）。--bell 开关装配。
func BellComponent(w io.Writer) stc.Component {
	return stc.Component{
		Name: "bell",
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			if err := Listen(c, TurnEnd, func(Payload) {
				fmt.Fprint(w, "\a")
			}); err != nil {
				return nil, err
			}
			return nil, nil
		},
	}
}
