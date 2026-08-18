// Package model implements the chat-model client as a fiber: it injects the
// config service and provides the chat service, so a config re-provision
// reloads the client (spec D4). Streaming (SSE) is the only model path
// (spec D14); tool_calls deltas are assembled by index.
package model

import (
	"bufio"
	"bytes"
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/0xdenny218/stc-agent/internal/config"
	stc "github.com/0xdenny218/stc-go"
)

// KeyChat 是模型对话服务。
var KeyChat = stc.NewKey[ChatService]("chat")

// Message 是一条对话消息。ToolCalls/ToolCallID 承载 OpenAI 工具调用协议：
// assistant 消息带 ToolCalls，tool 消息以 ToolCallID 回指。
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // 恒为 "function"
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON 字符串
}

// ToolSpec 是工具的线格式描述（JSON Schema 参数）。
type ToolSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

type ChatRequest struct {
	// System 是组装好的 system prompt（spec D16 段落注册表的产物）；
	// 非空时作为第一条 system 消息上线，不进会话历史。
	System   string
	Messages []Message
	Tools    []ToolSpec // 空则线格式省略 tools 字段
}

// Usage 是一次请求的 token 计量。Model 由客户端回填（线格式的 usage 里
// 没有模型字段），供会话事件日志区分换模型前后的用量。
type Usage struct {
	Model            string `json:"model,omitempty"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
}

type ChatResponse struct {
	Message      Message
	FinishReason string
	Usage        Usage
}

// ChatService 由 model fiber 提供；流式为唯一路径（spec D14）。onDelta
// 按到达顺序收到每个内容增量（可为 nil）；工具调用增量不回调，组装完整
// 后经返回值给出。
type ChatService interface {
	Chat(ctx stdctx.Context, req ChatRequest, onDelta func(delta string)) (*ChatResponse, error)
	Model() string
}

// ErrKind 区分失败类别，契约按类别断言。
type ErrKind int

const (
	KindTransport ErrKind = iota // 网络层失败
	KindTimeout                  // 请求超时
	KindHTTP                     // 非 200 响应
	KindProtocol                 // 200 但报文不合协议
)

// ChatError 是归一化的调用错误：Kind 供调用方分类，Err 保留原始原因。
type ChatError struct {
	Kind   ErrKind
	Status int    // KindHTTP 时的状态码
	Body   string // KindHTTP 时的响应节选
	Err    error
}

func (e *ChatError) Error() string {
	switch e.Kind {
	case KindTimeout:
		return fmt.Sprintf("model: request timeout: %v", e.Err)
	case KindHTTP:
		return fmt.Sprintf("model: HTTP %d: %s", e.Status, e.Body)
	case KindProtocol:
		return fmt.Sprintf("model: protocol error: %v", e.Err)
	default:
		return fmt.Sprintf("model: transport error: %v", e.Err)
	}
}

func (e *ChatError) Unwrap() error { return e.Err }

type client struct {
	baseURL string
	apiKey  string
	model   string
	timeout time.Duration
	hc      *http.Client
}

func NewClient(baseURL, apiKey, model string, timeout time.Duration) ChatService {
	return &client{baseURL: baseURL, apiKey: apiKey, model: model, timeout: timeout, hc: &http.Client{}}
}

func (c *client) Model() string { return c.model }

type wireToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type wireTool struct {
	Type     string           `json:"type"` // 恒为 "function"
	Function wireToolFunction `json:"function"`
}

type wireStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireRequest struct {
	Model         string             `json:"model"`
	Messages      []Message          `json:"messages"`
	Stream        bool               `json:"stream"`
	StreamOptions *wireStreamOptions `json:"stream_options,omitempty"`
	Tools         []wireTool         `json:"tools,omitempty"`
}

// wireChunk 是一个 SSE 分片。增量语义：content 追加；tool_calls 按 index
// 归位、各字段分片追加；usage 只在收尾分片出现（stream_options 开启后）。
type wireChunk struct {
	Choices []struct {
		Delta struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

func (c *client) Chat(ctx stdctx.Context, req ChatRequest, onDelta func(string)) (*ChatResponse, error) {
	ctx, cancel := stdctx.WithTimeout(ctx, c.timeout)
	defer cancel()

	wreq := wireRequest{
		Model:         c.model,
		Messages:      req.Messages,
		Stream:        true,
		StreamOptions: &wireStreamOptions{IncludeUsage: true},
	}
	if req.System != "" {
		// system 消息只在线格式头部出现，不入会话事件日志。
		wreq.Messages = append([]Message{{Role: "system", Content: req.System}}, req.Messages...)
	}
	for _, t := range req.Tools {
		wreq.Tools = append(wreq.Tools, wireTool{
			Type:     "function",
			Function: wireToolFunction{Name: t.Name, Description: t.Description, Parameters: t.Parameters},
		})
	}
	body, err := json.Marshal(wreq)
	if err != nil {
		return nil, fmt.Errorf("model: encode request: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("model: build request: %w", err)
	}
	hreq.Header.Set("Authorization", "Bearer "+c.apiKey)
	hreq.Header.Set("Content-Type", "application/json")

	resp, err := c.hc.Do(hreq)
	if err != nil {
		return nil, mapErr(err, ctx)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, &ChatError{Kind: KindHTTP, Status: resp.StatusCode, Body: string(snippet)}
	}
	res, err := readStream(resp.Body, onDelta)
	if err != nil {
		var ce *ChatError
		if errors.As(err, &ce) {
			return nil, ce
		}
		return nil, mapErr(err, ctx)
	}
	res.Usage.Model = c.model
	return res, nil
}

// mapErr 归一化传输层错误：超时单独归类；调用方主动取消（Ctrl-C 中断
// 本轮、级联重载）原样返回 ctx 错误，供上层区分于真实故障。
func mapErr(err error, ctx stdctx.Context) error {
	if errors.Is(ctx.Err(), stdctx.DeadlineExceeded) {
		return &ChatError{Kind: KindTimeout, Err: err}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return &ChatError{Kind: KindTransport, Err: err}
}

// thinkFilter 净化流式 content 中的内联思考段（GLM 真实冒烟发现）：
// 推理模型偶发把思考内联进 content——完整 "<think>...</think>" 段，或
// 孤立的 "</think>" 过渡标记（思考走了 reasoning_content、只剩收尾
// 标记落进 content）。增量流上的小型状态机：思考段整体抑制；孤立收尾
// 标记剥除；可能构成标记前缀的分片尾巴暂扣，待下一分片或收尾再判定——
// 分片边界劈开标记也不泄漏。
type thinkFilter struct {
	inThink bool
	pending string
}

const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

// feed 处理一个 content 分片，返回可见文本（可为空）。
func (f *thinkFilter) feed(s string) string {
	s = f.pending + s
	f.pending = ""
	var out strings.Builder
	for {
		if f.inThink {
			i := strings.Index(s, thinkClose)
			if i < 0 {
				f.pending = holdMarkerPrefix(s, thinkClose)
				return out.String()
			}
			s = s[i+len(thinkClose):]
			f.inThink = false
			continue
		}
		o, c := strings.Index(s, thinkOpen), strings.Index(s, thinkClose)
		switch {
		case c >= 0 && (o < 0 || c < o): // 孤立收尾标记：只剥标记本身
			out.WriteString(s[:c])
			s = s[c+len(thinkClose):]
		case o >= 0: // 思考段开始：此后整体抑制
			out.WriteString(s[:o])
			s = s[o+len(thinkOpen):]
			f.inThink = true
		default: // 无完整标记：暂扣可能是标记前缀的尾巴
			f.pending = holdMarkerPrefix(s, thinkOpen, thinkClose)
			out.WriteString(s[:len(s)-len(f.pending)])
			return out.String()
		}
	}
}

// flush 在流收尾时调用：思考段外的暂扣尾巴是字面文本（没有后续分片能
// 补全标记了），放出；思考段内的尾巴随段一并吞掉。
func (f *thinkFilter) flush() string {
	p := f.pending
	f.pending = ""
	if f.inThink {
		return ""
	}
	return p
}

// holdMarkerPrefix 返回 s 的最长后缀，要求它是任一标记的真前缀（完整
// 标记已在上面的分支处理，不会走到这里）。
func holdMarkerPrefix(s string, markers ...string) string {
	best := 0
	for _, m := range markers {
		for n := len(m) - 1; n > best; n-- {
			if len(s) >= n && s[len(s)-n:] == m[:n] {
				best = n
				break
			}
		}
	}
	return s[len(s)-best:]
}

// assembler 把 SSE 增量分片组装成完整应答：content 追加、tool_calls 按
// index 归位（各字段分片追加）、finish_reason 与收尾 usage 记录。
type assembler struct {
	onDelta      func(string)
	content      strings.Builder
	think        thinkFilter
	toolCalls    []ToolCall
	role         string
	finishReason string
	usage        Usage
	sawChunk     bool
}

// readStream 消费 SSE 流；只认 "data:" 行，[DONE] 或 EOF 收尾。
func readStream(body io.Reader, onDelta func(string)) (*ChatResponse, error) {
	a := &assembler{onDelta: onDelta}
	r := bufio.NewReader(body)
	for {
		line, err := r.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			done, hErr := a.handleLine(trimmed)
			if hErr != nil {
				return nil, hErr
			}
			if done {
				return a.result()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return a.result()
			}
			return nil, err
		}
	}
}

// handleLine 处理一行 SSE；返回 done = 收到 [DONE] 收尾。
func (a *assembler) handleLine(line []byte) (done bool, err error) {
	data, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return false, nil // event:/注释/空行：忽略
	}
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("[DONE]")) {
		return true, nil
	}
	var chunk wireChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return false, &ChatError{Kind: KindProtocol, Err: fmt.Errorf("bad SSE chunk: %w", err)}
	}
	a.sawChunk = true
	for _, ch := range chunk.Choices {
		d := ch.Delta
		if d.Role != "" {
			a.role = d.Role
		}
		if d.Content != "" {
			visible := a.think.feed(d.Content)
			if visible != "" {
				a.content.WriteString(visible)
				if a.onDelta != nil {
					a.onDelta(visible)
				}
			}
		}
		for _, dt := range d.ToolCalls {
			for len(a.toolCalls) <= dt.Index {
				a.toolCalls = append(a.toolCalls, ToolCall{})
			}
			tc := &a.toolCalls[dt.Index]
			tc.ID += dt.ID
			if dt.Type != "" {
				tc.Type = dt.Type
			}
			tc.Function.Name += dt.Function.Name
			tc.Function.Arguments += dt.Function.Arguments
		}
		if ch.FinishReason != "" {
			a.finishReason = ch.FinishReason
		}
	}
	if chunk.Usage != nil {
		a.usage = *chunk.Usage
	}
	return false, nil
}

func (a *assembler) result() (*ChatResponse, error) {
	// 收尾放出思考段外暂扣的字面尾巴（如恰好停在 "<thi" 的分片）。
	if tail := a.think.flush(); tail != "" {
		a.content.WriteString(tail)
		if a.onDelta != nil {
			a.onDelta(tail)
		}
	}
	if !a.sawChunk {
		return nil, &ChatError{Kind: KindProtocol, Err: errors.New("stream ended without chunks")}
	}
	role := a.role
	if role == "" {
		role = "assistant"
	}
	return &ChatResponse{
		Message:      Message{Role: role, Content: a.content.String(), ToolCalls: a.toolCalls},
		FinishReason: a.finishReason,
		Usage:        a.usage,
	}, nil
}

// Component 是模型 fiber：inject config → 提供 chat。config 重提供时
// 本 fiber 级联重载，chat 换成新实例。
func Component() stc.Component {
	return stc.Component{
		Name:    "model",
		Inject:  []stc.Key{config.KeyConfig},
		Provide: []stc.Key{KeyChat},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			cfg, err := stc.Service[config.Config](c, config.KeyConfig)
			if err != nil {
				return nil, err
			}
			if _, err := c.Provide(KeyChat, NewClient(cfg.BaseURL, cfg.APIKey, cfg.Model, cfg.Timeout)); err != nil {
				return nil, err
			}
			return nil, nil
		},
	}
}
