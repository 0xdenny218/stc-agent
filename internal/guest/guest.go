// Package guest 把 .wasm 文件粘合成 agent 工具（spec D5）：
// GuestToolComponent 装载模块、读取 guest 经 tool.<name> 服务公布的
// 线格式描述、把工具注册进稳定 toolset（Invoke 转发 Handle.Call），
// 并用 hmr.Watch 监听文件——重构建即对话进行中热替换。
package guest

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
	"github.com/0xdenny218/stc-go/hmr"
	"github.com/0xdenny218/stc-go/wasm"
)

// KeyRuntime 是共享 WASM 运行时服务（稳定；全部 guest 工具 fiber 共用）。
var KeyRuntime = stc.NewKey[*wasm.Runtime]("wasm-runtime")

// RuntimeComponent 提供共享的 *wasm.Runtime。
func RuntimeComponent() stc.Component {
	return stc.Component{
		Name:    "wasm-runtime",
		Provide: []stc.Key{KeyRuntime},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			rt, err := wasm.NewRuntime()
			if err != nil {
				return nil, err
			}
			if _, err := c.Provide(KeyRuntime, rt); err != nil {
				_ = rt.Close()
				return nil, err
			}
			return rt.Close, nil
		},
	}
}

// descriptor 是 guest 在 start 内经 tool.<name> 服务公布的线格式描述。
type descriptor struct {
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// GuestToolComponent 把 path 指向的 .wasm 装载为一个工具 fiber。
// 工具名取文件名（dice.wasm → dice；guest 把描述公布在 tool.dice）。
// Invoke 转发 Handle.Call("invoke")：入参/结果都是 JSON 字符串。
// hmr.Watch 监听同一路径，重构建触发热替换；结果经 onReload(name, err)
// 上报（nil 回调则丢弃）。换血只换行为——描述在初次装载时读取一次
// （spec M3：描述 churn 不在范围内）。
func GuestToolComponent(path string, onReload func(name string, err error)) stc.Component {
	name := strings.TrimSuffix(filepath.Base(path), ".wasm")
	return stc.Component{
		Name:   "guest-tool:" + name,
		Inject: []stc.Key{KeyRuntime, tools.KeyTools},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			rt, err := stc.Service[*wasm.Runtime](c, KeyRuntime)
			if err != nil {
				return nil, err
			}
			ts, err := stc.Service[*tools.Toolset](c, tools.KeyTools)
			if err != nil {
				return nil, err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("guest tool %s: %w", name, err)
			}
			h, err := wasm.Load(stdctx.Background(), c, rt, src, wasm.Options{Name: name})
			if err != nil {
				return nil, fmt.Errorf("guest tool %s: %w", name, err)
			}
			// guest 的 start 已在 Ready 前跑完；描述服务缺席即坏 guest，
			// 装载失败（启动期 fail-fast，见 spec M3）。
			raw, err := stc.Service[string](c, rt.Key("tool."+name))
			if err != nil {
				h.Dispose()
				return nil, fmt.Errorf("guest tool %s: no tool.%s service provided: %w", name, name, err)
			}
			var d descriptor
			if err := json.Unmarshal([]byte(raw), &d); err != nil {
				h.Dispose()
				return nil, fmt.Errorf("guest tool %s: bad descriptor: %w", name, err)
			}
			unregister := ts.Register(tools.Tool{
				Name:        name,
				Description: d.Description,
				Parameters:  d.Parameters,
				Invoke: func(ctx stdctx.Context, args json.RawMessage) (string, error) {
					return h.Call(ctx, "invoke", string(args))
				},
			})
			report := func(err error) {
				if onReload != nil {
					onReload(name, err)
				}
			}
			w, err := hmr.Watch(stdctx.Background(), h, path, &hmr.Options{OnReload: report})
			if err != nil {
				_ = unregister()
				h.Dispose()
				return nil, fmt.Errorf("guest tool %s: watch: %w", name, err)
			}
			return func() error {
				werr := w.Close()
				uerr := unregister()
				h.Dispose()
				return errors.Join(werr, uerr)
			}, nil
		},
	}
}

// Components 扫描 dir 下全部 *.wasm，每个文件一个 GuestToolComponent。
// 目录不存在或没有 .wasm 时返回 nil——没有 guest 工具不是错误；
// 坏 guest 由对应 fiber 的装载失败上报（启动期 fail-fast）。
func Components(dir string, onReload func(name string, err error)) ([]stc.Component, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.wasm"))
	if err != nil {
		return nil, fmt.Errorf("scan tools dir: %w", err)
	}
	comps := make([]stc.Component, 0, len(paths))
	for _, p := range paths {
		comps = append(comps, GuestToolComponent(p, onReload))
	}
	return comps, nil
}
