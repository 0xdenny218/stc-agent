package cli

import (
	"bufio"
	stdctx "context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/0xdenny218/stc-agent/internal/loop"
	stc "github.com/0xdenny218/stc-go"
)

// Console 是跨装载周期共享的终端通道：读取泵只建一次，fiber 重载（如换
// 模型触发的级联）只换消费 goroutine，已缓冲的输入不丢。
type Console struct {
	out   io.Writer
	lines chan string
	done  chan struct{}

	doneOnce sync.Once
}

func NewConsole(in io.Reader, out io.Writer) *Console {
	c := &Console{
		out:   out,
		lines: make(chan string, 16),
		done:  make(chan struct{}),
	}
	go c.pump(bufio.NewReader(in))
	return c
}

// pump 持续读行；EOF（含最后一段无换行的残留行）后关闭 lines。
func (c *Console) pump(r *bufio.Reader) {
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

// Done 在会话结束（stdin EOF 或 /quit）时关闭；只关一次，与重载无关。
func (c *Console) Done() <-chan struct{} { return c.done }

func (c *Console) signalDone() { c.doneOnce.Do(func() { close(c.done) }) }

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
	fmt.Fprint(w, "> ")
	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-console.lines:
			if !ok { // stdin EOF
				console.signalDone()
				return
			}
			line = strings.TrimSpace(line)
			switch {
			case line == "":
			case line == "/quit":
				console.signalDone()
				return
			case strings.HasPrefix(line, "/"):
				handled, err := Dispatch(ctx, w, line, reg)
				switch {
				case !handled:
					name, _, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
					fmt.Fprintf(w, "unknown command: /%s\n", name)
				case err != nil:
					fmt.Fprintf(w, "error: %v\n", err)
				}
			default:
				if err := r.RunTurn(ctx, line, w); err != nil {
					if ctx.Err() != nil {
						return // 周期被取消（级联重载）；新周期会接替
					}
					fmt.Fprintf(w, "error: %v\n", err)
				}
			}
			fmt.Fprint(w, "> ")
		}
	}
}
