// Package plan implements plan mode (spec M9): /plan toggles a mode in
// which a tools/pre-execute interceptor bails every non-read-only tool, and
// exit_plan_mode presents the plan to the user via the interaction service
// — approval turns the mode off and feeds the plan back as the tool result,
// rejection keeps the mode on. The decision is logged as a session approval
// event (same audit family as the M6 approval gate).
package plan

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/0xdenny218/stc-agent/internal/cli"
	"github.com/0xdenny218/stc-agent/internal/hooks"
	"github.com/0xdenny218/stc-agent/internal/interaction"
	"github.com/0xdenny218/stc-agent/internal/prompt"
	"github.com/0xdenny218/stc-agent/internal/session"
	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
)

// KeyMode 是 plan 模式状态服务（/plan 命令、拦截器与工具共享）。
var KeyMode = stc.NewKey[*Mode]("plan.mode")

// segmentName 是 plan 模式段落在 system prompt 里的名字（"15-" 排在
// 身份段之后、todo 段之前）。
const segmentName = "15-plan"

// planSegment 是 plan 模式在场时的提示段。
const planSegment = "## Plan mode\n" +
	"You are in plan mode: research freely with read-only tools, then present a plan and call " +
	"exit_plan_mode with it. Tools that modify anything are blocked until the user approves."

// readOnly 是 plan 模式放行的工具集（拦截器按名判断）。
var readOnly = map[string]bool{
	"read_file":      true,
	"inspect_agent":  true,
	"todo_write":     true,
	"exit_plan_mode": true,
}

