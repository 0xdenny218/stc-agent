package cli

import (
	"bytes"
	stdctx "context"
	"fmt"
	"io"
	"testing"
	"time"

	stc "github.com/0xdenny218/stc-go"
)

// Contract/CommandEffect 的 M1 形态：命令注册 = 可逆效应，fiber 卸载即
// 注销（spec M2 场景在 /tools 上复检）。
func TestCommandEffect(t *testing.T) {
	root := stc.New()
	defer root.Close()

	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()

	if err := root.Load(RegistryComponent()).Ready(ctx); err != nil {
		t.Fatalf("registry fiber: %v", err)
	}
	cmd := stc.Component{
		Name:   "cmd:x",
		Inject: []stc.Key{KeyCommands},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			reg, err := stc.Service[*Registry](c, KeyCommands)
			if err != nil {
				return nil, err
			}
			return reg.Register("x", func(_ stdctx.Context, w io.Writer, args string) error {
				fmt.Fprintf(w, "got:%s", args)
				return nil
			}), nil
		},
	}
	cf := root.Load(cmd)
	if err := cf.Ready(ctx); err != nil {
		t.Fatalf("command fiber: %v", err)
	}

	reg, err := stc.Service[*Registry](root, KeyCommands)
	if err != nil {
		t.Fatalf("resolve registry: %v", err)
	}
	var buf bytes.Buffer
	handled, err := reg.Dispatch(ctx, &buf, "/x hello")
	if err != nil || !handled {
		t.Fatalf("dispatch: handled=%v err=%v", handled, err)
	}
	if buf.String() != "got:hello" {
		t.Fatalf("command output: %q", buf.String())
	}

	cf.Dispose()
	if err := cf.Gone(ctx); err != nil {
		t.Fatalf("waiting command gone: %v", err)
	}
	if handled, _ := reg.Dispatch(ctx, &buf, "/x hello"); handled {
		t.Fatal("command still registered after its fiber unloaded")
	}
}
