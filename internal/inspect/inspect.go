// Package inspect 是 agent 自描述（spec D16）：fiber 目录 + inspect_agent
// 工具——模型可以反问"我现在有哪些 fiber、什么状态、哪些工具"，对齐
// dsh cordis_inspect_*，但这里fiber 状态与工具目录都是范式原生信息。
//
// 目录是装配方显式登记的活句柄表（main 登记启动 fibers，config.Control
// 在换血时换登记）；stc-go 内部本有全量 fiber 表（shared.registry）
// 但无公开枚举 API——这层重复是已知摩擦，回流议题见 stc-go 侧记录。
package inspect

import (
	stdctx "context"
	"encoding/json"
	"sort"
	"sync"

	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
)

// FiberInfo 是一个 fiber 的自描述快照。
type FiberInfo struct {
	ID    uint64 `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

// Directory 是 fiber 目录（稳定服务）：登记的只是指针，快照时状态现读，
// 因此级联重载（同一句柄的周期更替）天然反映最新状态。
type Directory struct {
	mu sync.RWMutex
	fs map[uint64]*stc.Fiber
}

func NewDirectory() *Directory {
	return &Directory{fs: make(map[uint64]*stc.Fiber)}
}

// Register 登记一个 fiber 句柄；返回的逆操作把它摘除（fiber 被撤退时
// 调用方负责摘除，保持目录与树一致）。
func (d *Directory) Register(f *stc.Fiber) stc.Inverse {
	d.mu.Lock()
	d.fs[f.ID()] = f
	d.mu.Unlock()
	return func() error {
		d.mu.Lock()
		delete(d.fs, f.ID())
		d.mu.Unlock()
		return nil
	}
}

// Snapshot 按名字排序返回当前快照（状态现读）。
func (d *Directory) Snapshot() []FiberInfo {
	d.mu.RLock()
	fs := make([]*stc.Fiber, 0, len(d.fs))
	for _, f := range d.fs {
		fs = append(fs, f)
	}
	d.mu.RUnlock()
	out := make([]FiberInfo, 0, len(fs))
	for _, f := range fs {
		out = append(out, FiberInfo{ID: f.ID(), Name: f.Name(), State: f.State().String()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// KeyDirectory 是 fiber 目录服务键。
var KeyDirectory = stc.NewKey[*Directory]("inspect-dir")

// DirectoryComponent 提供稳定的 fiber 目录（Inject 为空）。
func DirectoryComponent() stc.Component {
	return stc.Component{
		Name:    "inspect-dir",
		Provide: []stc.Key{KeyDirectory},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			_, err := c.Provide(KeyDirectory, NewDirectory())
			return nil, err
		},
	}
}

// toolInfo 是工具目录里的一条。
type toolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ToolComponent 是 inspect_agent 工具 fiber：报告 fiber 状态与工具目录
// （JSON）。只读，默认策略放行。
func ToolComponent() stc.Component {
	tool := tools.Tool{
		Name:        "inspect_agent",
		Description: "Inspect the agent itself: live fiber states and the current tool catalog",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	}
	return stc.Component{
		Name:   "tool:" + tool.Name,
		Inject: []stc.Key{tools.KeyTools, KeyDirectory},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			ts, err := stc.Service[*tools.Toolset](c, tools.KeyTools)
			if err != nil {
				return nil, err
			}
			dir, err := stc.Service[*Directory](c, KeyDirectory)
			if err != nil {
				return nil, err
			}
			tool.Invoke = func(stdctx.Context, json.RawMessage) (string, error) {
				var tls []toolInfo
				for _, t := range ts.List() {
					tls = append(tls, toolInfo{Name: t.Name, Description: t.Description})
				}
				report := struct {
					Fibers []FiberInfo `json:"fibers"`
					Tools  []toolInfo  `json:"tools"`
				}{Fibers: dir.Snapshot(), Tools: tls}
				b, err := json.MarshalIndent(report, "", "  ")
				if err != nil {
					return "", err
				}
				return string(b), nil
			}
			return ts.Register(tool.Name, tool), nil
		},
	}
}
