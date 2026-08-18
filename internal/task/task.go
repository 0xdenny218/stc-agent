// Package task implements the sub-agent tool (spec M9): the model can hand
// a self-contained subtask to a child agent that runs its own turns inside
// a child scope (stc-go D17: Context.Child + Isolate). The child shares
// model/approval/hooks/prompt with the parent — unisolated keys resolve up
// the tree — but gets a fresh toolset/session/runner in freshly-created
// realms, so the child's provides never collide with the parent's and the
// child's session never touches the parent's event log. The child session
// is in-memory only; its final assistant message flows back as the tool
// result, which lands in the parent's event log as an ordinary tool
// message ("结果回流父会话事件").
package task

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/0xdenny218/stc-agent/internal/loop"
	"github.com/0xdenny218/stc-agent/internal/session"
	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
	"github.com/0xdenny218/stc-go/registry"
)

// Options 是 task fiber 的装配参数。
type Options struct {
	MaxTurns int // 子 agent 单次调用允许的最大工具迭代次数
}

// schema 是 task 工具的参数 JSON Schema。
var schema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "prompt": {
      "type": "string",
      "description": "Complete, self-contained instructions for the sub-agent"
    },
    "tools": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Tool names the sub-agent may use (default: all current tools except task)"
    }
  },
  "required": ["prompt"]
}`)

// Component 是 task 工具 fiber：向父工具表注册 task 工具（注册=可逆
// 效应）。fiber 只 inject 稳定工具表，模型切换的级联重载不会碰到它；
// 子作用域在每次调用时现场搭建，子 loop 现场解析当时的 chat 服务。
func Component(opts Options) stc.Component {
	return stc.Component{
		Name:   "tool:task",
		Inject: []stc.Key{tools.KeyTools},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			ts, err := stc.Service[*tools.Toolset](c, tools.KeyTools)
			if err != nil {
				return nil, err
			}
			return ts.Register("task", tools.Tool{
				Name: "task",
				Description: "Spawn a sub-agent that works on a self-contained task in its own context and " +
					"returns its final answer. The sub-agent shares your model and approval gate but runs " +
					"on its own session with a subset of your tools. Use it for multi-step subtasks whose " +
					"intermediate details don't belong in this conversation.",
				Parameters: schema,
				Invoke: func(ctx stdctx.Context, args json.RawMessage) (string, error) {
					return run(ctx, c, ts, opts, args)
				},
			}), nil
		},
	}
}

// run 是 task 工具的执行体：解析参数后调用 Run（jobs fiber 的
// job_start(kind=task) 复用同一条子 agent 通路）。
func run(ctx stdctx.Context, home *stc.Context, ts *tools.Toolset, opts Options, args json.RawMessage) (string, error) {
	var a struct {
		Prompt string   `json:"prompt"`
		Tools  []string `json:"tools"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("task: bad arguments: %w", err)
	}
	return Run(ctx, home, ts, opts, a.Prompt, a.Tools)
}

