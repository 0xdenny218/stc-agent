// Command stc-agent is a minimal CLI chat agent where every capability is a
// fiber, built on stc-go. M2: multi-turn tool calling — the loop fiber drives
// [model → tool]* rounds against the static Go tool fibers (read_file,
// write_file, shell); /tools and /help are command fibers. M3: every *.wasm
// in --tools-dir is a guest tool fiber, hot-swapped in place on rebuild.
// M5: the session is an event log, the model client is SSE-streaming, and
// the terminal has readline plus a -p headless mode. M6: every tool call
// passes an approval gate (policy + mid-turn question loop) before running.
// M7: hooks (notify + intercept), system prompt assembled from fiber-registered
// segments, and an inspect_agent tool for self-description.
// M8: skills (SKILL.md directories hot-loaded as fibers) and MCP stdio
// servers as tool fibers (disconnect = tools vanish).
package main

import (
	stdctx "context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/0xdenny218/stc-agent/internal/approval"
	"github.com/0xdenny218/stc-agent/internal/cli"
	"github.com/0xdenny218/stc-agent/internal/config"
	"github.com/0xdenny218/stc-agent/internal/guest"
	"github.com/0xdenny218/stc-agent/internal/hooks"
	"github.com/0xdenny218/stc-agent/internal/inspect"
	"github.com/0xdenny218/stc-agent/internal/interaction"
	"github.com/0xdenny218/stc-agent/internal/loop"
	"github.com/0xdenny218/stc-agent/internal/mcp"
	"github.com/0xdenny218/stc-agent/internal/model"
	"github.com/0xdenny218/stc-agent/internal/prompt"
	"github.com/0xdenny218/stc-agent/internal/session"
	"github.com/0xdenny218/stc-agent/internal/skills"
	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Getenv))
}

type options struct {
	cfg        config.Config
	policy     approval.Policy
	transcript string
	toolsDir   string
	skillsDir  string
	mcpServers []mcp.Server
	print      string
}

func defaultConfig() config.Config {
	return config.Config{
		BaseURL: "https://api.deepseek.com",
		Model:   "deepseek-chat",
		Timeout: 60 * time.Second,
	}
}

// parseOptions 合并配置来源，优先级：默认 < 配置文件 < 环境变量 < 命令行。
func parseOptions(args []string, getenv func(string) string) (options, error) {
	fs := flag.NewFlagSet("stc-agent", flag.ContinueOnError)
	var (
		configPath = fs.String("config", "", "config file (default ~/.config/stc-agent/config.json)")
		baseURL    = fs.String("base-url", "", "OpenAI-compatible base URL")
		apiKey     = fs.String("api-key", "", "API key")
		modelName  = fs.String("model", "", "model name")
		timeout    = fs.Duration("timeout", 0, "request timeout")
		transcript = fs.String("transcript", "", "append a JSONL transcript to this path; an existing file is replayed")
		resume     = fs.String("resume", "", "alias of --transcript: resume from this transcript file")
		toolsDir   = fs.String("tools-dir", "tools.d", "directory of *.wasm guest tools (watched for hot-swap)")
		skillsDir  = fs.String("skills-dir", "skills.d", "directory of skills (each <name>/SKILL.md hot-loads as a fiber)")
		printShort = fs.String("p", "", "print mode: run a single turn non-interactively and exit")
		printLong  = fs.String("print", "", "alias of -p")
		allow      = fs.String("allow", "", "comma-separated tool names to auto-approve (\"*\" allows all)")
		mcpSpecs   []string
	)
	fs.Var(funcValue(func(v string) error { mcpSpecs = append(mcpSpecs, v); return nil }),
		"mcp", "MCP stdio server as name=command args... (repeatable)")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}

	cfg := defaultConfig()
	policy := approval.DefaultPolicy()
	var servers []mcp.Server

	path := *configPath
	if path == "" {
		if home, err := os.UserHomeDir(); err == nil {
			p := filepath.Join(home, ".config", "stc-agent", "config.json")
			if _, err := os.Stat(p); err == nil {
				path = p
			}
		}
	}
	if path != "" {
		filePolicy, fileServers, err := mergeFile(&cfg, path)
		if err != nil {
			return options{}, err
		}
		if filePolicy != nil {
			policy = *filePolicy
		}
		servers = append(servers, fileServers...)
	}

	if v := firstNonEmpty(getenv("STC_AGENT_API_KEY"), getenv("DEEPSEEK_API_KEY"), getenv("OPENAI_API_KEY")); v != "" {
		cfg.APIKey = v
	}
	if v := getenv("STC_AGENT_BASE_URL"); v != "" {
		cfg.BaseURL = v
	}
	if v := getenv("STC_AGENT_MODEL"); v != "" {
		cfg.Model = v
	}

	if *baseURL != "" {
		cfg.BaseURL = *baseURL
	}
	if *apiKey != "" {
		cfg.APIKey = *apiKey
	}
	if *modelName != "" {
		cfg.Model = *modelName
	}
	if *timeout != 0 {
		cfg.Timeout = *timeout
	}
	if *allow != "" {
		policy.Allow = append(policy.Allow, strings.Split(*allow, ",")...)
	}
	for _, spec := range mcpSpecs {
		srv, err := parseMCPSpec(spec)
		if err != nil {
			return options{}, err
		}
		servers = append(servers, srv)
	}

	tp := *transcript
	if *resume != "" {
		tp = *resume
	}
	pp := *printShort
	if *printLong != "" {
		pp = *printLong
	}
	return options{cfg: cfg, policy: policy, transcript: tp, toolsDir: *toolsDir, skillsDir: *skillsDir, mcpServers: servers, print: pp}, nil
}

