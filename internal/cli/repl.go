package cli

import (
	"bufio"
	stdctx "context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"

	"github.com/0xdenny218/stc-agent/internal/interaction"
	"github.com/0xdenny218/stc-agent/internal/loop"
	stc "github.com/0xdenny218/stc-go"
	"github.com/peterh/liner"
)

// Console 是跨装载周期共享的终端通道（spec D18）：输入泵只建一次，fiber
// 重载（如换模型触发的级联）只换消费 goroutine，已缓冲的输入不丢。
// stdin 是终端时行交互走 liner（历史、光标编辑，参照 Claude Code CLI
// 保持简单）；是管道时退回 bufio 逐行——e2e 与 demo 的输入通路不变。
type Console struct {
	out    io.Writer
	tty    bool
	lnr    *liner.State
	lines  chan string
	aborts chan struct{}
	gate   chan struct{}
	done   chan struct{}

	doneOnce  sync.Once
	closeOnce sync.Once

	mu         sync.Mutex
	turnCancel stdctx.CancelFunc
}

func NewConsole(in io.Reader, out io.Writer) *Console {
	c := &Console{
		out:    out,
		lines:  make(chan string, 16),
		aborts: make(chan struct{}, 1),
		gate:   make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	if f, ok := in.(*os.File); ok && isTTY(f) {
		c.tty = true
		c.lnr = liner.NewLiner()
		c.lnr.SetCtrlCAborts(true)
		c.lnr.SetMultiLineMode(true)
		go c.pumpTTY()
	} else {
		go c.pumpLines(bufio.NewReader(in))
	}
	go c.watchInterrupts()
	return c
}

func isTTY(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// pumpLines 是管道模式输入泵：持续读行；EOF（含最后一段无换行的残留行）
// 后关闭 lines。
func (c *Console) pumpLines(r *bufio.Reader) {
	defer close(c.lines)
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			c.lines <- line
		}
		if err != nil {
			return
		}
	}
}

// pumpTTY 是终端模式输入泵：只在消费者就绪（gate）时进入 liner.Prompt，
// 使轮次执行期间终端保持熟模式——那时 Ctrl-C 落为 SIGINT，由
// watchInterrupts 取消当前轮而不是杀进程。提示符处的 Ctrl-C 由 liner
// 消化为 ErrPromptAborted（经 aborts 上报）；Ctrl-D 空行 = EOF。
func (c *Console) pumpTTY() {
	defer close(c.lines)
	for {
		select {
		case <-c.gate:
		case <-c.done:
			return
		}
		line, err := c.lnr.Prompt("> ")
		switch {
		case err == nil:
			if line != "" {
				c.lnr.AppendHistory(line)
			}
			select {
			case c.lines <- line:
			case <-c.done:
				return
			}
		case errors.Is(err, liner.ErrPromptAborted):
			select {
			case c.aborts <- struct{}{}:
			case <-c.done:
				return
			}
		default: // io.EOF（Ctrl-D 空行）或终端错误
			return
		}
	}
}

// watchInterrupts 把 SIGINT 路由为取消：轮次进行中取消当前轮（会话与其余
// fiber 存活）；空闲时退化为退出。
func (c *Console) watchInterrupts() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	defer signal.Stop(ch)
	for {
		select {
		case <-ch:
			c.mu.Lock()
			cancel := c.turnCancel
			c.mu.Unlock()
			if cancel != nil {
				cancel()
			} else {
				c.signalDone()
			}
		case <-c.done:
			return
		}
	}
}

// setTurnCancel 登记/撤下当前轮的取消函数（serve 在轮次边界调用）。
func (c *Console) setTurnCancel(cancel stdctx.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.turnCancel = cancel
}

// Done 在会话结束（stdin EOF、/quit、提示符处连按两次 Ctrl-C）时关闭；
// 只关一次，与重载无关。
func (c *Console) Done() <-chan struct{} { return c.done }

func (c *Console) signalDone() { c.doneOnce.Do(func() { close(c.done) }) }

