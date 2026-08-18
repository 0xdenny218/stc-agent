// Package hooks 是生命周期事件域（spec D16）：fiber 注册监听/拦截都是
// 可逆效应。两类事件各用最合适的已有机制，不长第三套派发：
//
//   - 通知型（agent/*、tools/post-*）：走 stc 核心 On/Emit——fiber 在
//     Apply 里 Listen，回卷自动撤销；派发串行、尽力而为，永远不影响
//     轮次结果。
//   - 拦截型（tools/pre-execute）：走 registry 卫星——Interceptor 注册
//     进稳定注册表（成员增删不重载消费方 fiber）；Check 按注册表序
//     串行询问，第一个非 nil 错误即 bail：执行被阻断，错误文本作为
//     原因回灌模型。
package hooks

import (
	stdctx "context"

	stc "github.com/0xdenny218/stc-go"
	"github.com/0xdenny218/stc-go/registry"
)

// 事件域。
const (
	ToolPreExecute  = "tools/pre-execute"  // 拦截型
	ToolPostExecute = "tools/post-execute" // 通知型
	TurnStart       = "agent/turn-start"   // 通知型
	TurnEnd         = "agent/turn-end"     // 通知型
)

// Payload 是事件负载：tools/* 填 Tool/Arguments（post-execute 另有
// Result）；agent/* 填 Text（轮次输入或最终答复）。
type Payload struct {
	Tool      string
	Arguments string
	Result    string
	Text      string
}

// Interceptor 是拦截型 hook：Check 返回非 nil 即 bail。
type Interceptor struct {
	Event string
	Check func(stdctx.Context, Payload) error
}

// Interceptors 是拦截 hook 的稳定注册表（stc-go/registry 的实例化）。
type Interceptors = registry.Registry[Interceptor]

// KeyHooks 是拦截 hook 注册表服务键。
var KeyHooks = stc.NewKey[*Interceptors]("hooks")

// Component 提供稳定的拦截 hook 注册表（Inject 为空：成员增删不重载
// 消费方 fiber）。
func Component() stc.Component {
	return registry.Component[Interceptor]("hooks", KeyHooks)
}

// InterceptorComponent 是拦截 hook fiber 的骨架：inject 注册表并注册
// 自己（注册即效应，卸载自动摘除）。
func InterceptorComponent(name string, i Interceptor) stc.Component {
	return stc.Component{
		Name:   "hook:" + name,
		Inject: []stc.Key{KeyHooks},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			reg, err := stc.Service[*Interceptors](c, KeyHooks)
			if err != nil {
				return nil, err
			}
			return reg.Register(name, i), nil
		},
	}
}

// Check 串行派发拦截型事件：按注册表序（名字排序，稳定）逐个询问，
// 第一个非 nil 错误即 bail。
func Check(ctx stdctx.Context, reg *Interceptors, event string, p Payload) error {
	for _, i := range reg.List() {
		if i.Event != event {
			continue
		}
		if err := i.Check(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// Emit 派发通知型事件：串行、尽力而为；监听者阻塞会拖累派发方，
// 监听应轻。
func Emit(c *stc.Context, event string, p Payload) {
	c.Emit(event, p)
}

// Listen 注册通知型监听（在 fiber Apply 里调用；回卷自动撤销）。
func Listen(c *stc.Context, event string, fn func(Payload)) error {
	return c.On(event, func(args ...any) {
		if len(args) == 1 {
			if p, ok := args[0].(Payload); ok {
				fn(p)
			}
		}
	})
}
