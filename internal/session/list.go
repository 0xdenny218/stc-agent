package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Info 是历史会话列表的一条：路径、标题（最后一条 title 事件）、首条
// user 消息首行（无标题时的展示回退）与文件修改时间。
type Info struct {
	Path      string
	Title     string
	FirstUser string
	ModTime   time.Time
}

// List 列出 dir 下全部 *.jsonl 会话，按修改时间降序（最新在前）。空目录
// 或目录不存在返回空切片。逐行解析代价可接受：列出时只读每个文件一遍，
// title 取最后一条（last-wins），user 取第一条。
func List(dir string) ([]Info, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("sessions: %w", err)
	}
	var out []Info
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		p := filepath.Join(dir, e.Name())
		info := Info{Path: p}
		if st, err := e.Info(); err == nil {
			info.ModTime = st.ModTime()
		}
		f, err := os.Open(p)
		if err != nil {
			continue // 单个坏文件不拖垮整个列表
		}
		dec := json.NewDecoder(f)
		for {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				break // EOF / 坏行：读到哪算哪（列出是尽力而为）
			}
			ev, err := parseEvent(raw)
			if err != nil {
				continue
			}
			switch {
			case ev.Type == EventTitle:
				info.Title = ev.Title
			case ev.Type == EventMessage && info.FirstUser == "" &&
				ev.Message != nil && ev.Message.Role == "user":
				info.FirstUser = firstLine(ev.Message.Content)
			}
		}
		f.Close()
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out, nil
}

// Display 返回列表条目的展示标题：title 事件 > 首条 user 消息首行 > 文件名。
func (i Info) Display() string {
	if i.Title != "" {
		return i.Title
	}
	if i.FirstUser != "" {
		return i.FirstUser
	}
	return filepath.Base(i.Path)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
