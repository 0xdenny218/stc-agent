// Package session implements conversation history as a fiber: an in-memory
// message log plus an optional JSONL transcript registered as a revertible
// effect (spec D7). It injects nothing, so model-switch cascades never
// reload it — surviving history is the dependency graph's own conclusion.
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

// Session 是消息历史；Add 与关闭都走同一把锁。
type Session struct {
	mu     sync.Mutex
	msgs   []model.Message
	w      *bufio.Writer // nil = 纯内存
	f      *os.File
	closed bool
}

// Add 追加一条消息；有 transcript 时立即写穿一行。
func (s *Session) Add(m model.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.msgs = append(s.msgs, m)
	if s.w != nil {
		if err := json.NewEncoder(s.w).Encode(m); err != nil {
			return fmt.Errorf("session: write transcript: %w", err)
		}
		if err := s.w.Flush(); err != nil {
			return fmt.Errorf("session: flush transcript: %w", err)
		}
	}
	return nil
}

// History 返回历史副本。
func (s *Session) History() []model.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Message(nil), s.msgs...)
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
// （--resume 的语义），再以追加模式写穿；卸载 = 关文件（可逆效应）。
func Component(transcriptPath string) stc.Component {
	return stc.Component{
		Name:    "session",
		Provide: []stc.Key{KeySession},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			s := &Session{}
			if transcriptPath != "" {
				msgs, err := replay(transcriptPath)
				if err != nil {
					return nil, err
				}
				s.msgs = msgs
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

// replay 逐行读出已有 transcript；坏行报错返回（不静默容忍）。
func replay(path string) ([]model.Message, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("session: read transcript: %w", err)
	}
	defer f.Close()
	var msgs []model.Message
	dec := json.NewDecoder(f)
	for {
		var m model.Message
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				return msgs, nil
			}
			return nil, fmt.Errorf("session: corrupt transcript %s: %w", path, err)
		}
		msgs = append(msgs, m)
	}
}
