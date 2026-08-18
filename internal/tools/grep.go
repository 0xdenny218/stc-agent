package tools

import (
	"bytes"
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	stc "github.com/0xdenny218/stc-go"
)

// GrepComponent 正则逐行搜索工具：path 是文件或目录，目录需 recursive
// 打开。命中输出 "path:line: 内容"，二进制文件跳过。只读本地，默认
// 策略放行。
func GrepComponent() stc.Component {
	return component(Tool{
		Name:        "grep",
		Description: "search a file or directory for lines matching a regexp; recursive search on a directory requires recursive=true. Binary files are skipped.",
		Parameters: json.RawMessage(`{"type":"object","properties":{
  "pattern": {"type":"string","description":"Go regular expression"},
  "path": {"type":"string","description":"file or directory to search"},
  "recursive": {"type":"boolean","description":"recurse when path is a directory (default false)"}
},"required":["pattern","path"]}`),
		Invoke: func(_ stdctx.Context, args json.RawMessage) (string, error) {
			var a struct {
				Pattern   string `json:"pattern"`
				Path      string `json:"path"`
				Recursive bool   `json:"recursive"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return "", err
			}
			if a.Pattern == "" || a.Path == "" {
				return "", errors.New("invalid arguments: pattern and path are required")
			}
			re, err := regexp.Compile(a.Pattern)
			if err != nil {
				return "", fmt.Errorf("grep: bad pattern: %w", err)
			}
			info, err := os.Stat(a.Path)
			if err != nil {
				return "", err
			}
			var b strings.Builder
			count := 0
			if !info.IsDir() {
				count, err = grepFile(&b, a.Path, re)
				if err != nil {
					return "", err
				}
			} else if a.Recursive {
				err = filepath.WalkDir(a.Path, func(p string, d fs.DirEntry, err error) error {
					if err != nil {
						return nil
					}
					if d.IsDir() {
						return nil
					}
					n, gerr := grepFile(&b, p, re)
					if gerr == nil {
						count += n
					}
					return nil
				})
				if err != nil {
					return "", fmt.Errorf("grep: %w", err)
				}
			} else {
				return "", errors.New("grep: path is a directory; set recursive=true to search it")
			}
			if count == 0 {
				return "(no matches)", nil
			}
			return capOutput(strings.TrimSuffix(b.String(), "\n")), nil
		},
	})
}

// grepFile 单文件搜索：二进制（内容含 NUL）跳过，命中行以
// "path:line: 内容" 追加进 b，返回命中数。
func grepFile(b *strings.Builder, path string, re *regexp.Regexp) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return 0, nil // 二进制
	}
	count := 0
	for i, line := range bytes.Split(data, []byte("\n")) {
		if re.Match(line) {
			fmt.Fprintf(b, "%s:%d: %s\n", path, i+1, line)
			count++
		}
	}
	return count, nil
}