// funcValue 把单值回调适配成 flag.Value（--mcp 这类可重复旗标）。
type funcValue func(string) error

func (f funcValue) String() string     { return "" }
func (f funcValue) Set(v string) error { return f(v) }

// parseMCPSpec 解析 --mcp 的 "name=command args..." 形式。
func parseMCPSpec(v string) (mcp.Server, error) {
	name, cmdline, ok := strings.Cut(v, "=")
	if !ok || strings.TrimSpace(name) == "" {
		return mcp.Server{}, fmt.Errorf("--mcp: want name=command args..., got %q", v)
	}
	fields := strings.Fields(cmdline)
	if len(fields) == 0 {
		return mcp.Server{}, fmt.Errorf("--mcp %s: empty command", name)
	}
	return mcp.Server{Name: name, Command: fields[0], Args: fields[1:]}, nil
}

// fileConfig 是配置文件的子集（timeout 只走命令行）。
type fileConfig struct {
	BaseURL  string           `json:"base_url"`
	APIKey   string           `json:"api_key"`
	Model    string           `json:"model"`
	Approval *approval.Policy `json:"approval"`
	MCP      []mcp.Server     `json:"mcp"`
}

// mergeFile 把配置文件并入 cfg，返回文件携带的审批策略（无则 nil——
// 策略不经 config.Config：它是启动期输入，与 transcript/toolsDir 同族，
// 不随 /model 级联热换）与 MCP server 列表。
func mergeFile(cfg *config.Config, path string) (*approval.Policy, []mcp.Server, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read config file: %w", err)
	}
	var fc fileConfig
	if err := json.Unmarshal(b, &fc); err != nil {
		return nil, nil, fmt.Errorf("parse config file %s: %w", path, err)
	}
	if fc.BaseURL != "" {
		cfg.BaseURL = fc.BaseURL
	}
	if fc.APIKey != "" {
		cfg.APIKey = fc.APIKey
	}
	if fc.Model != "" {
		cfg.Model = fc.Model
	}
	for i, srv := range fc.MCP {
		if srv.Name == "" || srv.Command == "" {
			return nil, nil, fmt.Errorf("config file %s: mcp[%d] needs name and command", path, i)
		}
	}
	return fc.Approval, fc.MCP, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// identitySegment 是 system prompt 的基础段落（spec M7）：段落注册表按
// 名字序拼接，更多段落可由 fiber 注册进来（如 "20-style"），这里给的是
// 装配方自带的身份段。
const identitySegment = "You are stc-agent, a terminal chat agent built on stc-go where every capability is a fiber. Answer concisely and use the provided tools when they help."

