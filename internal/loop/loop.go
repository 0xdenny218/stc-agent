// Package loop implements the agent loop as a fiber: it injects
// chat/session/toolset and provides the turn-runner service the REPL calls.
// The toolset is a stable registry, so tool churn never reloads this fiber —
// each turn reads the current tool list (spec D3). A config re-provision
// reloads it via the chat dependency (spec D4).
package loop

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/0xdenny218/stc-agent/internal/model"
	"github.com/0xdenny218/stc-agent/internal/session"
	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
)

// KeyRunner 是轮次执行服务（REPL 的默认分发目标）。
var KeyRunner = stc.NewKey[Runner]("runner")

type Runner interface {
	RunTurn(ctx stdctx.Context, input string, w io.Writer) error
}

// MaxTurnsError 是熔断：连续工具调用未收敛到最终答复。
type MaxTurnsError struct{ Max int }

func (e *MaxTurnsError) Error() string {
	return fmt.Sprintf("agent: reached max tool turns (%d) without a final answer", e.Max)
}

type runner struct {
	chat     model.ChatService
	sess     *session.Session
	ts       *tools.Toolset
	maxTurns int
}

// Component 是 agent 循环 fiber。maxTurns 是单轮输入允许的最大工具
// 迭代次数。
func Component(maxTurns int) stc.Component {
	return stc.Component{
		Name:    "loop",
		Inject:  []stc.Key{model.KeyChat, session.KeySession, tools.KeyTools},
		Provide: []stc.Key{KeyRunner},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			chat, err := stc.Service[model.ChatService](c, model.KeyChat)
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
			if _, err := c.Provide(KeyRunner, &runner{chat: chat, sess: sess, ts: ts, maxTurns: maxTurns}); err != nil {
				return nil, err
			}
			return nil, nil
		},
	}
}

// RunTurn 执行一轮：user 消息入历史 → [模型 → 工具]* → 最终答复。
// 取消安全：若在工具调用中途被取消，为每个未应答的 tool_call 补一条
// aborted 结果，保证历史线格式合法（assistant 的 tool_calls 必须各有
// 对应 tool 消息）。
func (r *runner) RunTurn(ctx stdctx.Context, input string, w io.Writer) error {
	if err := r.sess.Add(model.Message{Role: "user", Content: input}); err != nil {
		return err
	}
	for range r.maxTurns {
		resp, err := r.chat.Chat(ctx, model.ChatRequest{
			Messages: r.sess.History(),
			Tools:    specs(r.ts.List()),
		})
		if err != nil {
			return err
		}
		msg := resp.Message
		if len(msg.ToolCalls) == 0 {
			if err := r.sess.Add(msg); err != nil {
				return err
			}
			fmt.Fprintln(w, msg.Content)
			return nil
		}
		if err := r.sess.Add(msg); err != nil {
			return err
		}
		answered := 0
		for i, tc := range msg.ToolCalls {
			if ctx.Err() != nil {
				break
			}
			fmt.Fprintf(w, "→ %s(%s)\n", tc.Function.Name, tc.Function.Arguments)
			r.sess.Add(toolResult(tc, r.invoke(ctx, tc)))
			answered = i + 1
		}
		if ctx.Err() != nil {
			for _, tc := range msg.ToolCalls[answered:] {
				_ = r.sess.Add(toolResult(tc, "error: turn aborted (agent reloaded)"))
			}
			return ctx.Err()
		}
	}
	return &MaxTurnsError{Max: r.maxTurns}
}

// invoke 把工具错误归一化为结果文本：坏参数与执行失败都回灌给模型自我
// 纠正，而不是炸掉整轮。
func (r *runner) invoke(ctx stdctx.Context, tc model.ToolCall) string {
	tool, ok := r.ts.Lookup(tc.Function.Name)
	if !ok {
		return fmt.Sprintf("error: unknown tool %q", tc.Function.Name)
	}
	out, err := tool.Invoke(ctx, json.RawMessage(tc.Function.Arguments))
	if err != nil {
		return "error: " + err.Error()
	}
	return out
}

func toolResult(tc model.ToolCall, content string) model.Message {
	return model.Message{Role: "tool", ToolCallID: tc.ID, Content: content}
}

func specs(ts []tools.Tool) []model.ToolSpec {
	if len(ts) == 0 {
		return nil
	}
	out := make([]model.ToolSpec, 0, len(ts))
	for _, t := range ts {
		out = append(out, model.ToolSpec{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	return out
}
