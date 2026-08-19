package tools

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	stc "github.com/0xdenny218/stc-go"
)

// SpillComponent 往 spill 目录写一个文件（草稿/笔记/产物暂存）。name 只
// 允许单段文件名，杜绝路径穿越（../ 或 / 都拒绝）。写工具，默认策略
// 询问。
func SpillComponent(dir string) stc.Component {
	return component(Tool{
		Name:        "spill",
		Description: fmt.Sprintf("write a scratch file into %s (drafts, notes, artifacts); returns the written path", dir),
		Parameters: json.RawMessage(`{"type":"object","properties":{
  "name": {"type":"string","description":"file name; a single path segment, no separators or .."},
  "content": {"type":"string","description":"file content"}
},"required":["name","content"]}`),
		Invoke: func(_ stdctx.Context, args json.RawMessage) (string, error) {
			var a struct {
				Name    string `json:"name"`
				Content string `json:"content"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return "", err
			}
			if a.Name == "" {
				return "", errors.New("invalid arguments: name is required")
			}
			if a.Name == "." || a.Name == ".." || filepath.Base(a.Name) != a.Name {
				return "", fmt.Errorf("spill: invalid name %q (must be a single path segment)", a.Name)
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", err
			}
			p := filepath.Join(dir, a.Name)
			if err := os.WriteFile(p, []byte(a.Content), 0o600); err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), p), nil
		},
		// 预览与 write_file 同族：目标文件存在则 diff，草稿新建则从空创建。
		Preview: func(args json.RawMessage) string {
			var a struct {
				Name    string `json:"name"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(args, &a); err != nil || a.Name == "" {
				return ""
			}
			p := filepath.Join(dir, a.Name)
			old, err := os.ReadFile(p)
			if err != nil {
				return unifiedDiff(p+" (new file)", "", a.Content)
			}
			return unifiedDiff(p, string(old), a.Content)
		},
	})
}
