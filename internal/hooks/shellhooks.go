// shellhooks.go 实现配置级 shell hook（spec M11）：用户在配置文件里把
// shell 命令挂到事件上，不用写 Go。事件名即 hooks 事件域键
// （agent/turn-start、agent/turn-end、tools/post-execute 是通知位——命令
// 跑完即忘，失败只上报一行；tools/pre-execute 是拦截位——退出码非 0
// 阻断本次工具调用，stderr 末行作为理由回灌模型）。载荷经环境变量注入：
// STC_HOOK_EVENT / STC_HOOK_TOOL / STC_HOOK_ARGUMENTS / STC_HOOK_RESULT /
// STC_HOOK_TEXT。
package hooks

import (
	stdctx "context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	stc "github.com/0xdenny218/stc-go"
)

// ShellHookEvents 是配置文件 "hooks" 键允许的事件名（fail-fast 校验）。
var ShellHookEvents = map[string]bool{
	TurnStart:       true,
	TurnEnd:         true,
	ToolPreExecute:  true,
	ToolPostExecute: true,
}

// ShellComponent 把 spec（事件 → shell 命令）装为一个 hook fiber。out
// 是失败上报的落点；timeout 限制单次命令执行。
func ShellComponent(spec map[string]string, out io.Writer, timeout time.Duration) stc.Component {
	return stc.Component{
		Name:   "hooks:shell",
		Inject: []stc.Key{KeyHooks},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			reg, err := stc.Service[*Interceptors](c, KeyHooks)
			if err != nil {
				return nil, err
			}
			var unregs []stc.Inverse
			fail := func(err error) (stc.Inverse, error) {
				for _, un := range unregs {
					_ = un()
				}
				return nil, err
			}
			for ev, cmd := range spec {
				event, command := ev, cmd
				switch event {
				case ToolPreExecute:
					unregs = append(unregs, reg.Register("shell:"+event, Interceptor{
						Event: event,
						Check: func(ctx stdctx.Context, p Payload) error {
							_, tail, err := runShellHook(ctx, event, p, command, timeout)
							if err == nil {
								return nil
							}
							reason := strings.TrimSpace(tail)
							if reason == "" {
								reason = err.Error()
							}
							return fmt.Errorf("hook %s blocked: %s", event, reason)
						},
					}))
				default:
					if err := Listen(c, event, func(p Payload) {
						_, tail, err := runShellHook(stdctx.Background(), event, p, command, timeout)
						if err != nil {
							msg := strings.TrimSpace(tail)
							if len(msg) > 200 {
								msg = msg[len(msg)-200:]
							}
							if msg == "" {
								msg = err.Error()
							}
							fmt.Fprintf(out, "[hook %s] failed: %s\n", event, msg)
						}
					}); err != nil {
						return fail(err)
					}
				}
			}
			return func() error {
				for _, un := range unregs {
					_ = un()
				}
				return nil
			}, nil
		},
	}
}

// runShellHook 执行一条 hook 命令，返回输出与错误。
func runShellHook(ctx stdctx.Context, event string, p Payload, command string, timeout time.Duration) (string, string, error) {
	ctx, cancel := stdctx.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Env = append(os.Environ(),
		"STC_HOOK_EVENT="+event,
		"STC_HOOK_TOOL="+p.Tool,
		"STC_HOOK_ARGUMENTS="+p.Arguments,
		"STC_HOOK_RESULT="+p.Result,
		"STC_HOOK_TEXT="+p.Text,
	)
	out, err := cmd.CombinedOutput()
	tail := string(out)
	if len(tail) > 1000 {
		tail = tail[len(tail)-1000:]
	}
	return string(out), tail, err
}
