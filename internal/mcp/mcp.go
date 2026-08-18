// Package mcp 把 MCP stdio server 粘合成工具 fiber（spec M8）：子进程
// 上跑 JSON-RPC（换行分隔），握手后 tools/list 的每个工具注册进稳定
// toolset（名字 mcp__<server>__<tool>），调用转发 tools/call。server
// 断开（stdio EOF/进程退出）= 工具即时从 toolset 消失。
//
// Forbidden 边界：只做 stdio 传输；MCP over HTTP/SSE 不在范围内。
package mcp

import (
	"bufio"
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
)

// protocolVersion 是握手声明的 MCP 协议版本（2024-11-05 是工具面
// （tools/list、tools/call）稳定的最老广泛支持版本）。
const protocolVersion = "2024-11-05"

// Server 是一个 MCP stdio server 的启动描述。
type Server struct {
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Env     []string `json:"env,omitempty"` // KEY=VALUE，追加在进程环境之后
}

// rpcRequest / rpcResponse 是线上帧（换行分隔的 JSON-RPC 2.0）。
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      uint64          `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("mcp rpc error %d: %s", e.Code, e.Message) }

// client 是一个 server 子进程上的 JSON-RPC 连接：写串行化，读循环把
// 响应按 id 派发给等待中的调用；done 在连接断开时关闭。
type client struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser

	wmu     sync.Mutex
	nextID  atomic.Uint64
	pmu     sync.Mutex
	pending map[uint64]chan rpcResponse
	closed  bool // 读循环已终止（pmu 保护；与 pending 冲刷同临界区）

	done     chan struct{}
	closeErr error // 读循环终止的原因（EOF 或进程错误）
}

// start 拉起子进程并完成 initialize 握手（含 initialized 通知）。
func start(srv Server) (*client, error) {
	cmd := exec.Command(srv.Command, srv.Args...)
	if len(srv.Env) > 0 {
		cmd.Env = append(os.Environ(), srv.Env...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &lockedWriter{w: &stderr}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %q: %w", srv.Command, err)
	}
	cl := &client{
		cmd:     cmd,
		stdin:   stdin,
		pending: map[uint64]chan rpcResponse{},
		done:    make(chan struct{}),
	}
	go cl.readLoop(stdout)

	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 10*time.Second)
	defer cancel()
	params, _ := json.Marshal(map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "stc-agent", "version": "0.1.1"},
	})
	if _, err := cl.call(ctx, "initialize", params); err != nil {
		cl.kill()
		return nil, fmt.Errorf("initialize: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	if err := cl.notify("notifications/initialized", nil); err != nil {
		cl.kill()
		return nil, fmt.Errorf("initialized notification: %w", err)
	}
	return cl, nil
}

// readLoop 逐行读响应并按 id 派发；server→client 的请求回方法未支持
// （ping 例外，回空结果）；EOF/错误时关闭 done 并唤醒全部等待者。
func (cl *client) readLoop(stdout io.Reader) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var head struct {
			ID     *uint64 `json:"id"`
			Method string  `json:"method"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			continue // 坏行丢弃：协议上不该出现，出现也不可恢复
		}
		if head.Method != "" && head.ID == nil {
			continue // server 通知：无需应答
		}
		if head.Method != "" { // server→client 请求
			resp := rpcResponse{JSONRPC: "2.0", ID: *head.ID}
			if head.Method == "ping" {
				resp.Result = json.RawMessage(`{}`)
			} else {
				resp.Error = &rpcError{Code: -32601, Message: "method not supported"}
			}
			_ = cl.write(resp)
			continue
		}
		var resp rpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			continue
		}
		cl.pmu.Lock()
		ch, ok := cl.pending[resp.ID]
		delete(cl.pending, resp.ID)
		cl.pmu.Unlock()
		if ok {
			ch <- resp
		}
	}
	cl.closeErr = sc.Err()
	cl.pmu.Lock()
	cl.closed = true
	for id, ch := range cl.pending {
		delete(cl.pending, id)
		ch <- rpcResponse{Error: &rpcError{Code: -32000, Message: "mcp server disconnected"}}
	}
	cl.pmu.Unlock()
	close(cl.done)
}

func (cl *client) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	cl.wmu.Lock()
	defer cl.wmu.Unlock()
	_, err = cl.stdin.Write(append(b, '\n'))
	return err
}

