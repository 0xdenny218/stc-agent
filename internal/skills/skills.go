// Package skills 把 skills-dir 的目录约定粘合成热装载 fiber（spec M8）：
// 每个含 SKILL.md 的子目录是一个 skill fiber——SKILL.md 正文注册为
// prompt 段落，目录里的 *.wasm 经 guest.Load 装成工具子集；supervisor
// fiber 监听目录增减：落盘即装、删除即卸，全程不重启（吃 hmr 红利：
// wasm 原地重建仍由 guest.Load 内的 hmr.Watch 热替换）。
package skills

import (
	stdctx "context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/0xdenny218/stc-agent/internal/guest"
	"github.com/0xdenny218/stc-agent/internal/prompt"
	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
)

// Skill 是一份 SKILL.md 的解析结果。
type Skill struct {
	Name        string // frontmatter name；缺省取目录名
	Description string // frontmatter description（可空）
	Body        string // frontmatter 之后的正文 → prompt 段落
}

// Parse 解析 SKILL.md：可选 frontmatter（"---" 行夹住的 key: value 行，
// 只认 name/description，未知键忽略）加正文。defName 是 name 缺省时的
// 回退（目录名）。正文为空是错误——没有内容的 skill 无意义。
func Parse(b []byte, defName string) (Skill, error) {
	s := Skill{Name: defName}
	text := string(b)
	if rest, ok := strings.CutPrefix(text, "---\n"); ok {
		end := strings.Index(rest, "\n---")
		if end < 0 {
			return s, errors.New("skills: frontmatter not closed (missing ---)")
		}
		for _, line := range strings.Split(rest[:end], "\n") {
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			switch strings.TrimSpace(k) {
			case "name":
				if n := strings.TrimSpace(v); n != "" {
					s.Name = n
				}
			case "description":
				s.Description = strings.TrimSpace(v)
			}
		}
		text = rest[end+len("\n---"):]
	}
	s.Body = strings.TrimSpace(text)
	if s.Body == "" {
		return s, errors.New("skills: empty skill body")
	}
	return s, nil
}

// Component 把 dir（含 SKILL.md 的目录）装成一个 skill fiber。SKILL.md
// 正文注册为 prompt 段落（注册名 "skill:"+<fiber 名>；registry 卫星的
// 同名覆盖语义让编辑热生效）；目录里的 *.wasm 经 guest.Load 装成工具
// 子集。SKILL.md 被编辑 → 重解析覆盖段落（解析失败保留旧段落，经
// onError 上报）；被删除/改名消失 → onGone 回调（supervisor 据此撤退
// 本 fiber）。onError / onGone 皆可 nil。
func Component(dir string, onError func(name string, err error), onGone func(name string)) stc.Component {
	name := filepath.Base(dir)
	return stc.Component{
		Name:   "skill:" + name,
		Inject: []stc.Key{prompt.KeyPrompt, guest.KeyRuntime, tools.KeyTools},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			segments, err := stc.Service[*prompt.Segments](c, prompt.KeyPrompt)
			if err != nil {
				return nil, err
			}
			mdPath := filepath.Join(dir, "SKILL.md")
			b, err := os.ReadFile(mdPath)
			if err != nil {
				return nil, fmt.Errorf("skill %s: %w", name, err)
			}
			sk, err := Parse(b, name)
			if err != nil {
				return nil, fmt.Errorf("skill %s: %w", name, err)
			}
			// 段落效应：只保留最新一代的注销逆（registry 同名覆盖后
			// 旧逆已失效，调用无害但留它没意义）。
			unregisterSegment := segments.Register("skill:"+name, sk.Body)

			var inverses []stc.Inverse
			fail := func(err error) (stc.Inverse, error) {
				_ = unregisterSegment()
				for _, inv := range inverses {
					_ = inv()
				}
				return nil, err
			}
			wasms, err := filepath.Glob(filepath.Join(dir, "*.wasm"))
			if err != nil {
				return fail(fmt.Errorf("skill %s: %w", name, err))
			}
			for _, p := range wasms {
				inv, err := guest.Load(c, p, func(tool string, err error) {
					if onError != nil && err != nil {
						onError(name, fmt.Errorf("tool %s reload: %w", tool, err))
					}
				})
				if err != nil {
					return fail(fmt.Errorf("skill %s: %w", name, err))
				}
				inverses = append(inverses, inv)
			}

			fw, err := fsnotify.NewWatcher()
			if err != nil {
				return fail(fmt.Errorf("skill %s: watch: %w", name, err))
			}
			if err := fw.Add(dir); err != nil {
				_ = fw.Close()
				return fail(fmt.Errorf("skill %s: watch: %w", name, err))
			}
			done := make(chan struct{})
			go watchSkill(dir, fw, done, func() { // 编辑热生效
				b, err := os.ReadFile(mdPath)
				if err != nil {
					if onError != nil {
						onError(name, err)
					}
					return
				}
				sk, err := Parse(b, name)
				if err != nil {
					if onError != nil {
						onError(name, err)
					}
					return
				}
				unregisterSegment = segments.Register("skill:"+name, sk.Body)
			}, func() { // SKILL.md 消失 → 交给 supervisor 撤退
				if onGone != nil {
					onGone(name)
				}
			})

			return func() error {
				close(done)
				werr := fw.Close()
				_ = unregisterSegment()
				var errs []error
				for _, inv := range inverses {
					errs = append(errs, inv())
				}
				return errors.Join(append([]error{werr}, errs...)...)
			}, nil
		},
	}
}

