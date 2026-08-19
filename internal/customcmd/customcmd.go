// Package customcmd 把 commands.d/*.md 热装载为用户斜杠命令 fiber
// （spec M11）：每个 <name>.md 一个命令——可选 frontmatter（description）
// 加正文作为 prompt 模板，$ARGUMENTS 占位符替换为命令参数（无占位符则
// 参数追加在末尾）。supervisor 监听目录，按签名（size+mtime）差分：
// 新增即装、删除即卸、修改即重载，全程不重启。命令执行 = 以模板为输入
// 跑一轮 RunTurn——用户命令与模型能力在同一循环里会师。
package customcmd

import (
	stdctx "context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/0xdenny218/stc-agent/internal/cli"
	"github.com/0xdenny218/stc-agent/internal/loop"
	stc "github.com/0xdenny218/stc-go"
	"github.com/0xdenny218/stc-go/watch"
)

// Command 是一份 <name>.md 的解析结果：正文即 prompt 模板。
type Command struct {
	Name        string
	Description string // frontmatter（可空，/help 只列名，留作展示扩展）
	Body        string
}

// Parse 解析命令文件：可选 frontmatter（"---" 夹住的 key: value 行，只认
// description）加正文；正文为空是错误。
func Parse(b []byte, name string) (Command, error) {
	c := Command{Name: name}
	text := string(b)
	if rest, ok := strings.CutPrefix(text, "---\n"); ok {
		end := strings.Index(rest, "\n---")
		if end < 0 {
			return c, fmt.Errorf("customcmd %s: frontmatter not closed (missing ---)", name)
		}
		for _, line := range strings.Split(rest[:end], "\n") {
			if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "description" {
				c.Description = strings.TrimSpace(v)
			}
		}
		text = rest[end+len("\n---"):]
	}
	c.Body = strings.TrimSpace(text)
	if c.Body == "" {
		return c, fmt.Errorf("customcmd %s: empty body", name)
	}
	return c, nil
}

// Component 把一个命令文件装为命令 fiber：注册进斜杠命令注册表，
// 执行即"以模板为输入跑一轮"。
func Component(path string) stc.Component {
	name := strings.TrimSuffix(filepath.Base(path), ".md")
	return stc.Component{
		Name:   "cmd:user:" + name,
		Inject: []stc.Key{cli.KeyCommands, loop.KeyRunner},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			reg, err := stc.Service[*cli.Registry](c, cli.KeyCommands)
			if err != nil {
				return nil, err
			}
			r, err := stc.Service[loop.Runner](c, loop.KeyRunner)
			if err != nil {
				return nil, err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("customcmd %s: %w", name, err)
			}
			cmd, err := Parse(b, name)
			if err != nil {
				return nil, err
			}
			return reg.Register(name, func(ctx stdctx.Context, w io.Writer, args string) error {
				prompt := cmd.Body
				if strings.Contains(prompt, "$ARGUMENTS") {
					prompt = strings.ReplaceAll(prompt, "$ARGUMENTS", args)
				} else if args != "" {
					prompt = prompt + "\n" + args
				}
				return r.RunTurn(ctx, prompt, w)
			}), nil
		},
	}
}

// SupervisorComponent 监听 commands-dir：*.md 出现即装载、消失即撤退、
// 修改（签名变化）即重载。root 用于装载；onError 上报失败；onChange 在
// 装载/撤退后上报。启动期坏命令 fail-fast（tools.d/skills.d 惯例）。
func SupervisorComponent(root *stc.Context, dir string,
	onError func(name string, err error), onChange func(name string, loaded bool)) stc.Component {
	return stc.Component{
		Name: "customcmds",
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("commands dir: %w", err)
			}
			s := &supervisor{
				root: root, dir: dir,
				onError: onError, onChange: onChange,
				loaded: map[string]*entry{},
			}
			w, err := watch.Watch(stdctx.Background(), dir, watch.Options{
				OnFire: func(watch.Event) { s.rescan() },
			})
			if err != nil {
				return nil, fmt.Errorf("commands watch: %w", err)
			}
			s.w = w
			// 启动期 fail-fast。
			for name := range s.scan() {
				if err := s.load(name, true); err != nil {
					_ = w.Close()
					return nil, err
				}
			}
			return s.close, nil
		},
	}
}

type entry struct {
	fib *stc.Fiber
	sig string
}

type supervisor struct {
	root     *stc.Context
	dir      string
	onError  func(string, error)
	onChange func(string, bool)
	w        *watch.Watcher

	mu     sync.Mutex
	loaded map[string]*entry
}

// scan 返回当前 *.md 的名字 → 签名（size+mtime）。
func (s *supervisor) scan() map[string]string {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		if st, err := e.Info(); err == nil {
			out[strings.TrimSuffix(e.Name(), ".md")] = fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano())
		}
	}
	return out
}

// rescan 差分：新增装载、消失撤退、签名变化先退再装。与 skills 的全量
// 名字 diff 不同，这里靠签名把"原地修改"也纳入差分——命令是单文件，
// 无需 per-fiber 监听。
func (s *supervisor) rescan() {
	current := s.scan()
	s.mu.Lock()
	var added, changed, gone []string
	for name, sig := range current {
		e, ok := s.loaded[name]
		switch {
		case !ok:
			added = append(added, name)
		case e.sig != sig:
			changed = append(changed, name)
		}
	}
	for name := range s.loaded {
		if _, ok := current[name]; !ok {
			gone = append(gone, name)
		}
	}
	s.mu.Unlock()
	for _, name := range changed {
		s.remove(name)
		_ = s.load(name, false)
	}
	for _, name := range added {
		_ = s.load(name, false)
	}
	for _, name := range gone {
		s.remove(name)
	}
}

// load 装载一个命令 fiber；boot=true 同步等 Ready（fail-fast），否则异步。
func (s *supervisor) load(name string, boot bool) error {
	path := filepath.Join(s.dir, name+".md")
	sig := ""
	if st, err := os.Stat(path); err == nil {
		sig = fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano())
	}
	s.mu.Lock()
	if _, dup := s.loaded[name]; dup {
		s.mu.Unlock()
		return nil
	}
	f := s.root.Load(Component(path))
	s.loaded[name] = &entry{fib: f, sig: sig}
	s.mu.Unlock()

	if !boot {
		go func() {
			ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 10*time.Second)
			defer cancel()
			if err := f.Ready(ctx); err != nil {
				if s.onError != nil {
					s.onError(name, err)
				}
				s.remove(name)
				return
			}
			if s.onChange != nil {
				s.onChange(name, true)
			}
		}()
		return nil
	}
	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 10*time.Second)
	defer cancel()
	if err := f.Ready(ctx); err != nil {
		s.remove(name)
		return fmt.Errorf("customcmd %s: %w", name, err)
	}
	if s.onChange != nil {
		s.onChange(name, true)
	}
	return nil
}

// remove 撤退一个命令 fiber（Dispose → Gone → 摘表）。幂等。
func (s *supervisor) remove(name string) {
	s.mu.Lock()
	e, ok := s.loaded[name]
	if ok {
		delete(s.loaded, name)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	e.fib.Dispose()
	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()
	_ = e.fib.Gone(ctx)
	if s.onChange != nil {
		s.onChange(name, false)
	}
}

func (s *supervisor) close() error {
	s.mu.Lock()
	names := make([]string, 0, len(s.loaded))
	for name := range s.loaded {
		names = append(names, name)
	}
	s.mu.Unlock()
	for _, name := range names {
		s.remove(name)
	}
	return s.w.Close()
}