// call 发一个请求并等响应；ctx 取消或连接断开都会返回错误。
func (cl *client) call(ctx stdctx.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	id := cl.nextID.Add(1)
	ch := make(chan rpcResponse, 1)
	cl.pmu.Lock()
	if cl.closed { // 注册与冲刷同锁：断开后登记的等待者不会被漏派
		cl.pmu.Unlock()
		return nil, errors.New("mcp server disconnected")
	}
	cl.pending[id] = ch
	cl.pmu.Unlock()

	if err := cl.write(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		cl.pmu.Lock()
		delete(cl.pending, id)
		cl.pmu.Unlock()
		return nil, err
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		cl.pmu.Lock()
		delete(cl.pending, id)
		cl.pmu.Unlock()
		return nil, ctx.Err()
	}
}

func (cl *client) notify(method string, params json.RawMessage) error {
	return cl.write(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

// kill 终止子进程并等它退出（inverse 路径用）。
func (cl *client) kill() {
	_ = cl.stdin.Close()
	if cl.cmd.Process != nil {
		_ = cl.cmd.Process.Kill()
	}
	_ = cl.cmd.Wait()
}

// toolDef 是 tools/list 结果里的一条。
type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// callResult 是 tools/call 的结果（content 数组 + isError 标记）。
type callResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// listTools 拉取 server 的工具目录。
func (cl *client) listTools(ctx stdctx.Context) ([]toolDef, error) {
	res, err := cl.call(ctx, "tools/list", json.RawMessage(`{}`))
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []toolDef `json:"tools"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("tools/list result: %w", err)
	}
	return out.Tools, nil
}

// callTool 调一个工具并把 content 里的 text 段拼成结果字符串；
// isError 结果转成 Go error（回灌模型自我纠正）。
func (cl *client) callTool(ctx stdctx.Context, name string, args json.RawMessage) (string, error) {
	params, _ := json.Marshal(struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{Name: name, Arguments: args})
	res, err := cl.call(ctx, "tools/call", params)
	if err != nil {
		return "", err
	}
	var cr callResult
	if err := json.Unmarshal(res, &cr); err != nil {
		return "", fmt.Errorf("tools/call result: %w", err)
	}
	var sb strings.Builder
	for _, c := range cr.Content {
		if c.Type == "text" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(c.Text)
		}
	}
	if cr.IsError {
		return "", errors.New(sb.String())
	}
	return sb.String(), nil
}

// toolName 给 MCP 工具一个线格式安全的全局名：
// mcp__<server>__<tool>，非法字符（OpenAI 兼容 API 要求
// [a-zA-Z0-9_-]）折叠为下划线。
func toolName(server, tool string) string {
	n := "mcp__" + server + "__" + tool
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, n)
}

// Component 把一个 MCP stdio server 装成 fiber：握手、tools/list 注册
// 进 toolset；server 断开 = 工具即时消失（审批门视角它们是未知工具，
// 默认询问——fail-closed 语义自然成立）。onStatus 上报断开等运行期
// 事件（nil 丢弃）。
func Component(srv Server, onStatus func(string)) stc.Component {
	return stc.Component{
		Name:   "mcp:" + srv.Name,
		Inject: []stc.Key{tools.KeyTools},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			ts, err := stc.Service[*tools.Toolset](c, tools.KeyTools)
			if err != nil {
				return nil, err
			}
			cl, err := start(srv)
			if err != nil {
				return nil, fmt.Errorf("mcp %s: %w", srv.Name, err)
			}
			ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 10*time.Second)
			defs, err := cl.listTools(ctx)
			cancel()
			if err != nil {
				cl.kill()
				return nil, fmt.Errorf("mcp %s: %w", srv.Name, err)
			}
			var unregs []stc.Inverse
			for _, d := range defs {
				def := d
				unregs = append(unregs, ts.Register(toolName(srv.Name, def.Name), tools.Tool{
					Name:        toolName(srv.Name, def.Name),
					Description: fmt.Sprintf("[mcp:%s] %s", srv.Name, def.Description),
					Parameters:  def.InputSchema,
					Invoke: func(ctx stdctx.Context, args json.RawMessage) (string, error) {
						return cl.callTool(ctx, def.Name, args)
					},
				}))
			}
			// 断开监听：EOF/进程退出 → 工具即时失效（注册逆幂等，
			// 与组件逆并发调用安全）。
			go func() {
				<-cl.done
				for _, u := range unregs {
					_ = u()
				}
				if onStatus != nil {
					onStatus(fmt.Sprintf("[mcp] %s disconnected; %d tools removed", srv.Name, len(unregs)))
				}
			}()
			return func() error {
				cl.kill()
				for _, u := range unregs {
					_ = u()
				}
				return nil
			}, nil
		},
	}
}

// lockedWriter 串行化 stderr 收集（启动失败时拼进错误）。
type lockedWriter struct {
	mu sync.Mutex
	w  *strings.Builder
}

func (lw *lockedWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.w.Write(p)
}
