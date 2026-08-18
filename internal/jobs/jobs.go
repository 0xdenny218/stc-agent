// Package jobs implements background jobs (spec M9): job_start spawns a
// shell command or a sub-agent task (the same task.Run path as the task
// tool), job_list enumerates them, job_kill cancels one. Completion
// notifications queue up behind the loop.Notices service and enter the
// session as user messages at the next model call — shell and sub-agent
// background work converge on one lifecycle.
package jobs

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"github.com/0xdenny218/stc-agent/internal/loop"
	"github.com/0xdenny218/stc-agent/internal/task"
	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
)

// Options 是 jobs fiber 的装配参数。
type Options struct {
	MaxTurns int // kind=task 的子 agent 工具迭代上限（与 task 工具同族）
}

// Job 是一条后台任务的状态（job_list 渲染用字段公开，cancel/result
// 等内部字段不导出）。
type Job struct {
	ID     int
	Kind   string // "shell" | "task"
	Detail string // 命令行或子任务 prompt（截断展示）
	Status string // "running" | "done" | "failed" | "killed"
	Result string

	cancel stdctx.CancelFunc
}

// Manager 是任务表 + 完成通知队列；实现 loop.Notices（Drain 一次性
// 取走全部通知）。
type Manager struct {
	mu      sync.Mutex
	next    int
	jobs    map[int]*Job
	notices []string
}

// Drain 取走全部待发通知（loop 在模型调用前 drain 成 user 消息）。
func (m *Manager) Drain() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.notices
	m.notices = nil
	return out
}

// List 返回按 ID 排序的任务快照。
func (m *Manager) List() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, *j)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].ID < out[k].ID })
	return out
}

// Kill 取消一条任务；未知 ID 返回错误文本（回灌模型）。
func (m *Manager) Kill(id int) error {
	m.mu.Lock()
	j, ok := m.jobs[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("job_kill: no job %d", id)
	}
	j.cancel()
	return nil
}

// start 登记任务并返回其 ID 与取消 context（调用方在自身 goroutine
// 里把实际工作绑到该 ctx 上，job_kill 即取消它）。
func (m *Manager) start(kind, detail string) (int, stdctx.Context) {
	m.mu.Lock()
	m.next++
	id := m.next
	ctx, cancel := stdctx.WithCancel(stdctx.Background())
	m.jobs[id] = &Job{ID: id, Kind: kind, Detail: detail, Status: "running", cancel: cancel}
	m.mu.Unlock()
	return id, ctx
}

// finish 落终态并入通知队列。
func (m *Manager) finish(id int, status, result string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[id]; ok {
		j.Status = status
		j.Result = result
	}
	m.notices = append(m.notices, fmt.Sprintf("[job %d %s] %s", id, status, preview(result, 200)))
}

// preview 截断首行/多行压平展示。
func preview(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "…"
	}
	if s == "" {
		s = "(no output)"
	}
	return s
}

