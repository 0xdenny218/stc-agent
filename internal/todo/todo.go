// Package todo implements the todo capability (spec M9): a todo_write tool
// that rewrites the full task-list snapshot as a session event, plus a
// system-prompt segment ("20-todos") that always renders the latest
// snapshot. The event log is the source of truth — the segment is rebuilt
// from the session projection on fiber load, so a replayed transcript
// restores the task list into the prompt automatically.
package todo

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/0xdenny218/stc-agent/internal/prompt"
	"github.com/0xdenny218/stc-agent/internal/session"
	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
)

// segmentName 是任务列表段落在 system prompt 里的名字（"20-" 排在身份
// 段之后）。
const segmentName = "20-todos"

// schema 是 todo_write 工具的参数 JSON Schema：全量快照，非增量。
var schema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "todos": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "content": {"type": "string"},
          "status": {"type": "string", "enum": ["pending", "in_progress", "completed"]}
        },
        "required": ["content", "status"]
      }
    }
  },
  "required": ["todos"]
}`)

// state 持有当前段落注销逆；Register 的逆幂等且 ABA 安全，重复调用
// 或被同名覆盖后调用都无害。
type state struct {
	mu  sync.Mutex
	inv stc.Inverse // nil = 段落当前不在册
}

// sync 把段落渲染同步到 todos：空列表 = 摘除段落（空段不该占位），
// 非空 = （重）注册。返回注销当前段落的逆（供 fiber 卸载用）。
func (s *state) sync(reg *prompt.Segments, todos []session.Todo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inv != nil {
		_ = s.inv() // 幂等：已被覆盖时是 no-op
		s.inv = nil
	}
	if text := render(todos); text != "" {
		s.inv = reg.Register(segmentName, text)
	}
}

// render 把快照渲染成段落文本；空列表渲染为空串。
func render(todos []session.Todo) string {
	if len(todos) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Task list (current snapshot; update it with todo_write)\n")
	for _, t := range todos {
		fmt.Fprintf(&b, "- [%s] %s\n", t.Status, t.Content)
	}
	return b.String()
}

// Component 是 todo fiber：注入 session/prompt/工具表（全稳定键，不随
// 模型级联重载）。装载时从会话投影重建段落（replay 恢复），todo_write
// 每次重写快照并重注册段落。
func Component() stc.Component {
	return stc.Component{
		Name:   "todo",
		Inject: []stc.Key{session.KeySession, prompt.KeyPrompt, tools.KeyTools},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			sess, err := stc.Service[*session.Session](c, session.KeySession)
			if err != nil {
				return nil, err
			}
			reg, err := stc.Service[*prompt.Segments](c, prompt.KeyPrompt)
			if err != nil {
				return nil, err
			}
			ts, err := stc.Service[*tools.Toolset](c, tools.KeyTools)
			if err != nil {
				return nil, err
			}
			st := &state{}
			st.sync(reg, sess.Todos()) // replay 恢复：投影即最新快照
			untool := ts.Register("todo_write", tools.Tool{
				Name: "todo_write",
				Description: "Rewrite the task list. Always send the complete snapshot (not a delta); an empty " +
					"list clears it. Use it to track multi-step work and keep statuses current.",
				Parameters: schema,
				Invoke: func(_ stdctx.Context, args json.RawMessage) (string, error) {
					return invoke(sess, reg, st, args)
				},
			})
			return func() error {
				_ = untool()
				st.sync(reg, nil)
				return nil
			}, nil
		},
	}
}

// invoke 是 todo_write 的执行体：校验 → 快照入会话日志 → 段落同步。
func invoke(sess *session.Session, reg *prompt.Segments, st *state, args json.RawMessage) (string, error) {
	var a struct {
		Todos []session.Todo `json:"todos"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("todo_write: bad arguments: %w", err)
	}
	for i, t := range a.Todos {
		if strings.TrimSpace(t.Content) == "" {
			return "", fmt.Errorf("todo_write: todos[%d]: content is required", i)
		}
		switch t.Status {
		case "pending", "in_progress", "completed":
		default:
			return "", fmt.Errorf("todo_write: todos[%d]: invalid status %q (want pending | in_progress | completed)", i, t.Status)
		}
	}
	if err := sess.AddTodos(a.Todos); err != nil {
		return "", err
	}
	st.sync(reg, a.Todos)
	if len(a.Todos) == 0 {
		return "todo list cleared", nil
	}
	return fmt.Sprintf("todo list updated: %d items", len(a.Todos)), nil
}
