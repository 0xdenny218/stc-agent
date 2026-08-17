package cli

import (
	"bufio"
	stdctx "context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/0xdenny218/stc-agent/internal/model"
	"github.com/0xdenny218/stc-agent/internal/session"
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

// Component 是 REPL fiber：inject chat/session/commands，每个装载周期起
// 一个循环 goroutine；逆 = 取消周期并等其退出。
func Component(console *Console) stc.Component {
	return stc.Component{
		Name:   "cli",
		Inject: []stc.Key{model.KeyChat, session.KeySession, KeyCommands},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			chat, err := stc.Service[model.ChatService](c, model.KeyChat)
			if err != nil {
				return nil, err
			}
			sess, err := stc.Service[*session.Session](c, session.KeySession)
			if err != nil {
				return nil, err
			}
			reg, err := stc.Service[*Registry](c, KeyCommands)
			if err != nil {
				return nil, err
			}
			ctx, cancel := stdctx.WithCancel(stdctx.Background())
			exited := make(chan struct{})
			go loop(ctx, console, chat, sess, reg, exited)
			return func() error {
				cancel()
				<-exited
				return nil
			}, nil
		},
	}
}

func loop(ctx stdctx.Context, console *Console, chat model.ChatService, sess *session.Session, reg *Registry, exited chan<- struct{}) {
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
				handled, err := reg.Dispatch(ctx, w, line)
				switch {
				case !handled:
					name, _, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
					fmt.Fprintf(w, "unknown command: /%s\n", name)
				case err != nil:
					fmt.Fprintf(w, "error: %v\n", err)
				}
			default:
				if !turn(ctx, w, chat, sess, line) {
					return // 周期被取消（级联重载）；新周期会接替
				}
			}
			fmt.Fprint(w, "> ")
		}
	}
}

// turn 跑一轮问答；false 表示周期 ctx 在调用中途被取消。
func turn(ctx stdctx.Context, w io.Writer, chat model.ChatService, sess *session.Session, input string) bool {
	if err := sess.Add(model.Message{Role: "user", Content: input}); err != nil {
		fmt.Fprintf(w, "error: %v\n", err)
		return true
	}
	resp, err := chat.Chat(ctx, model.ChatRequest{Messages: sess.History()})
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		fmt.Fprintf(w, "error: %v\n", err)
		return true
	}
	if err := sess.Add(model.Message{Role: "assistant", Content: resp.Message.Content}); err != nil {
		fmt.Fprintf(w, "error: %v\n", err)
		return true
	}
	fmt.Fprintln(w, resp.Message.Content)
	return true
}