// watchSkill 监听 skill 目录里 SKILL.md 的变化（防抖 200ms）：存在即
// reload，消失即 gone。原子保存（临时文件 + rename）落地为 rename 后
// 的 create，以事件后 stat 的终态为准，两种形态都安全。
func watchSkill(dir string, fw *fsnotify.Watcher, done chan struct{}, reload, gone func()) {
	const debounce = 200 * time.Millisecond
	var timer *time.Timer
	var timerC <-chan time.Time
	fire := func() {
		if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
			gone()
		} else {
			reload()
		}
	}
	for {
		select {
		case <-done:
			if timer != nil {
				timer.Stop()
			}
			return
		case ev, ok := <-fw.Events:
			if !ok {
				return
			}
			if filepath.Base(ev.Name) != "SKILL.md" {
				continue
			}
			if timer != nil {
				timer.Stop()
			}
			timer = time.NewTimer(debounce)
			timerC = timer.C
		case _, ok := <-fw.Errors:
			if !ok {
				return
			}
		case <-timerC:
			timerC = nil
			fire()
		}
	}
}

// SupervisorComponent 监听 skills-dir：含 SKILL.md 的子目录出现即装载
// skill fiber，消失（目录删除 / SKILL.md 删除）即撤退。root 用于装载
// ——与 config.Control 同构：动态 fiber 的生死由 supervisor 显式管理，
// 不靠级联。skill fiber 经注册表枚举天然对 inspect 可见（stc-go#4）；
// onError 上报运行期装载失败（坏 skill 不拖垮 supervisor，nil 丢弃）；
// onChange 在 skill 装载完成/撤退后上报（nil 丢弃）。
func SupervisorComponent(root *stc.Context, dir string, onError func(name string, err error), onChange func(name string, loaded bool)) stc.Component {
	return stc.Component{
		Name: "skills",
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("skills dir: %w", err)
			}
			fw, err := fsnotify.NewWatcher()
			if err != nil {
				return nil, fmt.Errorf("skills watch: %w", err)
			}
			if err := fw.Add(dir); err != nil {
				_ = fw.Close()
				return nil, fmt.Errorf("skills watch: %w", err)
			}
			s := &supervisor{
				root:     root,
				dir:      dir,
				onError:  onError,
				onChange: onChange,
				loaded:   map[string]*skillEntry{},
				gone:     make(chan string, 8),
				done:     make(chan struct{}),
			}
			// 启动期坏 skill 遵循 tools.d 惯例：fail-fast。
			for _, name := range s.scan() {
				if err := s.load(name, true); err != nil {
					_ = fw.Close()
					return nil, err
				}
			}
			go s.loop(fw)
			return s.close, nil
		},
	}
}

