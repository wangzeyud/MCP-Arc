package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dodoyu-sama/mcp-arc/internal/mask"
)

func TestParseFindings(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []mask.Finding
		wantErr bool
	}{
		{"plain", `{"findings":[{"path":"user.email","type":"person"}]}`, []mask.Finding{{Path: "user.email", Type: "person"}}, false},
		{"fenced", "```json\n" + `{"findings":[{"path":"$.note","type":"other"}]}` + "\n```", []mask.Finding{{Path: "note", Type: "other"}}, false},
		{"prose around json", `Sure! {"findings":[]} hope that helps`, []mask.Finding{}, false},
		{"no json at all", "nothing sensitive here", nil, false},
		{"wrong type", `{"findings":[{"path":123}]}`, nil, true},
	}
	for _, c := range cases {
		got, err := parseFindings(c.content)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, wantErr = %v", c.name, err, c.wantErr)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: findings = %#v, want %#v", c.name, got, c.want)
		}
	}
}

func TestNormalizePath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"$.note", "note"},
		{"arguments.user.email", "user.email"},
		{"params.a", "a"},
		{"  note  ", "note"},
		{`"note"`, "note"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizePath(c.in); got != c.want {
			t.Errorf("normalizePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestChatURL(t *testing.T) {
	cases := []struct{ endpoint, want string }{
		{"https://api.openai.com/v1", "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com/v1/", "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com/v1/chat/completions", "https://api.openai.com/v1/chat/completions"},
	}
	for _, c := range cases {
		client := &Client{cfg: Config{Endpoint: c.endpoint}}
		if got := client.chatURL(); got != c.want {
			t.Errorf("chatURL(%q) = %q, want %q", c.endpoint, got, c.want)
		}
	}
}

func TestNewRequiresEndpoint(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("expected an error when no endpoint is configured")
	}
}

// mockServer answers like an OpenAI-compatible endpoint and records what it saw.
func mockServer(t *testing.T, status int, body string) (*httptest.Server, *int32, func(int) map[string]any) {
	t.Helper()
	var hits int32
	var last map[string]any
	var mu = make(chan struct{}, 1)
	mu <- struct{}{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		<-mu
		last = req
		mu <- struct{}{}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	get := func(int) map[string]any {
		<-mu
		defer func() { mu <- struct{}{} }()
		return last
	}
	return srv, &hits, get
}

func chatResponse(content string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": map[string]string{"content": content}}},
	})
	return string(b)
}

func TestDetectCachesIdenticalPayloads(t *testing.T) {
	srv, hits, lastReq := mockServer(t, 200, chatResponse(`{"findings":[{"path":"note","type":"person"}]}`))
	defer srv.Close()

	c, err := New(Config{Endpoint: srv.URL, APIKey: "sk-test", Model: "m", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.Detect(map[string]any{"note": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "note" {
		t.Fatalf("unexpected findings: %#v", got)
	}
	req := lastReq(0)
	if req["model"] != "m" {
		t.Errorf("model not forwarded: %#v", req["model"])
	}

	if _, err := c.Detect(map[string]any{"note": "hi"}); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Errorf("identical payload should hit the cache, endpoint called %d times", n)
	}

	if _, err := c.Detect(map[string]any{"note": "different"}); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(hits); n != 2 {
		t.Errorf("different payload should call the endpoint again, got %d calls", n)
	}
}

func TestDetectSkipsOversizedPayloads(t *testing.T) {
	srv, hits, _ := mockServer(t, 200, chatResponse(`{"findings":[]}`))
	defer srv.Close()

	c, err := New(Config{Endpoint: srv.URL, Timeout: 2 * time.Second, MaxBytes: 32})
	if err != nil {
		t.Fatal(err)
	}
	big := map[string]any{"blob": strings.Repeat("x", 128)}
	got, err := c.Detect(big)
	if err != nil {
		t.Fatalf("oversized payload must fail open, got %v", err)
	}
	if got != nil {
		t.Errorf("oversized payload should be skipped, got %#v", got)
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Errorf("oversized payload must not reach the endpoint, got %d calls", n)
	}
}

func TestDetectFailsOpenThenOpensBreaker(t *testing.T) {
	srv, hits, _ := mockServer(t, 500, `{"error":"boom"}`)
	defer srv.Close()

	c, err := New(Config{Endpoint: srv.URL, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.Detect(map[string]any{"a": "b"}); err == nil {
		t.Fatal("expected an error from a failing endpoint")
	}

	// The breaker should now absorb subsequent calls instead of adding latency.
	got, err := c.Detect(map[string]any{"a": "b"})
	if err != nil {
		t.Errorf("breaker should short-circuit without an error, got %v", err)
	}
	if got != nil {
		t.Errorf("breaker should return no findings, got %#v", got)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Errorf("breaker must stop calling the endpoint, got %d calls", n)
	}
}
