// Command stc-agent is a minimal CLI chat agent where every capability is a
// fiber, built on stc-go. M1: minimal chat loop — config/model/session/cli
// fibers plus the /model command; model switching cascades while history
// survives.
package main

import (
	stdctx "context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/0xdenny218/stc-agent/internal/cli"
	"github.com/0xdenny218/stc-agent/internal/config"
	"github.com/0xdenny218/stc-agent/internal/model"
	"github.com/0xdenny218/stc-agent/internal/session"
	stc "github.com/0xdenny218/stc-go"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Getenv))
}

type options struct {
	cfg        config.Config
	transcript string
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
	)
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}

	cfg := defaultConfig()

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
		if err := mergeFile(&cfg, path); err != nil {
			return options{}, err
		}
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

	tp := *transcript
	if *resume != "" {
		tp = *resume
	}
	return options{cfg: cfg, transcript: tp}, nil
}

// fileConfig 是配置文件的子集（timeout 只走命令行）。
type fileConfig struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Model   string `json:"model"`
}

func mergeFile(cfg *config.Config, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config file: %w", err)
	}
	var fc fileConfig
	if err := json.Unmarshal(b, &fc); err != nil {
		return fmt.Errorf("parse config file %s: %w", path, err)
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
	return nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

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

	_, ctlComp := config.NewControl(root, opts.cfg)
	console := cli.NewConsole(stdin, stdout)

	// 装配列表（spec D2）：提供者在前，依赖由 inject 解析。
	comps := []stc.Component{
		ctlComp,
		model.Component(),
		session.Component(opts.transcript),
		cli.RegistryComponent(),
		cli.ModelCommandComponent(),
		cli.Component(console),
	}
	fibers := make([]*stc.Fiber, 0, len(comps))
	for _, c := range comps {
		fibers = append(fibers, root.Load(c))
	}

	boot, cancel := stdctx.WithTimeout(stdctx.Background(), 10*time.Second)
	defer cancel()
	for _, f := range fibers {
		if err := f.Ready(boot); err != nil {
			fmt.Fprintf(stdout, "error: fiber %s: %v\n", f.Name(), err)
			for _, g := range fibers {
				fmt.Fprintf(stdout, "  %-10s %s\n", g.Name(), g.State())
			}
			return 1
		}
	}

	<-console.Done()
	return 0
}
