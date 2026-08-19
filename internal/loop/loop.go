// Package loop implements the agent loop as a fiber: it injects
// chat/session/toolset and provides the turn-runner service the REPL calls.
// The toolset is a stable registry, so tool churn never reloads this fiber —
// each turn reads the current tool list (spec D3). A config re-provision
// reloads it via the chat dependency (spec D4).
package loop

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/0xdenny218/stc-agent/internal/approval"
	"github.com/0xdenny218/stc-agent/internal/hooks"
	"github.com/0xdenny218/stc-agent/internal/model"
	"github.com/0xdenny218/stc-agent/internal/prompt"
	"github.com/0xdenny218/stc-agent/internal/session"
	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
)

// KeyRunner 是轮次执行服务（REPL 的默认分发目标）。
var KeyRunner = stc.NewKey[Runner]("runner")

// KeyNotices 是异步通知服务（spec M9）：jobs 等后台来源把完成通知挂在
// 这里，loop 在每次模型调用前 drain 成 user 消息入历史——只在调用前
// drain，历史尾部此时必是 user/tool 消息，线格式恒合法。服务缺失 =
// 没有后台来源，drain 跳过（可选依赖）。
var KeyNotices = stc.NewKey[Notices]("loop.notices")

// Notices 是后台完成通知的出口（jobs fiber 提供）。
type Notices interface {
	// Drain 取走全部待发通知（每条渲染为一则 user 消息文本）。
	Drain() []string
}

type Runner interface {
	RunTurn(ctx stdctx.Context, input string, w io.Writer) error
}

// MaxTurnsError 是熔断：连续工具调用未收敛到最终答复。
type MaxTurnsError struct{ Max int }

func (e *MaxTurnsError) Error() string {
	return fmt.Sprintf("agent: reached max tool turns (%d) without a final answer", e.Max)
}

// Options 是 loop fiber 的装配参数。
type Options struct {
	MaxTurns int // 单轮输入允许的最大工具迭代次数
	// CompactThreshold：turn 结束后最近一次模型请求的 prompt tokens 超过
	// 该阈值即压缩历史（摘要事件入日志，投影折叠；spec M9）。0 = 不压缩。
	CompactThreshold int
}

type runner struct {
	chat     model.ChatService
	sess     *session.Session
	ts       *tools.Toolset
	gate     approval.Gate
	ic       *hooks.Interceptors
	segments *prompt.Segments
	fctx     *stc.Context // 周期 context：通知型事件的 Emit 端
	notices  Notices      // nil = 无后台通知源
	maxTurns int
	compact  int
}

// Component 是 agent 循环 fiber。
func Component(opts Options) stc.Component {
	return stc.Component{
		Name: "loop",
		Inject: []stc.Key{model.KeyChat, session.KeySession, tools.KeyTools,
			approval.KeyApprover, hooks.KeyHooks, prompt.KeyPrompt},
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
			gate, err := stc.Service[approval.Gate](c, approval.KeyApprover)
			if err != nil {
				return nil, err
			}
			ic, err := stc.Service[*hooks.Interceptors](c, hooks.KeyHooks)
			if err != nil {
				return nil, err
			}
			segments, err := stc.Service[*prompt.Segments](c, prompt.KeyPrompt)
			if err != nil {
				return nil, err
			}
			// 可选依赖：没有 jobs fiber 时 notices 为 nil，drain 跳过。
			notices, _ := stc.Service[Notices](c, KeyNotices)
			if _, err := c.Provide(KeyRunner, &runner{
				chat: chat, sess: sess, ts: ts, gate: gate,
				ic: ic, segments: segments, fctx: c,
				notices: notices, maxTurns: opts.MaxTurns, compact: opts.CompactThreshold,
			}); err != nil {
				return nil, err
			}
			return nil, nil
		},
	}
}

