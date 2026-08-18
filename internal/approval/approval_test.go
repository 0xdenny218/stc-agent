package approval

import (
	stdctx "context"
	"errors"
	"testing"

	"github.com/0xdenny218/stc-agent/internal/interaction"
	"github.com/0xdenny218/stc-agent/internal/model"
	"github.com/0xdenny218/stc-agent/internal/session"
)

// scriptIA 是脚本化提问 provider：按队列回答，记录提问次数。
type scriptIA struct {
	answers []string
	err     error
	asks    int
}

func (s *scriptIA) Ask(stdctx.Context, interaction.Question) (string, error) {
	s.asks++
	if s.err != nil {
		return "", s.err
	}
	if len(s.answers) == 0 {
		return "", errors.New("scriptIA: no answer queued")
	}
	a := s.answers[0]
	s.answers = s.answers[1:]
	return a, nil
}

func call(name, args string) model.ToolCall {
	return model.ToolCall{
		ID: "c1", Type: "function",
		Function: model.ToolCallFunction{Name: name, Arguments: args},
	}
}

func approvalsOf(sess *session.Session) []session.Approval {
	var out []session.Approval
	for _, ev := range sess.Events() {
		if ev.Type == session.EventApproval {
			out = append(out, *ev.Approval)
		}
	}
	return out
}

// 策略矩阵（spec D15）：内置默认只读放行（无询问、无事件）；deny 优先
// 于 allow；"*" 全放行；策略硬拒绝入事件日志且不提问。
func TestPolicyDecisions(t *testing.T) {
	ctx := stdctx.Background()

	t.Run("read_file allowed silently by default", func(t *testing.T) {
		sess := &session.Session{}
		ia := &scriptIA{}
		a := New(DefaultPolicy(), ia, sess)
		if err := a.Check(ctx, call("read_file", `{"path":"/x"}`)); err != nil {
			t.Fatalf("read_file should pass: %v", err)
		}
		if ia.asks != 0 {
			t.Fatalf("policy allow must not ask (%d asks)", ia.asks)
		}
		if got := approvalsOf(sess); len(got) != 0 {
			t.Fatalf("policy allow must not log: %+v", got)
		}
	})

	t.Run("write_file asks by default", func(t *testing.T) {
		sess := &session.Session{}
		ia := &scriptIA{answers: []string{"y"}}
		a := New(DefaultPolicy(), ia, sess)
		if err := a.Check(ctx, call("write_file", `{}`)); err != nil {
			t.Fatalf("user allowed: %v", err)
		}
		if ia.asks != 1 {
			t.Fatalf("asks: %d", ia.asks)
		}
	})

	t.Run("star allows everything", func(t *testing.T) {
		sess := &session.Session{}
		ia := &scriptIA{}
		a := New(Policy{Allow: []string{"*"}}, ia, sess)
		if err := a.Check(ctx, call("shell", `{}`)); err != nil {
			t.Fatalf("star allow: %v", err)
		}
		if ia.asks != 0 {
			t.Fatalf("star allow must not ask")
		}
	})

	t.Run("deny beats allow and logs policy denial", func(t *testing.T) {
		sess := &session.Session{}
		ia := &scriptIA{}
		a := New(Policy{Allow: []string{"*"}, Deny: []string{"shell"}}, ia, sess)
		err := a.Check(ctx, call("shell", `{"command":"rm -rf /"}`))
		var de *DeniedError
		if !errors.As(err, &de) {
			t.Fatalf("want DeniedError, got %v", err)
		}
		if ia.asks != 0 {
			t.Fatalf("policy deny must not ask")
		}
		got := approvalsOf(sess)
		if len(got) != 1 || got[0].Decision != "deny" || got[0].Source != "policy" || got[0].Tool != "shell" {
			t.Fatalf("policy denial event: %+v", got)
		}
	})
}

// 询问回路（spec D15/D18）：y/a/n 的落盘与后续行为；fail-closed；提问处
// Ctrl-C 原样上抛（轮次中断，不是拒绝）。
func TestAskLoop(t *testing.T) {
	ctx := stdctx.Background()

	t.Run("y allows once and logs", func(t *testing.T) {
		sess := &session.Session{}
		ia := &scriptIA{answers: []string{"y", "n"}}
		a := New(DefaultPolicy(), ia, sess)
		if err := a.Check(ctx, call("shell", `{"command":"ls"}`)); err != nil {
			t.Fatalf("y: %v", err)
		}
		err := a.Check(ctx, call("shell", `{}`))
		var de *DeniedError
		if !errors.As(err, &de) {
			t.Fatalf("y is once-only: %v", err)
		}
		got := approvalsOf(sess)
		if len(got) != 2 || got[0].Decision != "allow" || got[0].Source != "user" ||
			got[1].Decision != "deny" || got[1].Source != "user" {
			t.Fatalf("events: %+v", got)
		}
		if got[0].Arguments == "" {
			t.Fatalf("arguments should be recorded: %+v", got[0])
		}
	})

	t.Run("a always-allows for the session", func(t *testing.T) {
		sess := &session.Session{}
		ia := &scriptIA{answers: []string{"a"}}
		a := New(DefaultPolicy(), ia, sess)
		for range 3 {
			if err := a.Check(ctx, call("shell", `{}`)); err != nil {
				t.Fatalf("a: %v", err)
			}
		}
		if ia.asks != 1 {
			t.Fatalf("always-allow must not re-ask (%d asks)", ia.asks)
		}
		if got := approvalsOf(sess); len(got) != 1 {
			t.Fatalf("only the 'a' decision itself is logged: %+v", got)
		}
	})

	t.Run("unavailable provider fails closed", func(t *testing.T) {
		sess := &session.Session{}
		a := New(DefaultPolicy(), interaction.Deny(), sess)
		err := a.Check(ctx, call("write_file", `{}`))
		var de *DeniedError
		if !errors.As(err, &de) {
			t.Fatalf("want DeniedError, got %v", err)
		}
		got := approvalsOf(sess)
		if len(got) != 1 || got[0].Source != "fail-closed" || got[0].Decision != "deny" {
			t.Fatalf("fail-closed event: %+v", got)
		}
	})

	t.Run("prompt abort propagates as turn interrupt", func(t *testing.T) {
		sess := &session.Session{}
		a := New(DefaultPolicy(), &scriptIA{err: interaction.ErrAborted}, sess)
		err := a.Check(ctx, call("shell", `{}`))
		if !errors.Is(err, interaction.ErrAborted) {
			t.Fatalf("want ErrAborted, got %v", err)
		}
		if got := approvalsOf(sess); len(got) != 0 {
			t.Fatalf("aborted prompt is not a decision: %+v", got)
		}
	})
}
