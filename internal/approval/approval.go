// Package approval 实现工具执行的审批门（spec D15）：审批是执行的
// coeffect——策略由配置提供（内置默认只读放行，其余询问），询问经
// interaction 服务（D18），无交互提供者时 fail-closed；决定写入会话
// 事件日志（D13）。仍不做 sandbox 隔离。
package approval

import (
	stdctx "context"
	"errors"
	"fmt"
	"sync"

	"github.com/0xdenny218/stc-agent/internal/interaction"
	"github.com/0xdenny218/stc-agent/internal/model"
	"github.com/0xdenny218/stc-agent/internal/session"
	stc "github.com/0xdenny218/stc-go"
)

// Policy 是审批策略：工具名精确匹配，"*" 匹配全部；deny 优先于 allow。
type Policy struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

// DefaultPolicy 是内置默认（配置优先级的最底层）：只读本地工具
// （read_file / inspect_agent / glob / grep）直接放行；其余一律询问——
// 询问无门即拒绝，是为 fail-closed（spec D15）。写工具（edit/spill）、
// 远程工具（web_fetch/web_search）与 define_guest 均需人工过问。
func DefaultPolicy() Policy {
	return Policy{Allow: []string{"read_file", "inspect_agent", "glob", "grep"}}
}

func (p Policy) allows(name string) bool { return match(p.Allow, name) }
func (p Policy) denies(name string) bool { return match(p.Deny, name) }

func match(list []string, name string) bool {
	for _, pat := range list {
		if pat == "*" || pat == name {
			return true
		}
	}
	return false
}

// DeniedError 表示工具调用被拒绝（策略/用户/fail-closed）。它作为工具
// 结果回灌模型自我纠正，不是轮次级错误。
type DeniedError struct {
	Tool   string
	Reason string
}

func (e *DeniedError) Error() string {
	return fmt.Sprintf("approval: tool %q denied: %s", e.Tool, e.Reason)
}

// Gate 是审批门服务：loop 工具管线的 pre 阶段（spec D15）。
type Gate interface {
	Check(ctx stdctx.Context, tc model.ToolCall) error
}

// KeyApprover 是审批门能力键。
var KeyApprover = stc.NewKey[Gate]("approval")

// Approver 是审批门实现：策略判定 → 询问 → 决定落事件日志。
type Approver struct {
	policy Policy
	ia     interaction.Service
	sess   *session.Session

	mu     sync.Mutex
	always map[string]bool // 用户在本次会话里 "always allow" 的工具
}

// New 构造 Approver（Component 的 Apply 与测试共用）。
func New(policy Policy, ia interaction.Service, sess *session.Session) *Approver {
	return &Approver{policy: policy, ia: ia, sess: sess, always: map[string]bool{}}
}

// Component 是审批门 fiber。只 inject session——不碰 config，换模型的
// 级联不重载它，会话内的 always-allow 集合随 fiber 存活。
func Component(policy Policy, ia interaction.Service) stc.Component {
	return stc.Component{
		Name:    "approval",
		Inject:  []stc.Key{session.KeySession},
		Provide: []stc.Key{KeyApprover},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			sess, err := stc.Service[*session.Session](c, session.KeySession)
			if err != nil {
				return nil, err
			}
			_, err = c.Provide(KeyApprover, Gate(New(policy, ia, sess)))
			return nil, err
		},
	}
}

// Check 审批一次工具调用：nil = 放行；*DeniedError = 拒绝（loop 归一化
// 为工具结果回灌模型）；interaction.ErrAborted / ctx 取消 = 轮次级中断，
// 原样上抛。
func (a *Approver) Check(ctx stdctx.Context, tc model.ToolCall) error {
	name := tc.Function.Name
	args := tc.Function.Arguments
	if a.policy.denies(name) {
		return a.deny(name, args, "policy", "denied by policy")
	}
	if a.policy.allows(name) || a.alwaysAllowed(name) {
		return nil // 静态放行路径：不产生事件
	}
	ans, err := a.ia.Ask(ctx, interaction.Question{
		Title:   fmt.Sprintf("allow %q to run?", name),
		Detail:  truncate(args, 240),
		Default: "n",
		Options: []interaction.Option{
			{Key: "y", Label: "allow once"},
			{Key: "n", Label: "deny"},
			{Key: "a", Label: "always allow " + name},
		},
	})
	switch {
	case errors.Is(err, interaction.ErrUnavailable):
		return a.deny(name, args, "fail-closed", "no interactive approval available")
	case err != nil:
		return err
	case ans == "y":
		return a.allow(name, args)
	case ans == "a":
		a.mu.Lock()
		a.always[name] = true
		a.mu.Unlock()
		return a.allow(name, args)
	default:
		return a.deny(name, args, "user", "denied by user")
	}
}

func (a *Approver) alwaysAllowed(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.always[name]
}

func (a *Approver) allow(name, args string) error {
	return a.record(name, args, "allow", "user")
}

func (a *Approver) deny(name, args, source, reason string) error {
	if err := a.record(name, args, "deny", source); err != nil {
		return err
	}
	return &DeniedError{Tool: name, Reason: reason}
}

// record 把决定写进事件日志。写穿失败原样上抛（审计写入失败不应静默）。
func (a *Approver) record(name, args, decision, source string) error {
	return a.sess.AddApproval(session.Approval{
		Tool:      name,
		Arguments: truncate(args, 1024),
		Decision:  decision,
		Source:    source,
	})
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
