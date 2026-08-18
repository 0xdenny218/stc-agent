// Command echo 是一个最小 MCP stdio server（spec M8 的示例与 e2e 陪练）：
// 一个工具 "echo"，把 text 参数原样返回。协议 = 换行分隔的 JSON-RPC 2.0
// over stdio。
//
// ECHO_DIE_AFTER_MS：设置后经过该毫秒数自动退出（断开=工具失效语义的
// 测试辅助）。ECHO_DIE_AFTER_CALLS：应答满 N 次 tools/call 后退出
// （e2e 用的确定性断开——应答已冲刷，调用方先拿到结果再看到 EOF）。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *uint64         `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string  `json:"jsonrpc"`
	ID      uint64  `json:"id"`
	Result  any     `json:"result,omitempty"`
	Error   *errObj `json:"error,omitempty"`
}

type errObj struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func main() {
	if v := os.Getenv("ECHO_DIE_AFTER_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			time.AfterFunc(time.Duration(ms)*time.Millisecond, func() { os.Exit(0) })
		}
	}
	dieAfterCalls := 0
	if v := os.Getenv("ECHO_DIE_AFTER_CALLS"); v != "" {
		dieAfterCalls, _ = strconv.Atoi(v)
	}
	calls := 0

	out := bufio.NewWriter(os.Stdout)
	reply := func(r response) {
		b, _ := json.Marshal(r)
		fmt.Fprintf(out, "%s\n", b)
		out.Flush()
	}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var req request
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			continue
		}
		if req.ID == nil {
			continue // 通知（如 notifications/initialized）：无应答
		}
		id := *req.ID
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(req.Params, &p)
			reply(response{JSONRPC: "2.0", ID: id, Result: map[string]any{
				"protocolVersion": p.ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": "mcp-echo", "version": "0.1.0"},
			}})
		case "ping":
			reply(response{JSONRPC: "2.0", ID: id, Result: map[string]any{}})
		case "tools/list":
			reply(response{JSONRPC: "2.0", ID: id, Result: map[string]any{
				"tools": []map[string]any{{
					"name":        "echo",
					"description": "echo back the text argument",
					"inputSchema": map[string]any{
						"type":       "object",
						"properties": map[string]any{"text": map[string]string{"type": "string"}},
						"required":   []string{"text"},
					},
				}},
			}})
		case "tools/call":
			var p struct {
				Name      string `json:"name"`
				Arguments struct {
					Text string `json:"text"`
				} `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil || p.Name != "echo" {
				reply(response{JSONRPC: "2.0", ID: id, Error: &errObj{Code: -32602, Message: "unknown tool or bad arguments"}})
				continue
			}
			reply(response{JSONRPC: "2.0", ID: id, Result: map[string]any{
				"content": []map[string]string{{"type": "text", "text": p.Arguments.Text}},
			}})
			calls++
			if dieAfterCalls > 0 && calls >= dieAfterCalls {
				os.Exit(0)
			}
		default:
			reply(response{JSONRPC: "2.0", ID: id, Error: &errObj{Code: -32601, Message: "method not found"}})
		}
	}
}
