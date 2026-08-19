// Package instructions 把工作目录里的 AGENTS.md 装配为 system-prompt 段落
// （spec M11）：项目指令文件已成事实标准（Codex/Gemini/opencode 等同款
// 约定）。文件缺席 = 无段落不是错误；对话中编辑/增删即时生效（段落
// 注册表同名覆盖 + 持逆再注册，监听走 stc-go/watch 原语）。
package instructions

import (
	stdctx "context"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/0xdenny218/stc-agent/internal/prompt"
	stc "github.com/0xdenny218/stc-go"
	"github.com/0xdenny218/stc-go/watch"
)

// segmentName 是 AGENTS.md 段落在 system prompt 里的名字（"15-" 排在
// 身份段之后、todos 段之前）。
const segmentName = "15-project"

// FileName 是被识别的项目指令文件名。
const FileName = "AGENTS.md"

// Component 把 dir/AGENTS.md 装为段落 fiber。dir 通常是启动目录。
func Component(dir string) stc.Component {
	return stc.Component{
		Name:   "instructions",
		Inject: []stc.Key{prompt.KeyPrompt},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			segments, err := stc.Service[*prompt.Segments](c, prompt.KeyPrompt)
			if err != nil {
				return nil, err
			}
			// 段落同步：持最新一代注销逆，再注册前先调（registry 同名
			// 覆盖 + 幂等逆，todo/skills 同款）。watcher goroutine 与
			// fiber 卸载并发调用，上锁保底（todo.state 同款）。
			var mu sync.Mutex
			var unreg stc.Inverse
			syncSeg := func() {
				mu.Lock()
				defer mu.Unlock()
				if unreg != nil {
					_ = unreg()
					unreg = nil
				}
				if b, err := os.ReadFile(filepath.Join(dir, FileName)); err == nil {
					if text := strings.TrimSpace(string(b)); text != "" {
						unreg = segments.Register(segmentName, text)
					}
				}
			}
			syncSeg()

			// 监听目录（watch 原语要求路径存在，目录必在）：过滤到
			// AGENTS.md 的增删改，防抖后重新同步（stat 定终态）。
			w, err := watch.Watch(stdctx.Background(), dir, watch.Options{
				OnFire: func(ev watch.Event) {
					if filepath.Base(ev.Path) == FileName {
						syncSeg()
					}
				},
			})
			if err != nil {
				if unreg != nil {
					_ = unreg()
				}
				return nil, err
			}
			return func() error {
				werr := w.Close()
				mu.Lock()
				if unreg != nil {
					_ = unreg()
					unreg = nil
				}
				mu.Unlock()
				return werr
			}, nil
		},
	}
}
