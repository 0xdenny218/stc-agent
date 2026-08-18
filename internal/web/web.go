// Package web implements the network tools (spec M10): web_fetch grabs a
// URL's contents, web_search queries a key-free search endpoint. Both go
// through one shared fetch core with SSRF protection — private/loopback/
// link-local targets are blocked unless AllowPrivate (tests inject local
// httptest servers). Remote tools, so the default approval policy asks.
package web

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/0xdenny218/stc-agent/internal/tools"
	stc "github.com/0xdenny218/stc-go"
)

const (
	defaultMaxBytes  = 1 << 20
	defaultTimeout   = 15 * time.Second
	defaultSearchURL = "https://api.duckduckgo.com/?q={q}&format=json&no_html=1&skip_disambig=1"
)

// Options 配置网络工具。
type Options struct {
	Client       *http.Client
	MaxBytes     int64         // 响应体上限（默认 1 MiB）
	Timeout      time.Duration // 请求超时（默认 15s）
	AllowPrivate bool          // 放行私网/回环地址（本地测试注入端点用）
	SearchURL    string        // web_search 端点模板，{q} 替换为查询
}

// withDefaults 填充零值选项。
func (o Options) withDefaults() Options {
	if o.Client == nil {
		o.Client = http.DefaultClient
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = defaultMaxBytes
	}
	if o.Timeout <= 0 {
		o.Timeout = defaultTimeout
	}
	if o.SearchURL == "" {
		o.SearchURL = defaultSearchURL
	}
	return o
}

// WebFetchComponent 抓取 URL 内容的工具。
func WebFetchComponent(opts Options) stc.Component {
	o := opts.withDefaults()
	return stc.Component{
		Name:   "tool:web_fetch",
		Inject: []stc.Key{tools.KeyTools},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			ts, err := stc.Service[*tools.Toolset](c, tools.KeyTools)
			if err != nil {
				return nil, err
			}
			return register(ts, "web_fetch",
				"fetch the contents of a URL (http/https only)",
				json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","description":"URL to fetch"}},"required":["url"]}`),
				func(ctx stdctx.Context, args json.RawMessage) (string, error) {
					var a struct {
						URL string `json:"url"`
					}
					if err := decodeArgs(args, &a); err != nil {
						return "", err
					}
					if a.URL == "" {
						return "", errors.New("invalid arguments: url is required")
					}
					return o.fetch(ctx, a.URL)
				}), nil
		},
	}
}

// WebSearchComponent 搜索工具：GET SearchURL 模板（{q} 替换为查询），把
// 结果渲染成紧凑文本。默认端点 DuckDuckGo Instant Answer，免 key。
func WebSearchComponent(opts Options) stc.Component {
	o := opts.withDefaults()
	return stc.Component{
		Name:   "tool:web_search",
		Inject: []stc.Key{tools.KeyTools},
		Apply: func(c *stc.Context) (stc.Inverse, error) {
			ts, err := stc.Service[*tools.Toolset](c, tools.KeyTools)
			if err != nil {
				return nil, err
			}
			return register(ts, "web_search",
				"search the web and return a short summary of results",
				json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"search query"}},"required":["query"]}`),
				func(ctx stdctx.Context, args json.RawMessage) (string, error) {
					var a struct {
						Query string `json:"query"`
					}
					if err := decodeArgs(args, &a); err != nil {
						return "", err
					}
					if strings.TrimSpace(a.Query) == "" {
						return "", errors.New("invalid arguments: query is required")
					}
					u := strings.ReplaceAll(o.SearchURL, "{q}", url.QueryEscape(a.Query))
					body, err := o.fetch(ctx, u)
					if err != nil {
						return "", err
					}
					return renderSearch(body), nil
				}), nil
		},
	}
}

// register 在工具表里注册一个 web 工具。
func register(ts *tools.Toolset, name, desc string, params json.RawMessage, invoke func(stdctx.Context, json.RawMessage) (string, error)) stc.Inverse {
	return ts.Register(name, tools.Tool{Name: name, Description: desc, Parameters: params, Invoke: invoke})
}

func decodeArgs(args json.RawMessage, v any) error {
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("invalid arguments: %v", err)
	}
	return nil
}

// fetch 是共享抓取核心：scheme 白名单 + SSRF 门 + 超时 + 大小上限 +
// 二进制拒收。
func (o Options) fetch(ctx stdctx.Context, rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("web: bad URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("web: unsupported scheme %q (only http/https)", u.Scheme)
	}
	if ssrfBlocked(u.Hostname(), o.AllowPrivate) {
		return "", fmt.Errorf("web: %s is a private/loopback address (blocked)", u.Hostname())
	}
	ctx, cancel := stdctx.WithTimeout(ctx, o.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("web: %w", err)
	}
	resp, err := o.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("web: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("web: %s returned status %d", u.Host, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, o.MaxBytes+1))
	if err != nil {
		return "", fmt.Errorf("web: read body: %w", err)
	}
	if strings.ContainsRune(string(body), '\x00') {
		return "", fmt.Errorf("web: %s returned binary content", u.Host)
	}
	trunc := ""
	if int64(len(body)) > o.MaxBytes {
		body = body[:o.MaxBytes]
		trunc = "\n... (truncated)"
	}
	return string(body) + trunc, nil
}

// ssrfBlocked 判断主机是否命中私网/回环/链路本地等应阻断的地址。解析
// 失败按阻断处理（宁可误伤不可外连内网）。
func ssrfBlocked(host string, allowPrivate bool) bool {
	if allowPrivate {
		return false
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return true
	}
	for _, ip := range ips {
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return true
		}
	}
	return false
}

// renderSearch 把 DDG Instant Answer JSON 渲染成紧凑文本。
func renderSearch(body string) string {
	var d struct {
		Heading      string `json:"Heading"`
		Answer       string `json:"Answer"`
		AbstractText string `json:"AbstractText"`
		AbstractURL  string `json:"AbstractURL"`
		Related      []struct {
			Text     string `json:"Text"`
			FirstURL string `json:"FirstURL"`
		} `json:"RelatedTopics"`
	}
	// 端点返回非法 JSON 时给出原始正文，便于排查（不静默吞掉）。
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		return fmt.Sprintf("web_search: unexpected response (not JSON):\n%s", body)
	}
	var b strings.Builder
	if d.Answer != "" {
		fmt.Fprintf(&b, "Answer: %s\n", d.Answer)
	}
	if d.AbstractText != "" {
		fmt.Fprintf(&b, "%s\n", d.AbstractText)
		if d.AbstractURL != "" {
			fmt.Fprintf(&b, "(%s)\n", d.AbstractURL)
		}
	}
	if len(d.Related) == 0 && d.AbstractText == "" && d.Answer == "" && d.Heading == "" {
		return "(no results)"
	}
	for _, r := range d.Related {
		if r.Text != "" {
			fmt.Fprintf(&b, "- %s", r.Text)
			if r.FirstURL != "" {
				fmt.Fprintf(&b, " (%s)", r.FirstURL)
			}
			b.WriteByte('\n')
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}