func run(args []string, stdin io.Reader, stdout io.Writer, getenv func(string) string) int {
	opts, err := parseOptions(args, getenv)
	if err != nil {
		fmt.Fprintf(stdout, "error: %v\n", err)
		return 2
	}
	if err := opts.cfg.Validate(); err != nil {
		fmt.Fprintf(stdout, "error: %v\n", err)
		return 2
	}

	root := stc.New()
	defer root.Close()

	boot, cancel := stdctx.WithTimeout(stdctx.Background(), 10*time.Second)
	defer cancel()

	// fiber 目录最先装载：config 换血与其余启动 fiber 都往里登记
	// （inspect 工具的 fiber 视图来源）。
	dirFiber := root.Load(inspect.DirectoryComponent())
	if err := dirFiber.Ready(boot); err != nil {
		fmt.Fprintf(stdout, "error: fiber %s: %v\n", dirFiber.Name(), err)
		return 1
	}
	dir, err := stc.Service[*inspect.Directory](root, inspect.KeyDirectory)
	if err != nil {
		fmt.Fprintf(stdout, "error: %v\n", err)
		return 1
	}
	dir.Register(dirFiber)

	_, ctlComp := config.NewControl(root, opts.cfg, dir.Register)
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stdout, "error: %v\n", err)
		return 2
	}

	// 提问回路（spec D15/D18）：REPL 模式的终端通道提前创建，审批门与
	// REPL 共享它；headless 一次性模式用 fail-closed provider（问不了
	// 就拒绝）。
	var console *cli.Console
	var ia interaction.Service
	if opts.print == "" {
		console = cli.NewConsole(stdin, stdout)
		defer console.Close()
		ia = cli.TerminalInteraction(console)
	} else {
		ia = interaction.Deny()
	}

	// 装配列表（spec D2）：提供者在前，依赖由 inject 解析。
	comps := []stc.Component{
		ctlComp,
		model.Component(),
		session.Component(opts.transcript),
		approval.Component(opts.policy, ia),
		hooks.Component(),
		prompt.Component(),
		prompt.SegmentComponent("10-identity", identitySegment),
		cli.RegistryComponent(),
		tools.ToolsetComponent(),
		tools.ReadFileComponent(),
		tools.WriteFileComponent(),
		tools.ShellComponent(cwd, 30*time.Second),
		inspect.ToolComponent(),
		guest.RuntimeComponent(),
	}
	// guest 工具（spec D5）：tools-dir 里每个 *.wasm 一个工具 fiber，
	// 热替换结果打到会话输出。坏 guest 的 fiber 装载失败 → 启动 fail-fast。
	guestComps, err := guest.Components(opts.toolsDir, func(name string, err error) {
		if err != nil {
			fmt.Fprintf(stdout, "[guest] %s reload failed: %v\n", name, err)
		} else {
			fmt.Fprintf(stdout, "[guest] %s reloaded\n", name)
		}
	})
	if err != nil {
		fmt.Fprintf(stdout, "error: %v\n", err)
		return 2
	}
	comps = append(comps, guestComps...)
	// skills（spec M8）：supervisor 监听 skills-dir，落盘即装、删除即卸。
	comps = append(comps, skills.SupervisorComponent(root, opts.skillsDir, dir.Register,
		func(name string, err error) {
			fmt.Fprintf(stdout, "[skill] %s: %v\n", name, err)
		},
		func(name string, loaded bool) {
			if loaded {
				fmt.Fprintf(stdout, "[skill] %s loaded\n", name)
			} else {
				fmt.Fprintf(stdout, "[skill] %s unloaded\n", name)
			}
		}))
	// MCP stdio servers（spec M8）：每个 server 一个 fiber，断开 = 工具失效。
	for _, srv := range opts.mcpServers {
		comps = append(comps, mcp.Component(srv, func(s string) {
			fmt.Fprintln(stdout, s)
		}))
	}
	comps = append(comps,
		loop.Component(10),
		cli.ModelCommandComponent(),
		cli.ToolsCommandComponent(),
		cli.HelpCommandComponent(),
	)
	fibers := make([]*stc.Fiber, 0, len(comps)+1)
	for _, c := range comps {
		f := root.Load(c)
		dir.Register(f) // 目录随启动装配登记；逆在进程退出时无意义，丢弃
		fibers = append(fibers, f)
	}

	waitReady := func(f *stc.Fiber) bool {
		if err := f.Ready(boot); err != nil {
			fmt.Fprintf(stdout, "error: fiber %s: %v\n", f.Name(), err)
			for _, g := range fibers {
				fmt.Fprintf(stdout, "  %-10s %s\n", g.Name(), g.State())
			}
			return false
		}
		return true
	}
	for _, f := range fibers {
		if !waitReady(f) {
			return 1
		}
	}

	if opts.print != "" {
		// headless 一次性模式（spec M5）：跑一轮打印答案退出，不装 REPL。
		runner, err := stc.Service[loop.Runner](root, loop.KeyRunner)
		if err != nil {
			fmt.Fprintf(stdout, "error: %v\n", err)
			return 1
		}
		if err := runner.RunTurn(stdctx.Background(), opts.print, stdout); err != nil {
			fmt.Fprintf(stdout, "error: %v\n", err)
			return 1
		}
		return 0
	}

	// cli 最后装载：serve 始于其 Apply。先等全部能力 Ready（guest 工具
	// 的 wasm 编译较慢），第一轮对话才能看到完整的工具表。
	cliFiber := root.Load(cli.Component(console))
	dir.Register(cliFiber)
	fibers = append(fibers, cliFiber)
	if !waitReady(cliFiber) {
		return 1
	}

	<-console.Done()
	return 0
}
