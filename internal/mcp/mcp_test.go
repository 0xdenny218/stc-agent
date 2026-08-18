package mcp

// MCP stdio fiber 的契约测试（spec M8）：握手/工具目录/调用往返，以及
// 核心语义——server 断开 = 工具从 toolset 消失。陪练是
// examples/mcp/echo（go build 到临时目录，CI 有 Go 工具链即可跑）。

import (
	stdctx "context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
)

func buildEcho(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "echo-server")
	cmd := exec.Command("go", "build", "-o", out, "../../examples/mcp/echo")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build echo server: %v\n%s", err, b)
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestClientRoundTrip(t *testing.T) {
	cl, err := start(Server{Name: "echo", Command: buildEcho(t)})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer cl.kill()

	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()
	defs, err := cl.listTools(ctx)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(defs) != 1 || defs[0].Name != "echo" {
		t.Fatalf("tools: %+v", defs)
	}
	out, err := cl.callTool(ctx, "echo", json.RawMessage(`{"text":"hi mcp"}`))
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if out != "hi mcp" {
		t.Fatalf("echo result: %q", out)
	}
}

// E2E/MCPToolCall 的断开半边（spec M8 验收）：server 断开 = 工具即时
// 从 toolset 消失。
func TestFiberDisconnectRemovesTools(t *testing.T) {
	root := stc.New()
	defer root.Close()

	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()
	ready(t, ctx, root.Load(tools.ToolsetComponent()))
	ts, err := stc.Service[*tools.Toolset](root, tools.KeyTools)
	if err != nil {
		t.Fatal(err)
	}

	statusCh := make(chan string, 2)
	f := root.Load(Component(Server{
		Name:    "echo",
		Command: buildEcho(t),
		Env:     []string{"ECHO_DIE_AFTER_MS=400"},
	}, func(s string) { statusCh <- s }))
	ready(t, ctx, f)

	tool, ok := ts.Lookup("mcp__echo__echo")
	if !ok {
		t.Fatal("mcp tool registered after handshake")
	}
	out, err := tool.Invoke(stdctx.Background(), json.RawMessage(`{"text":"alive"}`))
	if err != nil || out != "alive" {
		t.Fatalf("invoke before disconnect: %q, %v", out, err)
	}

	// server 自杀 → 工具消失 + onStatus 上报。
	waitFor(t, "tool removed after disconnect", func() bool {
		_, ok := ts.Lookup("mcp__echo__echo")
		return !ok
	})
	select {
	case s := <-statusCh:
		if !strings.Contains(s, "disconnected") {
			t.Fatalf("status: %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no disconnect status reported")
	}
}

func ready(t *testing.T, ctx stdctx.Context, f *stc.Fiber) {
	t.Helper()
	if err := f.Ready(ctx); err != nil {
		t.Fatalf("fiber %s: %v", f.Name(), err)
	}
}

func TestToolName(t *testing.T) {
	if got := toolName("echo", "echo"); got != "mcp__echo__echo" {
		t.Fatalf("plain: %q", got)
	}
	if got := toolName("my server", "weird.tool"); got != "mcp__my_server__weird_tool" {
		t.Fatalf("sanitized: %q", got)
	}
}
