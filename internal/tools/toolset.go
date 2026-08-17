// Package tools implements the tool registry and the static Go tool fibers.
// Registration is a revertible effect (spec D3): a tool fiber registers
// itself into the stable toolset, so adding/removing tools never reloads
// the agent loop — the loop reads the current tool list per turn.
package tools

import (
	stdctx "context"
	"encoding/json"
	"sort"
	"sync"

	stc "github.com/0xdenny218/stc-go"
)

// KeyTools 是工具注册表服务（稳定；inject 为空，不随级联重载）。
var KeyTools = stc.NewKey[*Toolset]("tools")

// Tool 是一个可被模型调用的能力：线格式描述 + 调用闭包。
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON Schema
	Invoke      func(ctx stdctx.Context, args json.RawMessage) (string, error)
}

type Toolset struct {
	mu sync.RWMutex
	m  map[string]Tool
}

func NewToolset() *Toolset {
	return &Toolset{m: make(map[string]Tool)}
}

// Register 登记工具，返回注销它的逆。
func (t *Toolset) Register(tool Tool) stc.Inverse {
	t.mu.Lock()
	t.m[tool.Name] = tool
	t.mu.Unlock()
	return func() error {
		t.mu.Lock()
		delete(t.m, tool.Name)
		t.mu.Unlock()
		return nil
	}
}

func (t *Toolset) Lookup(name string) (Tool, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	tool, ok := t.m[name]
	return tool, ok
}

// List 返回按名排序的当前工具视图。
func (t *Toolset) List() []Tool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Tool, 0, len(t.m))
	for _, tool := range t.m {
		out = append(out, tool)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ToolsetComponent 提供稳定的工具注册表。
func ToolsetComponent() stc.Component {
	return stc.Component{
		Name:    "toolset",
		Provide: []stc.Key{KeyTools},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			_, err := c.Provide(KeyTools, NewToolset())
			return nil, err
		},
	}
}

// component 是静态工具 fiber 的统一骨架：inject toolset，注册自己。
func component(t Tool) stc.Component {
	return stc.Component{
		Name:   "tool:" + t.Name,
		Inject: []stc.Key{KeyTools},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			ts, err := stc.Service[*Toolset](c, KeyTools)
			if err != nil {
				return nil, err
			}
			return ts.Register(t), nil
		},
	}
}
