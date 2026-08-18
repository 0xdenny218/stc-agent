// Package config provides the agent's configuration fibers: a stable control
// service that owns the dynamic config fiber, so switching the model is a
// re-provision that cascades to consumers (spec D4).
package config

import (
	stdctx "context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	stc "github.com/0xdenny218/stc-go"
)

// Config 是模型接入配置（热可换的部分）。会话等不愿被级联波及的能力
// 不 inject 它——依赖图之外即存活（spec D4）。
type Config struct {
	BaseURL string        `json:"base_url"`
	APIKey  string        `json:"api_key"`
	Model   string        `json:"model"`
	Timeout time.Duration `json:"-"`
}

func (c Config) Validate() error {
	if c.BaseURL == "" {
		return errors.New("config: base URL is required")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("config: invalid base URL %q", c.BaseURL)
	}
	if c.APIKey == "" {
		return errors.New("config: API key is required (--api-key, or STC_AGENT_API_KEY / DEEPSEEK_API_KEY / OPENAI_API_KEY)")
	}
	if c.Model == "" {
		return errors.New("config: model is required")
	}
	if c.Timeout <= 0 {
		return errors.New("config: timeout must be positive")
	}
	return nil
}

// KeyConfig 是当前生效的配置；换模型 = 重提供它 → 级联重载消费者。
var KeyConfig = stc.NewKey[Config]("config")

// KeyConfigCtl 是稳定的配置控制服务（inject 为空，永不因换模型而重载）。
var KeyConfigCtl = stc.NewKey[*Control]("configctl")

// Control 持有当前 config fiber 的句柄，执行"先等旧提供者 Gone 再重提供"
// 的换血序列（stc-go 生命周期契约）。
type Control struct {
	root *stc.Context
	// track 把当前 config fiber 登记进 fiber 目录（inspect），返回的逆在
	// 换血/清理时摘除；nil 表示不登记。
	track func(*stc.Fiber) stc.Inverse

	mu      sync.Mutex
	cur     Config
	fib     *stc.Fiber
	untrack stc.Inverse
}

// NewControl 返回控制服务及其组件。root 用于装载 config fiber：注册表
// 中独立的子 fiber，卸载由组件逆显式负责（嵌套 fiber 不级联，stc-go D7）。
func NewControl(root *stc.Context, initial Config, track func(*stc.Fiber) stc.Inverse) (*Control, stc.Component) {
	ctl := &Control{root: root, cur: initial, track: track}
	return ctl, stc.Component{
		Name:    "configctl",
		Provide: []stc.Key{KeyConfigCtl},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			ctl.mu.Lock()
			ctl.fib = root.Load(component(ctl.cur))
			if ctl.track != nil {
				ctl.untrack = ctl.track(ctl.fib)
			}
			ctl.mu.Unlock()
			// 先登记子 fiber 的清理再提供 ctl：LIFO 回卷时先撤 ctl
			// 服务，再拆 config fiber。
			if err := c.Effect(func() stc.Inverse {
				return func() error {
					ctl.mu.Lock()
					f := ctl.fib
					untrack := ctl.untrack
					ctl.untrack = nil
					ctl.mu.Unlock()
					f.Dispose()
					ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
					defer cancel()
					err := f.Gone(ctx)
					if untrack != nil {
						untrack()
					}
					return err
				}
			}); err != nil {
				return nil, err
			}
			_, err := c.Provide(KeyConfigCtl, ctl)
			return nil, err
		},
	}
}

// component 把一份 Config 提供为服务的 fiber。
func component(cfg Config) stc.Component {
	return stc.Component{
		Name:    "config",
		Provide: []stc.Key{KeyConfig},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			if err := cfg.Validate(); err != nil {
				return nil, err
			}
			_, err := c.Provide(KeyConfig, cfg)
			return nil, err
		},
	}
}

// SetModel 换模型：撤退旧 config fiber（等 Gone）→ 装载新 fiber（等
// Active）；失败回滚旧配置。等待用脱离调用方取消的 ctx——调用方（REPL
// 周期）可能被这次级联本身取消。
func (ctl *Control) SetModel(ctx stdctx.Context, model string) error {
	if model == "" {
		return errors.New("config: empty model name")
	}
	ctl.mu.Lock()
	defer ctl.mu.Unlock()

	next := ctl.cur
	next.Model = model

	wait, cancel := stdctx.WithTimeout(stdctx.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	old := ctl.fib
	old.Dispose()
	if err := old.Gone(wait); err != nil {
		return fmt.Errorf("config: waiting old config gone: %w", err)
	}
	if ctl.untrack != nil { // 旧 fiber 从目录摘除
		ctl.untrack()
		ctl.untrack = nil
	}
	f := ctl.root.Load(component(next))
	if err := f.Ready(wait); err != nil {
		rb := ctl.root.Load(component(ctl.cur))
		if rbErr := rb.Ready(wait); rbErr != nil {
			return fmt.Errorf("config: switch failed (%v); rollback also failed: %w", err, rbErr)
		}
		ctl.fib = rb
		if ctl.track != nil {
			ctl.untrack = ctl.track(rb)
		}
		return fmt.Errorf("config: switch failed, rolled back: %w", err)
	}
	ctl.cur = next
	ctl.fib = f
	if ctl.track != nil {
		ctl.untrack = ctl.track(f)
	}
	return nil
}
