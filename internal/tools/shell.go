package tools

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"

	stc "github.com/0xdenny218/stc-go"
)

// ShellComponent 执行 shell 命令的工具。最小防护（spec D10）：超时 +
// 限定工作目录，无审批流——风险由 README 明示。
func ShellComponent(dir string, timeout time.Duration) stc.Component {
	return component(Tool{
		Name:        "shell",
		Description: fmt.Sprintf("run a shell command (sh -c) with working directory %s", dir),
		Parameters:  json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"the command to run"}},"required":["command"]}`),
		Invoke: func(ctx stdctx.Context, args json.RawMessage) (string, error) {
			var a struct {
				Command string `json:"command"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return "", err
			}
			if a.Command == "" {
				return "", errors.New("invalid arguments: command is required")
			}
			ctx, cancel := stdctx.WithTimeout(ctx, timeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "-c", a.Command)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if errors.Is(ctx.Err(), stdctx.DeadlineExceeded) {
				return "", fmt.Errorf("shell: command timed out after %s", timeout)
			}
			s := capOutput(string(out))
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				if s == "" {
					s = "(no output)"
				}
				return fmt.Sprintf("%s\n(exit status %d)", s, exitErr.ExitCode()), nil
			}
			if err != nil {
				return "", fmt.Errorf("shell: %w", err)
			}
			if s == "" {
				return "(no output)", nil
			}
			return s, nil
		},
	})
}