type skillEntry struct {
	fib   *stc.Fiber
	ready bool // 装载完成（onChange(true) 已报）
}

type supervisor struct {
	root     *stc.Context
	dir      string
	onError  func(string, error)
	onChange func(string, bool)

	mu     sync.Mutex
	loaded map[string]*skillEntry
	gone   chan string // skill fiber 上报"SKILL.md 消失"
	done   chan struct{}
}

// scan 返回当前含 SKILL.md 的子目录名集合。
func (s *supervisor) scan() []string {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if st, err := os.Stat(filepath.Join(s.dir, e.Name(), "SKILL.md")); err == nil && !st.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

// load 装载一个 skill fiber 并登记。boot 为 true 时同步等 Ready
// （fail-fast）；否则异步等——运行期坏 skill 只上报，不拖垮别人。
func (s *supervisor) load(name string, boot bool) error {
	s.mu.Lock()
	if _, dup := s.loaded[name]; dup {
		s.mu.Unlock()
		return nil
	}
	f := s.root.Load(Component(filepath.Join(s.dir, name), s.onError, func(n string) {
		// skill fiber 自己的 watcher 发现 SKILL.md 消失：经 gone 通道
		// 汇流进 supervisor 主循环，与目录删除走同一条撤退路径。
		select {
		case s.gone <- n:
		case <-s.done:
		}
	}))
	e := &skillEntry{fib: f}
	s.loaded[name] = e
	s.mu.Unlock()

	if boot {
		ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 15*time.Second)
		defer cancel()
		if err := f.Ready(ctx); err != nil {
			s.remove(name)
			return fmt.Errorf("skill %s: %w", name, err)
		}
		s.mu.Lock()
		e.ready = true
		s.mu.Unlock()
		if s.onChange != nil {
			s.onChange(name, true)
		}
		return nil
	}
	go func() {
		ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 15*time.Second)
		defer cancel()
		if err := f.Ready(ctx); err != nil {
			if s.onError != nil {
				s.onError(name, err)
			}
			s.remove(name)
			return
		}
		// 就绪标记只认仍在册的 entry（Ready 期间可能已被 remove）。
		s.mu.Lock()
		if cur, ok := s.loaded[name]; ok && cur == e {
			e.ready = true
			s.mu.Unlock()
			if s.onChange != nil {
				s.onChange(name, true)
			}
			return
		}
		s.mu.Unlock()
	}()
	return nil
}

// remove 撤退一个 skill fiber：Dispose → Gone。幂等。
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
	if e.ready && s.onChange != nil {
		s.onChange(name, false)
	}
}

// loop 事件主循环：目录事件防抖后全量扫描做 diff（增装、退卸），
// 外加 skill fiber 自报的 gone。防抖合并原子保存/批量操作的连发。
func (s *supervisor) loop(fw *fsnotify.Watcher) {
	const debounce = 200 * time.Millisecond
	var timer *time.Timer
	var timerC <-chan time.Time
	rescan := func() {
		current := map[string]bool{}
		for _, name := range s.scan() {
			current[name] = true
			_ = s.load(name, false)
		}
		s.mu.Lock()
		var stale []string
		for name := range s.loaded {
			if !current[name] {
				stale = append(stale, name)
			}
		}
		s.mu.Unlock()
		for _, name := range stale {
			s.remove(name)
		}
	}
	for {
		select {
		case <-s.done:
			if timer != nil {
				timer.Stop()
			}
			return
		case name := <-s.gone:
			s.remove(name)
		case _, ok := <-fw.Events:
			if !ok {
				return
			}
			if timer != nil {
				timer.Stop()
			}
			timer = time.NewTimer(debounce)
			timerC = timer.C
		case _, ok := <-fw.Errors:
			if !ok {
				return
			}
		case <-timerC:
			timerC = nil
			rescan()
		}
	}
}

// close 是组件逆：停 watcher 与主循环，撤退全部 skill fiber。
func (s *supervisor) close() error {
	close(s.done)
	s.mu.Lock()
	names := make([]string, 0, len(s.loaded))
	for name := range s.loaded {
		names = append(names, name)
	}
	s.mu.Unlock()
	for _, name := range names {
		s.remove(name)
	}
	return nil
}
