// Package model implements the chat-model client as a fiber: it injects the
// config service and provides the chat service, so a config re-provision
// reloads the client (spec D4). Non-streaming OpenAI-compatible wire format
// (spec D8).
package model

import (
	"bytes"
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	Messages []Message
	Tools    []ToolSpec // 空则线格式省略 tools 字段
}

type ChatResponse struct {
	Message Message
}

// ChatService 由 model fiber 提供；非流式（spec D8）。
type ChatService interface {
	Chat(ctx stdctx.Context, req ChatRequest) (*ChatResponse, error)
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

type wireRequest struct {
	Model    string     `json:"model"`
	Messages []Message  `json:"messages"`
	Stream   bool       `json:"stream"`
	Tools    []wireTool `json:"tools,omitempty"`
}

type wireResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
}

func (c *client) Chat(ctx stdctx.Context, req ChatRequest) (*ChatResponse, error) {
	ctx, cancel := stdctx.WithTimeout(ctx, c.timeout)
	defer cancel()

	wreq := wireRequest{Model: c.model, Messages: req.Messages, Stream: false}
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
		if errors.Is(ctx.Err(), stdctx.DeadlineExceeded) {
			return nil, &ChatError{Kind: KindTimeout, Err: err}
		}
		return nil, &ChatError{Kind: KindTransport, Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, &ChatError{Kind: KindHTTP, Status: resp.StatusCode, Body: string(snippet)}
	}
	var wresp wireResponse
	if err := json.NewDecoder(resp.Body).Decode(&wresp); err != nil {
		return nil, &ChatError{Kind: KindProtocol, Err: err}
	}
	if len(wresp.Choices) == 0 {
		return nil, &ChatError{Kind: KindProtocol, Err: errors.New("response has no choices")}
	}
	return &ChatResponse{Message: wresp.Choices[0].Message}, nil
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
