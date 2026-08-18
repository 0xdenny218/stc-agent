package model_test

import (
	stdctx "context"
	"testing"
	"time"

	"github.com/0xdenny218/stc-agent/internal/config"
	"github.com/0xdenny218/stc-agent/internal/model"
	stc "github.com/0xdenny218/stc-go"
)

// Contract/ConfigCascade：config 重提供 → model fiber 重载（chat 换成
// 新实例、新模型生效）。
func TestConfigCascade(t *testing.T) {
	root := stc.New()
	defer root.Close()

	cfg := config.Config{BaseURL: "http://127.0.0.1", APIKey: "k", Model: "alpha", Timeout: time.Second}
	ctl, ctlComp := config.NewControl(root, cfg, nil)
	root.Load(ctlComp)
	root.Load(model.Component())

	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()

	chat0 := waitChat(t, ctx, root, "alpha")

	if err := ctl.SetModel(ctx, "beta"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	chat1 := waitChat(t, ctx, root, "beta")
	if chat1 == chat0 {
		t.Fatal("expected a new chat service instance after config re-provision")
	}
}

func waitChat(t *testing.T, ctx stdctx.Context, root *stc.Context, wantModel string) model.ChatService {
	t.Helper()
	for {
		v, err := root.WaitService(ctx, model.KeyChat)
		if err != nil {
			t.Fatalf("waiting chat service (model %q): %v", wantModel, err)
		}
		if svc, ok := v.(model.ChatService); ok && svc.Model() == wantModel {
			return svc
		}
		// 级联窗口期：chat 还是旧实例，稍等重提供。
		time.Sleep(5 * time.Millisecond)
	}
}
