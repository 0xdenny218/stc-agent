package tools

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/0xdenny218/stc-agent/internal/session"
	stc "github.com/0xdenny218/stc-go"
)

// SessionTitleComponent 会话标题工具：把标题写进 session 事件日志（唯一
// 真源，replay 恢复）。与 todo 同族——依赖 session，故不套 component()
// 骨架而直接写 Apply（session 是稳定键，不随模型级联重载）。
func SessionTitleComponent() stc.Component {
	return stc.Component{
		Name:   "tool:session_title",
		Inject: []stc.Key{KeyTools, session.KeySession},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			sess, err := stc.Service[*session.Session](c, session.KeySession)
			if err != nil {
				return nil, err
			}
			ts, err := stc.Service[*Toolset](c, KeyTools)
			if err != nil {
				return nil, err
			}
			return ts.Register("session_title", Tool{
				Name:        "session_title",
				Description: "Set the session title — a short label for this whole session (e.g. \"fix login bug\").",
				Parameters: json.RawMessage(`{"type":"object","properties":{
  "title": {"type":"string","description":"new session title"}
},"required":["title"]}`),
				Invoke: func(_ stdctx.Context, args json.RawMessage) (string, error) {
					var a struct {
						Title string `json:"title"`
					}
					if err := decodeArgs(args, &a); err != nil {
						return "", err
					}
					title := strings.TrimSpace(a.Title)
					if title == "" {
						return "", errors.New("invalid arguments: title is required")
					}
					if err := sess.AddTitle(title); err != nil {
						return "", err
					}
					return fmt.Sprintf("session title set to %q", title), nil
				},
			}), nil
		},
	}
}
