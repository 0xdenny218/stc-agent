// Package cli implements the terminal REPL and the slash-command machinery.
// Command registration is a revertible effect (spec D3's pattern): a command
// fiber unloading unregisters its command. The registry itself is stc-go's
// registry satellite (stc-go v0.4.0, #2); this package keeps the domain
// parts: CommandFunc and slash-line dispatch.
package cli

import (
	stdctx "context"
	"io"
	"strings"

	stc "github.com/0xdenny218/stc-go"
	"github.com/0xdenny218/stc-go/registry"
)

// KeyCommands 是斜杠命令注册表服务。
var KeyCommands = stc.NewKey[*Registry]("commands")

// Registry 是命令的稳定注册表（stc-go/registry 的实例化）。
type Registry = registry.Registry[CommandFunc]

type CommandFunc func(ctx stdctx.Context, w io.Writer, args string) error

// Dispatch 解析 "/name args" 并调用；未注册返回 handled=false。
func Dispatch(ctx stdctx.Context, w io.Writer, line string, r *Registry) (handled bool, err error) {
	name, args, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	fn, ok := r.Lookup(name)
	if !ok {
		return false, nil
	}
	return true, fn(ctx, w, strings.TrimSpace(args))
}

// RegistryComponent 提供稳定的命令注册表（inject 为空 → 不随任何级联
// 重载）。
func RegistryComponent() stc.Component {
	return registry.Component[CommandFunc]("commands", KeyCommands)
}
