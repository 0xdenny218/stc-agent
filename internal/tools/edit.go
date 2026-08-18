package tools

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	stc "github.com/0xdenny218/stc-go"
)

// EditComponent 精确字符串替换工具（str_replace，Claude Code edit 语义的
// 最小版）：old_string 必须唯一，除非 replace_all 打开——避免模型在长文
// 件里误替换。读改写一体，与 write_file 同族（默认策略询问）。
func EditComponent() stc.Component {
	return component(Tool{
		Name:        "edit",
		Description: "replace an exact string in a file (old_string must be unique unless replace_all)",
		Parameters: json.RawMessage(`{"type":"object","properties":{
  "path": {"type":"string","description":"path of the file to edit"},
  "old_string": {"type":"string","description":"exact text to replace (must appear exactly once unless replace_all)"},
  "new_string": {"type":"string","description":"replacement text"},
  "replace_all": {"type":"boolean","description":"replace every occurrence (default false)"}
},"required":["path","old_string","new_string"]}`),
		Invoke: func(_ stdctx.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path       string `json:"path"`
				Old        string `json:"old_string"`
				New        string `json:"new_string"`
				ReplaceAll bool   `json:"replace_all"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return "", err
			}
			if a.Path == "" || a.Old == "" {
				return "", errors.New("invalid arguments: path and old_string are required")
			}
			b, err := os.ReadFile(a.Path)
			if err != nil {
				return "", err
			}
			content := string(b)
			n := strings.Count(content, a.Old)
			if n == 0 {
				return "", fmt.Errorf("edit: old_string not found in %s", a.Path)
			}
			if n > 1 && !a.ReplaceAll {
				return "", fmt.Errorf("edit: old_string appears %d times in %s; provide more context or set replace_all", n, a.Path)
			}
			replaced := strings.ReplaceAll(content, a.Old, a.New)
			if err := os.WriteFile(a.Path, []byte(replaced), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("replaced %d occurrence(s) in %s (%d bytes)", n, a.Path, len(replaced)), nil
		},
	})
}
