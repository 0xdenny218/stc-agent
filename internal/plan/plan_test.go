package plan

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/0xdenny218/stc-agent/internal/cli"
	"github.com/0xdenny218/stc-agent/internal/hooks"
	"github.com/0xdenny218/stc-agent/internal/interaction"
	"github.com/0xdenny218/stc-agent/internal/prompt"
	"github.com/0xdenny218/stc-agent/internal/session"
	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
)

// stubIA 是可脚本化的提问 provider：按队列回键或错误。
type stubIA struct {
	answers []string
	errs    []error
}

func (s *stubIA) Ask(stdctx.Context, interaction.Question) (string, error) {
	if len(s.errs) > 0 {
		err := s.errs[0]
		s.errs = s.errs[1:]
		return "", err
	}
	if len(s.answers) == 0 {
		return "n", nil
	}
	a := s.answers[0]
	s.answers = s.answers[1:]
	return a, nil
}

// load 装配 plan 相关 fiber，返回全部句柄。
func load(t *testing.T, ia interaction.Service) (*prompt.Segments, *session.Session, *tools.Toolset,
	*hooks.Interceptors, *cli.Registry, *Mode) {
	t.Helper()
	root := stc.New()
	t.Cleanup(func() { root.Close() })
	comps := []stc.Component{
		prompt.Component(),
		session.Component(""),
		tools.ToolsetComponent(),
		hooks.Component(),
		cli.RegistryComponent(),
		Component(ia),
	}
	boot, cancel := stdctx.WithTimeout(stdctx.Background(), 10*time.Second)
	defer cancel()
	for _, c := range comps {
		f := root.Load(c)
		if err := f.Ready(boot); err != nil {
			t.Fatalf("load %s: %v", c.Name, err)
		}
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	reg, err := stc.Service[*prompt.Segments](root, prompt.KeyPrompt)
	must(err)
	sess, err := stc.Service[*session.Session](root, session.KeySession)
	must(err)
	ts, err := stc.Service[*tools.Toolset](root, tools.KeyTools)
	must(err)
	ic, err := stc.Service[*hooks.Interceptors](root, hooks.KeyHooks)
	must(err)
	cmds, err := stc.Service[*cli.Registry](root, cli.KeyCommands)
	must(err)
	m, err := stc.Service[*Mode](root, KeyMode)
	must(err)
	return reg, sess, ts, ic, cmds, m
}

// lastApproval 取最近一条审批事件。
func lastApproval(t *testing.T, sess *session.Session) *session.Approval {
	t.Helper()
	for i := len(sess.Events()) - 1; i >= 0; i-- {
		if ev := sess.Events()[i]; ev.Type == session.EventApproval {
			return ev.Approval
		}
	}
	t.Fatal("no approval event in log")
	return nil
}

func exitTool(t *testing.T, ts *tools.Toolset) tools.Tool {
	t.Helper()
	tool, ok := ts.Lookup("exit_plan_mode")
	if !ok {
		t.Fatal("exit_plan_mode not registered")
	}
	return tool
}

// Contract/PlanGate（spec M9）：/plan 开关模式；模式在场时拦截器 bail
// 非只读工具（只读集放行）；exit_plan_mode 批准 = 关模式 + 计划回灌 +
// 审批事件入日志。
func TestPlanModeGate(t *testing.T) {
	ia := &stubIA{}
	reg, sess, ts, ic, cmds, m := load(t, ia)

	// /plan 开：模式在场 + 段落进 system prompt。
	var out strings.Builder
	if _, err := cli.Dispatch(stdctx.Background(), &out, "/plan", cmds); err != nil {
		t.Fatalf("/plan: %v", err)
	}
	if !strings.Contains(out.String(), "plan mode on") || !m.Enabled() {
		t.Fatalf("/plan output: %q enabled=%v", out.String(), m.Enabled())
	}
	if _, ok := reg.Lookup(segmentName); !ok {
		t.Fatal("plan segment must be registered while in plan mode")
	}

	// 拦截器：非只读工具被 bail（loop 会把原因回灌模型），只读放行。
	err := hooks.Check(stdctx.Background(), ic, hooks.ToolPreExecute,
		hooks.Payload{Tool: "write_file", Arguments: `{}`})
	if err == nil || !strings.Contains(err.Error(), "not read-only") {
		t.Fatalf("write_file must be bailed in plan mode: %v", err)
	}
	if err := hooks.Check(stdctx.Background(), ic, hooks.ToolPreExecute,
		hooks.Payload{Tool: "read_file"}); err != nil {
		t.Fatalf("read_file must pass in plan mode: %v", err)
	}
	if err := hooks.Check(stdctx.Background(), ic, hooks.ToolPreExecute,
		hooks.Payload{Tool: "exit_plan_mode"}); err != nil {
		t.Fatalf("exit_plan_mode must pass (it asks the user itself): %v", err)
	}

	// 批准：模式关、段落摘除、计划回灌、审批事件 {allow,user}。
	ia.answers = []string{"y"}
	out2, err := exitTool(t, ts).Invoke(stdctx.Background(), json.RawMessage(`{"plan":"step 1 then step 2"}`))
	if err != nil {
		t.Fatalf("exit_plan_mode: %v", err)
	}
	if !strings.Contains(out2, "approved") || !strings.Contains(out2, "step 1 then step 2") {
		t.Fatalf("approved plan must feed back: %q", out2)
	}
	if m.Enabled() {
		t.Fatal("approval must turn plan mode off")
	}
	if _, ok := reg.Lookup(segmentName); ok {
		t.Fatal("segment must be removed after approval")
	}
	if a := lastApproval(t, sess); a.Decision != "allow" || a.Source != "user" || a.Tool != "exit_plan_mode" {
		t.Fatalf("approval event: %+v", a)
	}

	// 拦截器随模式关闭而放行。
	if err := hooks.Check(stdctx.Background(), ic, hooks.ToolPreExecute,
		hooks.Payload{Tool: "write_file"}); err != nil {
		t.Fatalf("write_file must pass once plan mode is off: %v", err)
	}
}

// 拒绝、不在模式、空计划与提问中断的语义。
func TestPlanModeRejections(t *testing.T) {
	ia := &stubIA{}
	_, sess, ts, _, cmds, m := load(t, ia)

	// 不在 plan 模式：no-op 错误，不问用户、不落日志。
	if _, err := exitTool(t, ts).Invoke(stdctx.Background(), json.RawMessage(`{"plan":"x"}`)); err == nil ||
		!strings.Contains(err.Error(), "not in plan mode") {
		t.Fatalf("outside plan mode: %v", err)
	}
	if n := len(sess.Events()); n != 0 {
		t.Fatalf("no events expected outside plan mode: %d", n)
	}

	cli.Dispatch(stdctx.Background(), &strings.Builder{}, "/plan", cmds)

	// 拒绝：留在模式，审批事件 {deny,user}。
	ia.answers = []string{"n"}
	out, err := exitTool(t, ts).Invoke(stdctx.Background(), json.RawMessage(`{"plan":"bad plan"}`))
	if err != nil {
		t.Fatalf("reject is not a turn error: %v", err)
	}
	if !strings.Contains(out, "rejected") || !m.Enabled() {
		t.Fatalf("rejection must keep plan mode on: %q enabled=%v", out, m.Enabled())
	}
	if a := lastApproval(t, sess); a.Decision != "deny" || a.Source != "user" {
		t.Fatalf("rejection event: %+v", a)
	}

	// 空计划：参数错误，不问、不落日志。
	before := len(sess.Events())
	if _, err := exitTool(t, ts).Invoke(stdctx.Background(), json.RawMessage(`{"plan":"  "}`)); err == nil ||
		!strings.Contains(err.Error(), "plan is required") {
		t.Fatalf("blank plan: %v", err)
	}
	if len(sess.Events()) != before {
		t.Fatal("blank plan must not log")
	}

	// 提问处 Ctrl-C：轮次级中断（ErrAborted 上抛）。
	ia.errs = []error{interaction.ErrAborted}
	if _, err := exitTool(t, ts).Invoke(stdctx.Background(), json.RawMessage(`{"plan":"x"}`)); !errors.Is(err, interaction.ErrAborted) {
		t.Fatalf("want ErrAborted, got %v", err)
	}

	// headless fail-closed：问不了就拒绝，留在模式，事件 {deny,fail-closed}。
	ia2 := &stubIA{errs: []error{interaction.ErrUnavailable}}
	_, sess2, ts2, _, cmds2, m2 := load(t, ia2)
	var out2 strings.Builder
	cli.Dispatch(stdctx.Background(), &out2, "/plan", cmds2)
	out3, err := exitTool(t, ts2).Invoke(stdctx.Background(), json.RawMessage(`{"plan":"x"}`))
	if err != nil || !strings.Contains(out3, "fail-closed") {
		t.Fatalf("fail-closed: %q %v", out3, err)
	}
	if !m2.Enabled() {
		t.Fatal("fail-closed must keep plan mode on")
	}
	if a := lastApproval(t, sess2); a.Decision != "deny" || a.Source != "fail-closed" {
		t.Fatalf("fail-closed event: %+v", a)
	}
}

// /plan 再切一次 = 手动关（不经提问）。
func TestPlanToggleOff(t *testing.T) {
	_, _, _, _, cmds, m := load(t, &stubIA{})
	var out strings.Builder
	cli.Dispatch(stdctx.Background(), &out, "/plan", cmds)
	cli.Dispatch(stdctx.Background(), &out, "/plan", cmds)
	if m.Enabled() {
		t.Fatal("second /plan must toggle off")
	}
	if !strings.Contains(out.String(), "plan mode off") {
		t.Fatalf("toggle output: %q", out.String())
	}
}