// Run 执行一次子 agent 调用：prompt 为子任务指令，toolNames 为显式
// 工具子集（空 = 全部去掉 task）。home 是调用方 fiber 的周期
// context——fiber 卸载后它关闭，Child 派生得到已关闭 context，
// Isolate 返回 ErrInactive，调用以错误文本告终（优雅降级）。
func Run(ctx stdctx.Context, home *stc.Context, ts *tools.Toolset, opts Options, prompt string, toolNames []string) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", errors.New("task: prompt is required")
	}
	subset, err := pickSubset(ts, toolNames)
	if err != nil {
		return "", err
	}

	child := home.Child()
	// 四个键隔离到各自的新 realm：子域的 provide 不与父域同键冲突
	// （provides 按 (realm, key) 全局唯一），且子域解析不到时沿 realm
	// 父链回落——所以每个被隔离的键都必须在子域里真的提供，否则
	// 子 loop 会拿到父域实例（notices 回落到父域 = 子 agent 偷走
	// 父会话的后台通知）。
	isolations := []struct {
		key  stc.Key
		name string
	}{
		{tools.KeyTools, "task-tools"},
		{session.KeySession, "task-session"},
		{loop.KeyRunner, "task-runner"},
		{loop.KeyNotices, "task-notices"},
	}
	for _, iso := range isolations {
		if err := child.Isolate(iso.key, stc.NewRealm(nil, iso.name)); err != nil {
			return "", err
		}
	}

	// 子域 fiber：内存会话 + 工具子集快照 + 空通知源 + 子 loop。
	// 快照是注册表新实例（子域短命，不追父表后续增删）；Tool 值
	// 拷贝共享 Invoke 闭包——guest/skill 工具在子域照常可用。
	fibers := []*stc.Fiber{
		child.Load(session.Component("")),
		child.Load(toolsetComponent(subset)),
		child.Load(noticesComponent()),
		child.Load(loop.Component(loop.Options{MaxTurns: opts.MaxTurns})),
	}
	// 撤退：fiber 逐个 Dispose（卸载异步，fire-and-forget——unprovide
	// 与 Isolate 撤销都作用于已不可达的 realm，顺序无关，残留 fiber
	// 由根 context 关闭兜底），再 Release 撤销 Isolate 声明。
	defer func() {
		for _, f := range fibers {
			f.Dispose()
		}
		_ = child.Release()
	}()

	// 只等 loop fiber：它 Active 即全部 inject（会话/工具表/通知 +
	// 父域的 chat/approval/hooks/prompt）解析成功。
	boot, cancel := stdctx.WithTimeout(ctx, 10*time.Second)
	err = fibers[len(fibers)-1].Ready(boot)
	cancel()
	if err != nil {
		return "", fmt.Errorf("task: start sub-agent: %w", err)
	}
	runner, err := stc.Service[loop.Runner](child, loop.KeyRunner)
	if err != nil {
		return "", err
	}
	sess, err := stc.Service[*session.Session](child, session.KeySession)
	if err != nil {
		return "", err
	}

	// 子 agent 的流式内容与工具轨迹不进父终端（工具拿不到父轮次的
	// writer）；可观测性走 hooks 事件——turn/tool 事件经全局监听表
	// 照常抵达父域监听者。ctx 同源：用户中断同时取消子轮。
	if err := runner.RunTurn(ctx, prompt, io.Discard); err != nil {
		return "", err
	}

	// 终答 = 子会话最后一条非空 assistant 消息。
	hist := sess.History()
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i].Role == "assistant" && hist[i].Content != "" {
			return hist[i].Content, nil
		}
	}
	return "(sub-agent finished without a final answer)", nil
}

// pickSubset 选子 agent 的工具集：默认父工具表去掉 task（不许递归）；
// 显式指定 names 则按名取——不存在的名字与 task 本身报错回灌模型。
func pickSubset(ts *tools.Toolset, names []string) ([]tools.Tool, error) {
	if len(names) == 0 {
		var out []tools.Tool
		for _, t := range ts.List() {
			if t.Name != "task" {
				out = append(out, t)
			}
		}
		return out, nil
	}
	out := make([]tools.Tool, 0, len(names))
	for _, n := range names {
		if n == "task" {
			return nil, errors.New(`task: "task" is not available to sub-agents (no recursion)`)
		}
		t, ok := ts.Lookup(n)
		if !ok {
			return nil, fmt.Errorf("task: unknown tool %q for sub-agent", n)
		}
		out = append(out, t)
	}
	return out, nil
}

// toolsetComponent 在子域提供装好子集的工具注册表。
func toolsetComponent(subset []tools.Tool) stc.Component {
	return stc.Component{
		Name:    "task-toolset",
		Provide: []stc.Key{tools.KeyTools},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			sub := registry.New[tools.Tool]()
			for _, t := range subset {
				_ = sub.Register(t.Name, t) // 逆随子域撤退，无需逐个持有
			}
			_, err := c.Provide(tools.KeyTools, sub)
			return nil, err
		},
	}
}

// emptyNotices 是子域的空通知源：后台完成通知只该进父会话。
type emptyNotices struct{}

func (emptyNotices) Drain() []string { return nil }

func noticesComponent() stc.Component {
	return stc.Component{
		Name:    "task-notices",
		Provide: []stc.Key{loop.KeyNotices},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			_, err := c.Provide(loop.KeyNotices, emptyNotices{})
			return nil, err
		},
	}
}
