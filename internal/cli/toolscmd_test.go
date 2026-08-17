package cli

import (
	"bytes"
	stdctx "context"
	"strings"
	"testing"
	"time"

	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
)

// Contract/CommandEffect 在 /tools 上复检（spec M2）：工具列表精确反映
// 注册效应；命令 fiber 卸载即注销。
func TestToolsCommandEffect(t *testing.T) {
	root := stc.New()
	defer root.Close()

	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()

	load := func(c stc.Component) *stc.Fiber {
		t.Helper()
		f := root.Load(c)
		if err := f.Ready(ctx); err != nil {
			t.Fatalf("fiber %s: %v", f.Name(), err)
		}
		return f
	}
	load(RegistryComponent())
	load(tools.ToolsetComponent())
	tf := load(ToolsCommandComponent())
	load(HelpCommandComponent())

	reg, err := stc.Service[*Registry](root, KeyCommands)
	if err != nil {
		t.Fatalf("resolve registry: %v", err)
	}

	var buf bytes.Buffer
	if handled, err := reg.Dispatch(ctx, &buf, "/tools"); err != nil || !handled {
		t.Fatalf("dispatch /tools: handled=%v err=%v", handled, err)
	}
	if !strings.Contains(buf.String(), "no tools registered") {
		t.Fatalf("empty tool list: %q", buf.String())
	}

	load(tools.ReadFileComponent())
	buf.Reset()
	if _, err := reg.Dispatch(ctx, &buf, "/tools"); err != nil {
		t.Fatalf("dispatch /tools: %v", err)
	}
	if !strings.Contains(buf.String(), "read_file — read the contents of a file") {
		t.Fatalf("/tools output: %q", buf.String())
	}

	buf.Reset()
	if _, err := reg.Dispatch(ctx, &buf, "/help"); err != nil {
		t.Fatalf("dispatch /help: %v", err)
	}
	for _, want := range []string{"/help", "/tools", "/quit"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("/help missing %q: %q", want, buf.String())
		}
	}

	tf.Dispose()
	if err := tf.Gone(ctx); err != nil {
		t.Fatalf("tools command gone: %v", err)
	}
	if handled, _ := reg.Dispatch(ctx, &buf, "/tools"); handled {
		t.Fatal("/tools still registered after its fiber unloaded")
	}
}