// Component 是 jobs fiber：提供 loop.Notices 服务，注册 job_start /
// job_list / job_kill 三个工具。inject 全稳定键，不随模型级联重载；
// kind=task 的子作用域挂在 jobs fiber 的周期 context 下（fiber 卸载后
// 新任务拒绝启动，在跑任务自然收尾）。
func Component(opts Options) stc.Component {
	return stc.Component{
		Name:    "jobs",
		Provide: []stc.Key{loop.KeyNotices},
		Inject:  []stc.Key{tools.KeyTools},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			ts, err := stc.Service[*tools.Toolset](c, tools.KeyTools)
			if err != nil {
				return nil, err
			}
			m := &Manager{jobs: make(map[int]*Job)}
			home := c
			tasks := task.Options{MaxTurns: opts.MaxTurns}
			inv := []stc.Inverse{
				ts.Register("job_start", tools.Tool{
					Name: "job_start",
					Description: "Start a background job and return immediately: kind=shell runs a command " +
						"(sh -c, no timeout, output captured), kind=task runs a sub-agent on a self-contained prompt. " +
						"Completion arrives as a user message at your next model call; use job_list/job_kill meanwhile.",
					Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "kind": {"type": "string", "enum": ["shell", "task"]},
    "command": {"type": "string", "description": "shell command (kind=shell)"},
    "prompt": {"type": "string", "description": "self-contained sub-agent instructions (kind=task)"},
    "tools": {"type": "array", "items": {"type": "string"}, "description": "tool subset for kind=task (default: all except task)"}
  },
  "required": ["kind"]
}`),
					Invoke: func(ctx stdctx.Context, args json.RawMessage) (string, error) {
						var a struct {
							Kind    string   `json:"kind"`
							Command string   `json:"command"`
							Prompt  string   `json:"prompt"`
							Tools   []string `json:"tools"`
						}
						if err := json.Unmarshal(args, &a); err != nil {
							return "", fmt.Errorf("job_start: bad arguments: %w", err)
						}
						switch a.Kind {
						case "shell":
							if strings.TrimSpace(a.Command) == "" {
								return "", errors.New("job_start: command is required for kind=shell")
							}
							id, jctx := m.start("shell", a.Command)
							go func() {
								cmd := exec.CommandContext(jctx, "sh", "-c", a.Command)
								out, err := cmd.CombinedOutput()
								m.finish(id, status(jctx, err), preview(string(out), 4000))
							}()
							return fmt.Sprintf("[job %d started] shell: %s", id, preview(a.Command, 80)), nil
						case "task":
							if strings.TrimSpace(a.Prompt) == "" {
								return "", errors.New("job_start: prompt is required for kind=task")
							}
							id, jctx := m.start("task", a.Prompt)
							go func() {
								out, err := task.Run(jctx, home, ts, tasks, a.Prompt, a.Tools)
								m.finish(id, status(jctx, err), resultText(out, err))
							}()
							return fmt.Sprintf("[job %d started] task: %s", id, preview(a.Prompt, 80)), nil
						default:
							return "", fmt.Errorf("job_start: unknown kind %q (want shell | task)", a.Kind)
						}
					},
				}),
				ts.Register("job_list", tools.Tool{
					Name:        "job_list",
					Description: "List background jobs with status (running | done | failed | killed).",
					Parameters:  json.RawMessage(`{"type":"object"}`),
					Invoke: func(stdctx.Context, json.RawMessage) (string, error) {
						js := m.List()
						if len(js) == 0 {
							return "no jobs", nil
						}
						var b strings.Builder
						for _, j := range js {
							fmt.Fprintf(&b, "%d\t%s\t%s\t%s", j.ID, j.Kind, j.Status, preview(j.Detail, 60))
							if j.Status != "running" {
								fmt.Fprintf(&b, "\t→ %s", preview(j.Result, 60))
							}
							b.WriteByte('\n')
						}
						return b.String(), nil
					},
				}),
				ts.Register("job_kill", tools.Tool{
					Name:        "job_kill",
					Description: "Kill a background job by id.",
					Parameters:  json.RawMessage(`{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"]}`),
					Invoke: func(_ stdctx.Context, args json.RawMessage) (string, error) {
						var a struct {
							ID int `json:"id"`
						}
						if err := json.Unmarshal(args, &a); err != nil {
							return "", fmt.Errorf("job_kill: bad arguments: %w", err)
						}
						if err := m.Kill(a.ID); err != nil {
							return "", err
						}
						return fmt.Sprintf("job %d kill signal sent", a.ID), nil
					},
				}),
			}
			if _, err := c.Provide(loop.KeyNotices, m); err != nil {
				for _, f := range inv {
					_ = f()
				}
				return nil, err
			}
			return func() error {
				for _, f := range inv {
					if err := f(); err != nil {
						return err
					}
				}
				return nil
			}, nil
		},
	}
}

// status 判定终态：被取消 = killed，出错 = failed，否则 done。
func status(ctx stdctx.Context, err error) string {
	switch {
	case ctx.Err() != nil:
		return "killed"
	case err != nil:
		return "failed"
	default:
		return "done"
	}
}

// resultText 把任务产出归一为展示文本。
func resultText(out string, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	return out
}
