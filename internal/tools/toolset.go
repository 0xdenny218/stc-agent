// Package tools implements the static Go tool fibers. Registration is a
// revertible effect (spec D3): a tool fiber registers itself into the
// stable toolset, so adding/removing tools never reloads the agent loop —
// the loop reads the current tool list per turn.
//
// The toolset itself is stc-go's registry satellite (stc-go v0.4.0, #2);
// this package keeps only the domain type Tool and the member-fiber
// skeleton.
package tools

import (
	stdctx "context"
	"encoding/json"

	stc "github.com/0xdenny218/stc-go"
	"github.com/0xdenny218/stc-go/registry"
)

// KeyTools 是工具注册表服务（稳定；inject 为空，不随级联重载）。
var KeyTools = stc.NewKey[*Toolset]("tools")

// Toolset 是工具的稳定注册表（stc-go/registry 的实例化）。
type Toolset = registry.Registry[Tool]

// Tool 是一个可被模型调用的能力：线格式描述 + 调用闭包。
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON Schema
	Invoke      func(ctx stdctx.Context, args json.RawMessage) (string, error)
	// Preview 可选：把参数渲染成人读的变更预览（统一 diff 等），供审批
	// 门在询问前展示——用户按 diff 决定 y/n，而不是对着参数 JSON 猜。
	// 返回空串 = 无预览（门回退到参数展示）。出错也应返回带说明的文本
	// 而不是空串之外的特殊值。
	Preview func(args json.RawMessage) string
}

// ToolsetComponent 提供稳定的工具注册表。
func ToolsetComponent() stc.Component {
	return registry.Component[Tool]("toolset", KeyTools)
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
			return ts.Register(t.Name, t), nil
		},
	}
}
