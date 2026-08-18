package cli

import (
	stdctx "context"
	"io"
	"testing"
	"time"
)

// stubRunner 满足 loop.Runner；退出路径测试不跑轮次。
type stubRunner struct{}

func (stubRunner) RunTurn(stdctx.Context, string, io.Writer) error { return nil }

// newPipeConsole 手工构造管道模式 Console（不启输入泵），供直调 serve
// 的测试注入 lines/aborts 事件。
func newPipeConsole() *Console {
	return &Console{
		out:    io.Discard,
		lines:  make(chan string),
		aborts: make(chan struct{}, 4),
		gate:   make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
}

// 提示符处 Ctrl-C：第一次丢行并提示，第二次直接退出；中间读入一行则
// 计数清零（对齐 Claude Code 的双击退出）。
func TestPromptCtrlCExitsOnSecondPress(t *testing.T) {
	console := newPipeConsole()
	exited := make(chan struct{})
	go func() {
		serve(stdctx.Background(), console, stubRunner{}, nil, exited)
	}()

	press := func() {
		select {
		case console.aborts <- struct{}{}:
		case <-time.After(time.Second):
			t.Fatal("aborts channel blocked")
		}
	}

	press() // 第一次：丢行，提示再按一次
	select {
	case <-console.Done():
		t.Fatal("first Ctrl-C must not exit")
	case <-exited:
		t.Fatal("first Ctrl-C must not end serve")
	case <-time.After(100 * time.Millisecond):
	}

	// 读入一行清零计数：退出要求的是"连续"两次。
	console.lines <- "hello"
	// stubRunner 立即返回；等 serve 回到读取态。
	time.Sleep(50 * time.Millisecond)
	press()
	select {
	case <-console.Done():
		t.Fatal("single Ctrl-C after a line must not exit")
	case <-time.After(100 * time.Millisecond):
	}

	press() // 连续第二次：退出
	select {
	case <-console.Done():
	case <-time.After(time.Second):
		t.Fatal("second consecutive Ctrl-C must exit")
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("serve must return after exit")
	}
}
