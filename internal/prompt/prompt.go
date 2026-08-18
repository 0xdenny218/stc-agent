// Package prompt 是 system-prompt 段落注册表（spec D16）：段落 fiber
// 注册文本段是可逆效应——装载即插入、卸载即摘除，组装结果随 fiber
// 增删反应式变化，消费方 fiber（agent 循环）不因段落增删而重载。
// 顺序按段落名排序（注册表名字序，稳定；需要控制次序时用名字前缀
// 如 "10-identity"）。
package prompt

import (
	"strings"

	stc "github.com/0xdenny218/stc-go"
	"github.com/0xdenny218/stc-go/registry"
)

// Segments 是段落稳定注册表（stc-go/registry 的实例化）：键是段落名，
// 值是段落文本。
type Segments = registry.Registry[string]

// KeyPrompt 是段落注册表服务键。
var KeyPrompt = stc.NewKey[*Segments]("prompt")

// Component 提供稳定的段落注册表（Inject 为空：成员增删不重载消费方
// fiber）。
func Component() stc.Component {
	return registry.Component[string]("prompt", KeyPrompt)
}

// SegmentComponent 是段落 fiber 的骨架：inject 注册表并注册一段文本
// （注册即效应，卸载自动摘除）。
func SegmentComponent(name, text string) stc.Component {
	return stc.Component{
		Name:   "prompt:" + name,
		Inject: []stc.Key{KeyPrompt},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			reg, err := stc.Service[*Segments](c, KeyPrompt)
			if err != nil {
				return nil, err
			}
			return reg.Register(name, text), nil
		},
	}
}

// Assemble 按段落名序拼接全部段落（空注册表返回空串——不发 system
// 消息）。
func Assemble(reg *Segments) string {
	return strings.Join(reg.List(), "\n\n")
}
