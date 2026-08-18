// Package inspect 是 agent 自描述（spec D16）：fiber 目录 + inspect_agent
// 工具——模型可以反问"我现在有哪些 fiber、什么状态、哪些工具"，对齐
// dsh cordis_inspect_*，但这里 fiber 状态与工具目录都是范式原生信息。
//
// fiber 目录自 stc-go v0.5.0 起直接读树上的注册表快照（(*Context).Fibers()，
// stc-go#4）：在册即枚举、出册即消失，不再有手工登记面——define_guest 的
// invoke 期装载等一切生命周期路径天然可见，目录与树不再有可漂移的副本。
package inspect

import (
	stdctx "context"
	"encoding/json"
	"sort"

	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
)

// FiberInfo 是一个 fiber 的自描述快照。
type FiberInfo struct {
	ID    uint64 `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

// Directory 是 fiber 目录（稳定服务）：stc-go 注册表的只读视图适配，
// 快照经 (*Context).Fibers() 现读——登记语义归注册表本身，这里只做
// 名字排序与 JSON 形态。
type Directory struct {
	tree *stc.Context // 本树任意 context 皆可（Fibers 按树观察）
}

// NewDirectory 绑定要观察的树（root 或其子作用域等效）。
func NewDirectory(tree *stc.Context) *Directory { return &Directory{tree: tree} }

// Snapshot 按名字排序返回当前快照（状态现读）。
func (d *Directory) Snapshot() []FiberInfo {
	fs := d.tree.Fibers()
	out := make([]FiberInfo, 0, len(fs))
	for _, f := range fs {
		out = append(out, FiberInfo{ID: f.ID(), Name: f.Name(), State: f.State().String()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// KeyDirectory 是 fiber 目录服务键。
var KeyDirectory = stc.NewKey[*Directory]("inspect-dir")

// DirectoryComponent 提供稳定的 fiber 目录（Inject 为空）。目录在其
// fiber 自己的 context 上观察树：同一棵树的注册表视图处处相同。
func DirectoryComponent() stc.Component {
	return stc.Component{
		Name:    "inspect-dir",
		Provide: []stc.Key{KeyDirectory},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			_, err := c.Provide(KeyDirectory, NewDirectory(c))
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
