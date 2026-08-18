package tools

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"

	stc "github.com/0xdenny218/stc-go"
)

// GlobComponent 按模式列出文件（支持 ** 递归）。模式相对 root（默认
// "."）；"**" 匹配任意层目录。只读本地，默认策略放行。
func GlobComponent() stc.Component {
	return component(Tool{
		Name:        "glob",
		Description: "list files matching a glob pattern under a root directory (default .); use ** for recursion, e.g. **/*.go",
		Parameters: json.RawMessage(`{"type":"object","properties":{
  "pattern": {"type":"string","description":"glob pattern relative to root"},
  "root": {"type":"string","description":"directory to search under (default .)"}
},"required":["pattern"]}`),
		Invoke: func(_ stdctx.Context, args json.RawMessage) (string, error) {
			var a struct {
				Pattern string `json:"pattern"`
				Root    string `json:"root"`
			}
			if err := decodeArgs(args, &a); err != nil {
				return "", err
			}
			if a.Pattern == "" {
				return "", errors.New("invalid arguments: pattern is required")
			}
			root := a.Root
			if root == "" {
				root = "."
			}
			segs := splitSegs(a.Pattern)
			if len(segs) == 0 {
				return "", errors.New("invalid arguments: pattern is empty")
			}
			var hits []string
			err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil // 不可达文件跳过，不炸整次搜索
				}
				if d.IsDir() {
					return nil
				}
				rel, err := filepath.Rel(root, p)
				if err != nil {
					return nil
				}
				if matchSegs(segs, splitSegs(rel)) {
					hits = append(hits, p)
				}
				return nil
			})
			if err != nil {
				return "", fmt.Errorf("glob: %w", err)
			}
			if len(hits) == 0 {
				return "(no matches)", nil
			}
			return capOutput(strings.Join(hits, "\n")), nil
		},
	})
}

// splitSegs 把路径切成分段（统一 / 分隔；"**" 保留为独立分段）。
func splitSegs(p string) []string {
	p = filepath.ToSlash(filepath.Clean(p))
	p = strings.TrimPrefix(p, "./")
	if p == "." || p == "/" || p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// matchSegs 逐段匹配：普通段用 path.Match（* 不跨 /），** 匹配任意层。
func matchSegs(segs, rel []string) bool {
	if len(segs) == 0 {
		return len(rel) == 0
	}
	if segs[0] == "**" {
		for i := 0; i <= len(rel); i++ {
			if matchSegs(segs[1:], rel[i:]) {
				return true
			}
		}
		return false
	}
	if len(rel) == 0 {
		return false
	}
	ok, _ := path.Match(segs[0], rel[0])
	return ok && matchSegs(segs[1:], rel[1:])
}