// RunTurn 执行一轮：user 消息入历史 → [模型 → 工具]* → 最终答复。模型
// 内容增量即时渲染到 w（spec D14），每次模型请求的 token 用量写入会话
// 事件日志（spec D13）。system prompt 每次请求前从段落注册表现装
// （spec D16：段落 fiber 增删即下一轮生效）；轮次边界与工具执行都会
// 派发 hooks 事件。取消安全：若在工具调用中途被取消，为每个未应答
// 的 tool_call 补一条 aborted 结果，保证历史线格式合法（assistant 的
// tool_calls 必须各有对应 tool 消息）。
func (r *runner) RunTurn(ctx stdctx.Context, input string, w io.Writer) (err error) {
	// 可选依赖惰性解析：Apply 时 jobs fiber 可能尚未 provide（装载
	// 异步）；REPL 在全部 fiber Ready 后才发轮，此处解析必然可见。
	if r.notices == nil {
		if n, err := stc.Service[Notices](r.fctx, KeyNotices); err == nil {
			r.notices = n
		}
	}
	hooks.Emit(r.fctx, hooks.TurnStart, hooks.Payload{Text: input})
	var final string
	defer func() {
		if err != nil {
			final = "error: " + err.Error()
		}
		hooks.Emit(r.fctx, hooks.TurnEnd, hooks.Payload{Text: final})
	}()

	if err := r.sess.Add(model.Message{Role: "user", Content: input}); err != nil {
		return err
	}
	for range r.maxTurns {
		// 后台完成通知只在模型调用前入历史（此时尾部必是 user/tool
		// 消息，线格式恒合法）。
		if r.notices != nil {
			for _, n := range r.notices.Drain() {
				if err := r.sess.Add(model.Message{Role: "user", Content: n}); err != nil {
					return err
				}
			}
		}
		resp, err := r.chat.Chat(ctx, model.ChatRequest{
			System:   prompt.Assemble(r.segments),
			Messages: r.sess.History(),
			Tools:    specs(r.ts.List()),
		}, func(delta string) { fmt.Fprint(w, delta) })
		if err != nil {
			return err
		}
		msg := resp.Message
		if err := r.sess.Add(msg); err != nil {
			return err
		}
		if err := r.sess.AddUsage(resp.Usage); err != nil {
			return err
		}
		if len(msg.ToolCalls) == 0 {
			fmt.Fprintln(w) // 内容已流式打出，补收尾换行
			// 用量随轮打印（spec M11 小件）：一行，轻量可见。
			fmt.Fprintf(w, "[tokens: prompt %d + completion %d = %d]\n",
				resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
			final = msg.Content
			r.maybeCompact(ctx, w)
			return nil
		}
		if msg.Content != "" {
			fmt.Fprintln(w) // 流式内容与工具轨迹之间换行
		}
		answered := 0
		for i, tc := range msg.ToolCalls {
			if ctx.Err() != nil {
				break
			}
			fmt.Fprintf(w, "→ %s(%s)\n", tc.Function.Name, tc.Function.Arguments)
			out, err := r.runTool(ctx, tc)
			if err != nil {
				// 轮次级中断（审批提问处 Ctrl-C 等）：当前与后续
				// tool_call 补中断标记，保持线格式合法。
				r.fillUnanswered(msg.ToolCalls[i:], "error: turn interrupted")
				return err
			}
			r.sess.Add(toolResult(tc, out))
			answered = i + 1
		}
		if ctx.Err() != nil {
			r.fillUnanswered(msg.ToolCalls[answered:], "error: turn aborted (agent reloaded)")
			return ctx.Err()
		}
	}
	return &MaxTurnsError{Max: r.maxTurns}
}

func (r *runner) fillUnanswered(tcs []model.ToolCall, content string) {
	for _, tc := range tcs {
		_ = r.sess.Add(toolResult(tc, content))
	}
}

// compactInstruction 是摘要请求的收尾指令（spec M9：压缩是事件日志上的
// 投影策略，摘要进 compaction 事件，被压缩段在 replay 时折叠）。
const compactInstruction = "Summarize the conversation above in at most 500 words, preserving: the user's goals, decisions made, files and tools touched, and any pending tasks. Reply with the summary only."

// maybeCompact 在一轮成功结束后检查：最近一次模型请求的 prompt tokens
// 超过阈值即让模型摘要当前全部历史，compaction 事件入日志（投影立即
// 折叠，下一个请求的历史从摘要开始）。压缩失败不炸已完成的轮次——只
// 报告一行，下轮再试。
func (r *runner) maybeCompact(ctx stdctx.Context, w io.Writer) {
	if r.compact <= 0 {
		return
	}
	u, ok := r.sess.LastUsage()
	if !ok || u.PromptTokens <= r.compact {
		return
	}
	history := r.sess.History()
	if len(history) < 2 {
		return // 空历史或只剩一条摘要，没有值得折叠的
	}
	req := model.ChatRequest{
		Messages: append(append([]model.Message(nil), history...),
			model.Message{Role: "user", Content: compactInstruction}),
	}
	resp, err := r.chat.Chat(ctx, req, func(string) {}) // 摘要不流向终端
	if err != nil {
		fmt.Fprintf(w, "[compaction failed: %v — will retry after the next turn]\n", err)
		return
	}
	n := len(history)
	if err := r.sess.AddCompaction(resp.Message.Content); err != nil {
		fmt.Fprintf(w, "[compaction failed: %v]\n", err)
		return
	}
	_ = r.sess.AddUsage(resp.Usage) // 摘要本身的成本也入账
	fmt.Fprintf(w, "[compacted %d messages (prompt tokens %d > threshold %d)]\n", n, u.PromptTokens, r.compact)
}

// runTool 是工具执行管线（spec D15/D16）：pre = 拦截 hook（bail 即阻断）
// → 审批门；执行；post = 结果归一化 + tools/post-execute 通知——坏参数、
// 执行失败、hook 阻断与审批拒绝都化作结果文本回灌模型自我纠正，只有
// 轮次级中断（提问处 Ctrl-C、ctx 取消）以 error 上抛。post-execute
// 只在工具真实执行后派发（被拦/被拒的调用没有执行，不派发）。
func (r *runner) runTool(ctx stdctx.Context, tc model.ToolCall) (string, error) {
	tool, ok := r.ts.Lookup(tc.Function.Name)
	if !ok {
		return fmt.Sprintf("error: unknown tool %q", tc.Function.Name), nil
	}
	p := hooks.Payload{Tool: tc.Function.Name, Arguments: tc.Function.Arguments}
	if err := hooks.Check(ctx, r.ic, hooks.ToolPreExecute, p); err != nil {
		return "error: " + err.Error(), nil
	}
	if err := r.gate.Check(ctx, tc); err != nil {
		var de *approval.DeniedError
		if errors.As(err, &de) {
			return "error: " + de.Reason, nil
		}
		return "", err
	}
	out, err := tool.Invoke(ctx, json.RawMessage(tc.Function.Arguments))
	if err != nil {
		out = "error: " + err.Error()
	}
	p.Result = out
	hooks.Emit(r.fctx, hooks.ToolPostExecute, p)
	return out, nil
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
