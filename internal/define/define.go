// Package define implements the authored-guest-tool capability (spec M10):
// a define_guest tool lets the model write a guest tool's Go source, which
// the host compiles with TinyGo into wasm and loads as a regular guest tool
// via guest.Load — the same path skills use, so hmr hot reload, the approval
// gate, and failure rollback all come for free. Compile or load failure
// removes the wasm and leaves no residue in the toolset.
package define

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/0xdenny218/stc-agent/internal/guest"
	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
)

// Options 配置 define_guest。
type Options struct {
	ToolsDir string                       // 源码与产物的目录（写 <name>.go，产出 <name>.wasm）
	TinyGo   string                       // tinygo 可执行路径（默认 "tinygo"）
	Timeout  time.Duration                // 编译超时（默认 60s）
	OnReload func(name string, err error) // 重载上报（透传 guest.Load）
}

// withDefaults 填充零值选项。
func (o Options) withDefaults() Options {
	if o.ToolsDir == "" {
		o.ToolsDir = "tools.d"
	}
	if o.TinyGo == "" {
		o.TinyGo = "tinygo"
	}
	if o.Timeout <= 0 {
		o.Timeout = 60 * time.Second
	}
	return o
}

// loader 持有 fiber 周期 context（供 guest.Load 解析运行时/工具表）与
// 已装载 guest 的逆。工具 Invoke 在循环 goroutine 串行执行，互斥锁只为
// fiber 卸载（回卷全部逆）与 invoke 可能交错时保底。
type loader struct {
	opts   Options
	c      *stc.Context
	mu     sync.Mutex
	guests map[string]stc.Inverse // name → guest.Load 逆（fiber 卸载时回卷）
}

// Component 是 define_guest fiber。
func Component(opts Options) stc.Component {
	o := opts.withDefaults()
	return stc.Component{
		Name:   "tool:define_guest",
		Inject: []stc.Key{guest.KeyRuntime, tools.KeyTools},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			ts, err := stc.Service[*tools.Toolset](c, tools.KeyTools)
			if err != nil {
				return nil, err
			}
			ld := &loader{opts: o, c: c, guests: map[string]stc.Inverse{}}
			untool := ts.Register("define_guest", tools.Tool{
				Name:        "define_guest",
				Description: "Define a new tool by writing its Go source. The host compiles it with TinyGo to wasm and loads it as a regular tool. Source contract: package main; import github.com/0xdenny218/stc-go/guest; in init() call guest.OnInvoke(func(args string) string); export func start() which calls guest.Provide(\"tool.<name>\", <json descriptor>). The descriptor is {\"description\": \"...\", \"parameters\": <JSON Schema object>} — the key is \"parameters\", not \"inputSchema\". Re-defining a name replaces the existing tool.",
				Parameters: json.RawMessage(`{"type":"object","properties":{
  "name": {"type":"string","description":"tool name; a single path segment, no separators"},
  "source": {"type":"string","description":"complete Go source of the guest tool"}
},"required":["name","source"]}`),
				Invoke: ld.invoke,
			})
			return func() error {
				var errs []error
				ld.mu.Lock()
				for name, g := range ld.guests {
					if err := g(); err != nil {
						errs = append(errs, fmt.Errorf("define_guest: unload %s: %w", name, err))
					}
				}
				ld.guests = map[string]stc.Inverse{}
				ld.mu.Unlock()
				_ = untool()
				return errors.Join(errs...)
			}, nil
		},
	}
}

// invoke 是 define_guest 的执行体：校验 → 回卷同名旧客 → 写源码 →
// 编译 → 装载（guest.Load 注册工具 + hmr 监听）。失败只删 wasm，
// toolset 不留残项。
func (ld *loader) invoke(_ stdctx.Context, args json.RawMessage) (string, error) {
	var a struct {
		Name   string `json:"name"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("define_guest: bad arguments: %w", err)
	}
	name := strings.TrimSpace(a.Name)
	if name == "" || strings.TrimSpace(a.Source) == "" {
		return "", errors.New("define_guest: name and source are required")
	}
	if name == "." || name == ".." || filepath.Base(name) != name {
		return "", fmt.Errorf("define_guest: invalid name %q (must be a single path segment)", name)
	}

	ld.mu.Lock()
	if old, ok := ld.guests[name]; ok {
		if err := old(); err != nil {
			ld.mu.Unlock()
			return "", fmt.Errorf("define_guest: unload old %s: %w", name, err)
		}
		delete(ld.guests, name)
	}
	ld.mu.Unlock()

	if err := os.MkdirAll(ld.opts.ToolsDir, 0o755); err != nil {
		return "", err
	}
	srcPath := filepath.Join(ld.opts.ToolsDir, name+".go")
	wasmPath := filepath.Join(ld.opts.ToolsDir, name+".wasm")
	if err := os.WriteFile(srcPath, []byte(a.Source), 0o644); err != nil {
		return "", err
	}
	if err := ld.build(wasmPath, srcPath); err != nil {
		_ = os.Remove(wasmPath)
		return "", fmt.Errorf("define_guest: compile %s: %w", name, err)
	}
	inv, err := guest.Load(ld.c, wasmPath, ld.opts.OnReload)
	if err != nil {
		_ = os.Remove(wasmPath)
		return "", fmt.Errorf("define_guest: load %s: %w", name, err)
	}
	ld.mu.Lock()
	ld.guests[name] = inv
	ld.mu.Unlock()
	return fmt.Sprintf("guest tool %q defined and loaded (source kept at %s)", name, srcPath), nil
}

// build 用 TinyGo 把 guest 源码编译成 wasm。工作目录 = 模块根（从当前
// 目录向上找 go.mod），保证源码对 stc-go guest SDK 的 import 能被模块
// 解析（版本随宿主 go.mod 锁定）。编译失败返回 stderr 末尾摘录。
func (ld *loader) build(wasmPath, srcPath string) error {
	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), ld.opts.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, ld.opts.TinyGo,
		"build", "-target", "wasip1", "-buildmode=c-shared", "-o", wasmPath, srcPath)
	root, err := findModuleRoot()
	if err != nil {
		return err
	}
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 1000 {
			msg = msg[len(msg)-1000:]
		}
		if msg == "" {
			return err
		}
		return fmt.Errorf("tinygo failed: %v\n%s", err, msg)
	}
	return nil
}

// findModuleRoot 从当前工作目录向上找 go.mod。
func findModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("define_guest: no go.mod found (run stc-agent from within the module)")
		}
		dir = parent
	}
}
