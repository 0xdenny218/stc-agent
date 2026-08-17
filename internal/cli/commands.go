// Package cli implements the terminal REPL and the slash-command machinery.
// Command registration is a revertible effect (spec D3's pattern): a command
// fiber unloading unregisters its command.
package cli

import (
	stdctx "context"
	"io"
	"sort"
	"strings"
	"sync"

	stc "github.com/0xdenny218/stc-go"
)

// KeyCommands 是斜杠命令注册表服务。
var KeyCommands = stc.NewKey[*Registry]("commands")

type CommandFunc func(ctx stdctx.Context, w io.Writer, args string) error

type Registry struct {
	mu   sync.RWMutex
	cmds map[string]CommandFunc
}

func NewRegistry() *Registry {
	return &Registry{cmds: make(map[string]CommandFunc)}
}

// Register 登记命令，返回注销它的逆。
func (r *Registry) Register(name string, fn CommandFunc) stc.Inverse {
	r.mu.Lock()
	r.cmds[name] = fn
	r.mu.Unlock()
	return func() error {
		r.mu.Lock()
		delete(r.cmds, name)
		r.mu.Unlock()
		return nil
	}
}

// Dispatch 解析 "/name args" 并调用；未注册返回 handled=false。
func (r *Registry) Dispatch(ctx stdctx.Context, w io.Writer, line string) (handled bool, err error) {
	name, args, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	r.mu.RLock()
	fn, ok := r.cmds[name]
	r.mu.RUnlock()
	if !ok {
		return false, nil
	}
	return true, fn(ctx, w, strings.TrimSpace(args))
}

// Names 列出已注册命令（/help 用，M2）。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.cmds))
	for n := range r.cmds {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// RegistryComponent 提供稳定的命令注册表（inject 为空 → 不随任何级联
// 重载）。
func RegistryComponent() stc.Component {
	return stc.Component{
		Name:    "commands",
		Provide: []stc.Key{KeyCommands},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			_, err := c.Provide(KeyCommands, NewRegistry())
			return nil, err
		},
	}
}
