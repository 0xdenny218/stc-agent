package tools

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	stc "github.com/0xdenny218/stc-go"
)

// maxOutput 是单个工具结果的字节上限，超出截断并标注，避免一次性撑爆
// 上下文。
const maxOutput = 32 << 10

func capOutput(s string) string {
	if len(s) <= maxOutput {
		return s
	}
	return s[:maxOutput] + "\n... (output truncated)"
}

func decodeArgs(args json.RawMessage, v any) error {
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("invalid arguments: %v", err)
	}
	return nil
}

// ReadFileComponent 读取文件内容的工具。
func ReadFileComponent() stc.Component {
	return component(Tool{
		Name:        "read_file",
		Description: "read the contents of a file",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"path of the file to read"}},"required":["path"]}`),
		Invoke: func(_ stdctx.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path string `json:"path"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return "", err
			}
			if a.Path == "" {
				return "", errors.New("invalid arguments: path is required")
			}
			b, err := os.ReadFile(a.Path)
			if err != nil {
				return "", err
			}
			return capOutput(string(b)), nil
		},
	})
}

// WriteFileComponent 写文件的工具。
func WriteFileComponent() stc.Component {
	return component(Tool{
		Name:        "write_file",
		Description: "write content to a file (overwrites; parent directory must exist)",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"path of the file to write"},"content":{"type":"string","description":"content to write"}},"required":["path","content"]}`),
		Invoke: func(_ stdctx.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return "", err
			}
			if a.Path == "" {
				return "", errors.New("invalid arguments: path is required")
			}
			if err := os.WriteFile(a.Path, []byte(a.Content), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), a.Path), nil
		},
		// 预览 = 现有内容 vs 新内容的统一 diff；新文件即"从空创建"。
		Preview: func(args json.RawMessage) string {
			var a struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(args, &a); err != nil || a.Path == "" {
				return ""
			}
			old, err := os.ReadFile(a.Path)
			if err != nil {
				return unifiedDiff(a.Path+" (new file)", "", a.Content)
			}
			return unifiedDiff(a.Path, string(old), a.Content)
		},
	})
}
