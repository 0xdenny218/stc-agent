package web_test

import (
	stdctx "context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/0xdenny218/stc-agent/internal/tools"
	"github.com/0xdenny218/stc-agent/internal/web"
	stc "github.com/0xdenny218/stc-go"
)

// loadWeb 装载 toolset + 给定 web 工具，返回按名查找器。
func loadWeb(t *testing.T, comps ...stc.Component) func(string) tools.Tool {
	t.Helper()
	root := stc.New()
	t.Cleanup(func() { root.Close() })
	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 5*time.Second)
	defer cancel()
	for _, c := range append([]stc.Component{tools.ToolsetComponent()}, comps...) {
		f := root.Load(c)
		if err := f.Ready(ctx); err != nil {
			t.Fatalf("fiber %s: %v", f.Name(), err)
		}
	}
	ts, err := stc.Service[*tools.Toolset](root, tools.KeyTools)
	if err != nil {
		t.Fatalf("resolve toolset: %v", err)
	}
	return func(name string) tools.Tool {
		t.Helper()
		tool, ok := ts.Lookup(name)
		if !ok {
			t.Fatalf("tool %q not registered", name)
		}
		return tool
	}
}

func args(t *testing.T, kv map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(kv)
	if err != nil {
		t.Fatal(err)
	}
	return json.RawMessage(b)
}

func TestWebFetch(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		switch r.URL.Path {
		case "/echo":
			w.Write([]byte("hello world"))
		case "/err":
			w.WriteHeader(http.StatusNotFound)
		case "/binary":
			w.Write([]byte("a\x00b"))
		case "/big":
			w.Write([]byte(strings.Repeat("x", 20)))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	bg := stdctx.Background()

	lookup := loadWeb(t, web.WebFetchComponent(web.Options{AllowPrivate: true}))
	fetch := lookup("web_fetch")

	t.Run("fetch content", func(t *testing.T) {
		out, err := fetch.Invoke(bg, args(t, map[string]any{"url": srv.URL + "/echo"}))
		if err != nil {
			t.Fatalf("web_fetch: %v", err)
		}
		if out != "hello world" {
			t.Fatalf("web_fetch output: %q", out)
		}
	})

	t.Run("http status error", func(t *testing.T) {
		if _, err := fetch.Invoke(bg, args(t, map[string]any{"url": srv.URL + "/err"})); err == nil ||
			!strings.Contains(err.Error(), "status 404") {
			t.Fatalf("expected 404 error, got %v", err)
		}
	})

	t.Run("binary rejected", func(t *testing.T) {
		if _, err := fetch.Invoke(bg, args(t, map[string]any{"url": srv.URL + "/binary"})); err == nil ||
			!strings.Contains(err.Error(), "binary content") {
			t.Fatalf("expected binary error, got %v", err)
		}
	})

	t.Run("truncated by max bytes", func(t *testing.T) {
		lookup2 := loadWeb(t, web.WebFetchComponent(web.Options{AllowPrivate: true, MaxBytes: 8}))
		out, err := lookup2("web_fetch").Invoke(bg, args(t, map[string]any{"url": srv.URL + "/big"}))
		if err != nil {
			t.Fatalf("web_fetch: %v", err)
		}
		if len(out) != 8+len("\n... (truncated)") || !strings.HasSuffix(out, "... (truncated)") {
			t.Fatalf("truncated output: %q", out)
		}
	})

	t.Run("unsupported scheme", func(t *testing.T) {
		if _, err := fetch.Invoke(bg, args(t, map[string]any{"url": "file:///etc/passwd"})); err == nil ||
			!strings.Contains(err.Error(), "unsupported scheme") {
			t.Fatalf("expected scheme error, got %v", err)
		}
	})

	t.Run("bad args", func(t *testing.T) {
		if _, err := fetch.Invoke(bg, args(t, map[string]any{})); err == nil ||
			!strings.Contains(err.Error(), "url is required") {
			t.Fatalf("expected missing-url error, got %v", err)
		}
	})

	_ = gotURL
}

func TestWebFetchSSRFBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("secret"))
	}))
	defer srv.Close()

	lookup := loadWeb(t, web.WebFetchComponent(web.Options{})) // AllowPrivate=false
	if _, err := lookup("web_fetch").Invoke(stdctx.Background(), args(t, map[string]any{"url": srv.URL})); err == nil ||
		!strings.Contains(err.Error(), "blocked") {
		t.Fatalf("expected SSRF block for loopback, got %v", err)
	}
}

func TestWebSearch(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		switch gotQuery {
		case "golang":
			w.Write([]byte(`{"Heading":"Go (programming language)","AbstractText":"Go is a statically typed language.","AbstractURL":"https://en.wikipedia.org/wiki/Go","RelatedTopics":[{"Text":"Go — text/template","FirstURL":"https://go.dev/pkg/text/template"}]}`))
		default:
			w.Write([]byte(`{"Heading":"","RelatedTopics":[]}`))
		}
	}))
	defer srv.Close()
	bg := stdctx.Background()

	lookup := loadWeb(t, web.WebSearchComponent(web.Options{
		AllowPrivate: true,
		SearchURL:    srv.URL + "/?q={q}&format=json",
	}))
	search := lookup("web_search")

	t.Run("query substituted and rendered", func(t *testing.T) {
		out, err := search.Invoke(bg, args(t, map[string]any{"query": "golang"}))
		if err != nil {
			t.Fatalf("web_search: %v", err)
		}
		if gotQuery != "golang" {
			t.Fatalf("server saw q=%q", gotQuery)
		}
		if !strings.Contains(out, "Go is a statically typed language") ||
			!strings.Contains(out, "- Go — text/template") {
			t.Fatalf("web_search output: %q", out)
		}
	})

	t.Run("empty results", func(t *testing.T) {
		out, err := search.Invoke(bg, args(t, map[string]any{"query": "zzzzz"}))
		if err != nil {
			t.Fatalf("web_search: %v", err)
		}
		if out != "(no results)" {
			t.Fatalf("web_search empty: %q", out)
		}
	})

	t.Run("non-JSON surfaced", func(t *testing.T) {
		srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("not json at all"))
		}))
		defer srv2.Close()
		lookup2 := loadWeb(t, web.WebSearchComponent(web.Options{AllowPrivate: true, SearchURL: srv2.URL + "/?q={q}"}))
		out, err := lookup2("web_search").Invoke(bg, args(t, map[string]any{"query": "x"}))
		if err != nil {
			t.Fatalf("web_search: %v", err)
		}
		if !strings.Contains(out, "not JSON") {
			t.Fatalf("web_search non-json: %q", out)
		}
	})

	t.Run("bad args", func(t *testing.T) {
		if _, err := search.Invoke(bg, args(t, map[string]any{"query": "  "})); err == nil ||
			!strings.Contains(err.Error(), "query is required") {
			t.Fatalf("expected missing-query error, got %v", err)
		}
	})
}