// Close 释放终端状态（liner 恢复 termios）；幂等，程序退出前调用。
func (c *Console) Close() {
	c.closeOnce.Do(func() {
		if c.lnr != nil {
			_ = c.lnr.Close()
		}
	})
}

// Component 是 REPL fiber：inject runner（轮次执行）与 commands（命令分
// 发）。换模型时 runner 随级联重载，本 fiber 亦换周期；控制台外置故
// 输入不丢。
func Component(console *Console) stc.Component {
	return stc.Component{
		Name:   "cli",
		Inject: []stc.Key{loop.KeyRunner, KeyCommands},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			r, err := stc.Service[loop.Runner](c, loop.KeyRunner)
			if err != nil {
				return nil, err
			}
			reg, err := stc.Service[*Registry](c, KeyCommands)
			if err != nil {
				return nil, err
			}
			ctx, cancel := stdctx.WithCancel(stdctx.Background())
			exited := make(chan struct{})
			go serve(ctx, console, r, reg, exited)
			return func() error {
				cancel()
				<-exited
				return nil
			}, nil
		},
	}
}

func serve(ctx stdctx.Context, console *Console, r loop.Runner, reg *Registry, exited chan<- struct{}) {
	defer close(exited)
	w := console.out
	// 提示符处 Ctrl-C 连按两次直接退出（对齐 Claude Code）：第一次只丢
	// 当前行并提示，任何成功读入的行都会清零计数。轮次中的 Ctrl-C 走
	// SIGINT → 中断当前轮，不在此列。
	aborts := 0
	for {
		if console.tty {
			// 放行输入泵进入 Prompt（轮次执行期间不放行，终端保持熟
			// 模式，Ctrl-C 才走 SIGINT → 中断当前轮）。
			select {
			case console.gate <- struct{}{}:
			default:
			}
		} else {
			fmt.Fprint(w, "> ")
		}
		select {
		case <-ctx.Done():
			return
		case <-console.aborts: // 提示符处 Ctrl-C：丢行；连按两次退出
			aborts++
			if aborts >= 2 {
				console.signalDone()
				return
			}
			fmt.Fprintln(w, "^C (press Ctrl-C again to exit)")
		case line, ok := <-console.lines:
			if !ok { // stdin EOF（管道）或 Ctrl-D 空行（终端）
				console.signalDone()
				return
			}
			aborts = 0
			line = strings.TrimSpace(line)
			switch {
			case line == "":
			case line == "/quit" || line == "quit" || line == "exit" || line == "/exit":
				console.signalDone()
				return
			case strings.HasPrefix(line, "/"):
				// 用户命令（customcmd）可能整轮跑模型：与普通轮次同享
				// Ctrl-C 语义（中断命令而非杀会话）。
				turnCtx, cancel := stdctx.WithCancel(ctx)
				console.setTurnCancel(cancel)
				handled, err := Dispatch(turnCtx, w, line, reg)
				console.setTurnCancel(nil)
				cancel()
				switch {
				case !handled:
					name, _, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
					fmt.Fprintf(w, "unknown command: /%s\n", name)
				case err != nil:
					fmt.Fprintf(w, "error: %v\n", err)
				}
			default:
				turnCtx, cancel := stdctx.WithCancel(ctx)
				console.setTurnCancel(cancel)
				err := r.RunTurn(turnCtx, line, w)
				console.setTurnCancel(nil)
				cancel()
				switch {
				case err == nil:
				case ctx.Err() != nil:
					return // 周期被取消（级联重载）；新周期会接替
				case turnCtx.Err() != nil || errors.Is(err, interaction.ErrAborted):
					// Ctrl-C 中断当前轮（轮次中或审批提问处）：流式内容
					// 可能没有收尾换行。
					fmt.Fprintln(w, "\n^C turn interrupted")
				default:
					fmt.Fprintf(w, "error: %v\n", err)
				}
			}
		}
	}
}
