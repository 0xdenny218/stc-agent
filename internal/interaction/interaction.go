// Package interaction 定义用户提问/审批/中断的服务化接口（spec D18）。
// 终端行交互是默认 provider（internal/cli），构造时注入、实现可换——
// 将来真有 TUI 也只是换 provider，审批门等核心不动。
package interaction

import (
	stdctx "context"
	"errors"
)

// ErrUnavailable 表示当前 provider 无法提问（headless 等）。调用方按
// fail-closed 处理（spec D15：问不了就拒绝）。
var ErrUnavailable = errors.New("interaction: no interactive provider")

// ErrAborted 表示用户在提问处按了 Ctrl-C。语义是中断当前轮，而不是
// 拒绝这一次调用——拒绝请用回答键（如 "n"）。
var ErrAborted = errors.New("interaction: prompt aborted")

// Option 是提问的一个可选回答。
type Option struct {
	Key   string // 回答键（如 "y"/"n"/"a"），Ask 返回所选键
	Label string // 展示文案
}

// Question 是一次途中提问（工具审批门等）。
type Question struct {
	Title   string   // 一句话问题（`allow "shell" to run?`）
	Detail  string   // 细节（工具参数等，可空）
	Default string   // 回车/EOF 时的默认回答键（审批门应取拒绝向）
	Options []Option // 可选回答
}

// Service 是提问服务：途中挂起等用户回答，返回所选 Option.Key。
type Service interface {
	Ask(ctx stdctx.Context, q Question) (string, error)
}

// deny 是 headless provider：任何提问都报 ErrUnavailable。
type deny struct{}

func (deny) Ask(stdctx.Context, Question) (string, error) { return "", ErrUnavailable }

// Deny 返回 fail-closed 的 provider（-p headless 模式使用）。
func Deny() Service { return deny{} }