// schema 是 exit_plan_mode 工具的参数 JSON Schema。
var schema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "plan": {
      "type": "string",
      "description": "The plan to present to the user for approval"
    }
  },
  "required": ["plan"]
}`)

// Mode 是 plan 模式状态：原子开关 + 段落同步。
type Mode struct {
	on  atomic.Bool
	mu  sync.Mutex
	inv stc.Inverse // 当前段落注销逆；nil = 段落不在册
}

// Enabled 返回 plan 模式是否在场。
func (m *Mode) Enabled() bool { return m.on.Load() }

// set 切换模式并同步段落（空注册表 = 测试裸用 Mode 时跳过段落）。
func (m *Mode) set(on bool, reg *prompt.Segments) {
	m.on.Store(on)
	if reg == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inv != nil {
		_ = m.inv()
		m.inv = nil
	}
	if on {
		m.inv = reg.Register(segmentName, planSegment)
	}
}

// Component 是 plan fiber：提供 Mode 服务，注册 /plan 命令、拦截器与
// exit_plan_mode 工具（全稳定键注入，不随模型级联重载）。interaction
// 走构造注入（spec D18：与审批门同族，不立 fiber 键）。
func Component(ia interaction.Service) stc.Component {
	return stc.Component{
		Name:    "plan",
		Provide: []stc.Key{KeyMode},
		Inject: []stc.Key{prompt.KeyPrompt, session.KeySession, tools.KeyTools,
			hooks.KeyHooks, cli.KeyCommands},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			reg, err := stc.Service[*prompt.Segments](c, prompt.KeyPrompt)
			if err != nil {
				return nil, err
			}
			sess, err := stc.Service[*session.Session](c, session.KeySession)
			if err != nil {
				return nil, err
			}
			ts, err := stc.Service[*tools.Toolset](c, tools.KeyTools)
			if err != nil {
				return nil, err
			}
			ic, err := stc.Service[*hooks.Interceptors](c, hooks.KeyHooks)
			if err != nil {
				return nil, err
			}
			cmds, err := stc.Service[*cli.Registry](c, cli.KeyCommands)
			if err != nil {
				return nil, err
			}
			m := &Mode{}
			inv := make([]stc.Inverse, 0, 3)
			inv = append(inv,
				cmds.Register("plan", func(_ stdctx.Context, w io.Writer, _ string) error {
					if m.Enabled() {
						m.set(false, reg)
						fmt.Fprintln(w, "plan mode off")
					} else {
						m.set(true, reg)
						fmt.Fprintln(w, "plan mode on — non-read-only tools blocked; research, then call exit_plan_mode")
					}
					return nil
				}),
				ic.Register("plan-gate", hooks.Interceptor{
					Event: hooks.ToolPreExecute,
					Check: func(_ stdctx.Context, p hooks.Payload) error {
						if !m.Enabled() || readOnly[p.Tool] {
							return nil
						}
						return fmt.Errorf("plan mode: %q is not read-only; research first, then present the plan via exit_plan_mode", p.Tool)
					},
				}),
				ts.Register("exit_plan_mode", tools.Tool{
					Name: "exit_plan_mode",
					Description: "Present your plan to the user for approval and leave plan mode. On approval the " +
						"mode turns off and you may execute; on rejection you stay in plan mode and revise.",
					Parameters: schema,
					Invoke: func(ctx stdctx.Context, args json.RawMessage) (string, error) {
						return exitPlan(ctx, ia, m, reg, sess, args)
					},
				}),
			)
			if _, err := c.Provide(KeyMode, m); err != nil {
				for _, f := range inv {
					_ = f()
				}
				return nil, err
			}
			return joinInverse(inv), nil
		},
	}
}

// exitPlan 是 exit_plan_mode 的执行体：不在 plan 模式 = no-op 错误；
// 在场则问用户——批准 = 关模式 + 计划回灌；拒绝 = 留在模式；提问处
// Ctrl-C 上抛中断轮次；问不了（headless）= fail-closed 拒绝。
func exitPlan(ctx stdctx.Context, ia interaction.Service, m *Mode, reg *prompt.Segments,
	sess *session.Session, args json.RawMessage) (string, error) {
	var a struct {
		Plan string `json:"plan"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("exit_plan_mode: bad arguments: %w", err)
	}
	if strings.TrimSpace(a.Plan) == "" {
		return "", errors.New("exit_plan_mode: plan is required")
	}
	if !m.Enabled() {
		return "", errors.New("exit_plan_mode: not in plan mode")
	}
	detail := a.Plan
	if len(detail) > 4096 { // 问题详情截断（计划是要给人读的，给足篇幅）
		detail = detail[:4096] + "\n… (truncated)"
	}
	ans, err := ia.Ask(ctx, interaction.Question{
		Title:   "approve this plan and exit plan mode?",
		Detail:  detail,
		Default: "n",
		Options: []interaction.Option{
			{Key: "y", Label: "approve"},
			{Key: "n", Label: "reject"},
		},
	})
	switch {
	case errors.Is(err, interaction.ErrAborted):
		return "", err // 提问处 Ctrl-C：轮次中断，不是拒绝
	case err != nil:
		_ = sess.AddApproval(session.Approval{
			Tool: "exit_plan_mode", Arguments: truncate(a.Plan, 1024),
			Decision: "deny", Source: "fail-closed",
		})
		return fmt.Sprintf("error: plan rejected (fail-closed: %v)", err), nil
	case ans == "y":
		m.set(false, reg)
		_ = sess.AddApproval(session.Approval{
			Tool: "exit_plan_mode", Arguments: truncate(a.Plan, 1024),
			Decision: "allow", Source: "user",
		})
		return "Plan approved by the user. Plan mode is off. The approved plan:\n\n" + a.Plan, nil
	default:
		_ = sess.AddApproval(session.Approval{
			Tool: "exit_plan_mode", Arguments: truncate(a.Plan, 1024),
			Decision: "deny", Source: "user",
		})
		return "The user rejected the plan. You are still in plan mode — revise the plan and call exit_plan_mode again.", nil
	}
}

// truncate 截断字符串（事件日志参数防膨胀，M6 同款约定）。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// joinInverse 合并多个逆（顺序执行，首错返回）。
func joinInverse(invs []stc.Inverse) stc.Inverse {
	return func() error {
		var first error
		for _, f := range invs {
			if err := f(); err != nil && first == nil {
				first = err
			}
		}
		return first
	}
}
