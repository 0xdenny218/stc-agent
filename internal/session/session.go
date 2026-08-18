// Package session implements the session spine (spec D13): an append-only
// event log of typed events (messages, token usage, ...), written through to
// a JSONL transcript as a revertible effect. The in-memory message list is
// demoted to a projection of the log; resume = replay the projection. It
// injects nothing, so model-switch cascades never reload it — surviving
// history is the dependency graph's own conclusion.
package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/0xdenny218/stc-agent/internal/model"
	stc "github.com/0xdenny218/stc-go"
)

// KeySession 是会话历史服务。
var KeySession = stc.NewKey[*Session]("session")

// ErrClosed 表示会话已随 fiber 卸载而关闭。
var ErrClosed = errors.New("session: closed")

// 事件类型。凡进入模型请求的内容都必须能由日志重建（"model-visible means
// logged"）；审批决定、compaction 摘要等都是新增事件类型。
const (
	EventMessage  = "message"  // 对话消息（user/assistant/tool）
	EventUsage    = "usage"    // 一次模型请求的 token 用量
	EventApproval = "approval" // 一次审批决定（spec D15）
)

// Approval 是一次审批决定。只有询问类决定与硬性拒绝落日志：策略放行是
// 静态默认路径，不产生事件（策略本身由配置可知，逐条记录只是噪声）。
type Approval struct {
	Tool      string `json:"tool"`
	Arguments string `json:"arguments,omitempty"`
	Decision  string `json:"decision"` // "allow" | "deny"
	Source    string `json:"source"`   // "user" | "policy" | "fail-closed"
}

// Event 是事件日志的一行。同一事件只填与 Type 对应的载荷字段。
type Event struct {
	Type     string         `json:"type"`
	Message  *model.Message `json:"message,omitempty"`
	Usage    *model.Usage   `json:"usage,omitempty"`
	Approval *Approval      `json:"approval,omitempty"`
}

// Session 是事件日志 + 投影：events 是源，msgs 是消息投影的缓存。
// Add/AddUsage 与关闭都走同一把锁。
type Session struct {
	mu     sync.Mutex
	events []Event
	msgs   []model.Message
	w      *bufio.Writer // nil = 纯内存
	f      *os.File
	closed bool
}

// Add 追加一条消息事件；有 transcript 时立即写穿一行。
func (s *Session) Add(m model.Message) error {
	return s.append(Event{Type: EventMessage, Message: &m})
}

// AddUsage 追加一条 token 用量事件（每次模型请求一条）。
func (s *Session) AddUsage(u model.Usage) error {
	return s.append(Event{Type: EventUsage, Usage: &u})
}

// AddApproval 追加一条审批决定事件（spec D15：决定与来源可审计）。
func (s *Session) AddApproval(a Approval) error {
	return s.append(Event{Type: EventApproval, Approval: &a})
}

func (s *Session) append(ev Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.events = append(s.events, ev)
	if ev.Type == EventMessage {
		s.msgs = append(s.msgs, *ev.Message)
	}
	if s.w != nil {
		if err := json.NewEncoder(s.w).Encode(ev); err != nil {
			return fmt.Errorf("session: write transcript: %w", err)
		}
		if err := s.w.Flush(); err != nil {
			return fmt.Errorf("session: flush transcript: %w", err)
		}
	}
	return nil
}

// History 返回消息投影的副本。
func (s *Session) History() []model.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Message(nil), s.msgs...)
}

// Events 返回事件日志的副本（载荷一并拷贝）。
func (s *Session) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	for i, ev := range s.events {
		out[i] = ev
		if ev.Message != nil {
			m := *ev.Message
			out[i].Message = &m
		}
		if ev.Usage != nil {
			u := *ev.Usage
			out[i].Usage = &u
		}
		if ev.Approval != nil {
			a := *ev.Approval
			out[i].Approval = &a
		}
	}
	return out
}

func (s *Session) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.f == nil {
		return nil
	}
	return errors.Join(s.w.Flush(), s.f.Close())
}

// Component 是会话 fiber。transcriptPath 非空时：装载先 replay 已有文件
// （--resume 的语义，replay 即投影），再以追加模式写穿；卸载 = 关文件
// （可逆效应）。
func Component(transcriptPath string) stc.Component {
	return stc.Component{
		Name:    "session",
		Provide: []stc.Key{KeySession},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			s := &Session{}
			if transcriptPath != "" {
				events, err := replay(transcriptPath)
				if err != nil {
					return nil, err
				}
				for _, ev := range events {
					s.events = append(s.events, ev)
					if ev.Type == EventMessage {
						s.msgs = append(s.msgs, *ev.Message)
					}
				}
				f, err := os.OpenFile(transcriptPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
				if err != nil {
					return nil, fmt.Errorf("session: open transcript: %w", err)
				}
				s.f, s.w = f, bufio.NewWriter(f)
			}
			if _, err := c.Provide(KeySession, s); err != nil {
				_ = s.close()
				return nil, err
			}
			if err := c.Effect(func() stc.Inverse {
				return s.close
			}); err != nil {
				_ = s.close()
				return nil, err
			}
			return nil, nil
		},
	}
}

// replay 逐行读出已有 transcript 并投影；坏行与未知事件类型报错返回
// （不静默容忍）。
func replay(path string) ([]Event, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("session: read transcript: %w", err)
	}
	defer f.Close()
	var events []Event
	dec := json.NewDecoder(f)
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				return events, nil
			}
			return nil, fmt.Errorf("session: corrupt transcript %s: %w", path, err)
		}
		ev, err := parseEvent(raw)
		if err != nil {
			return nil, fmt.Errorf("session: corrupt transcript %s: %w", path, err)
		}
		events = append(events, ev)
	}
}

// parseEvent 解析一行事件；兼容 v0.1 的裸 message 行（无 type 字段）。
func parseEvent(raw json.RawMessage) (Event, error) {
	var ev Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return Event{}, err
	}
	switch ev.Type {
	case EventMessage:
		if ev.Message == nil {
			return Event{}, errors.New("message event without message")
		}
		return ev, nil
	case EventUsage:
		if ev.Usage == nil {
			return Event{}, errors.New("usage event without usage")
		}
		return ev, nil
	case EventApproval:
		if ev.Approval == nil {
			return Event{}, errors.New("approval event without approval")
		}
		return ev, nil
	case "":
		var m model.Message
		if err := json.Unmarshal(raw, &m); err != nil || m.Role == "" {
			return Event{}, errors.New("line has neither type nor role")
		}
		return Event{Type: EventMessage, Message: &m}, nil
	default:
		return Event{}, fmt.Errorf("unknown event type %q", ev.Type)
	}
}
