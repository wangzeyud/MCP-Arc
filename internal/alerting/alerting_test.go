package alerting

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wangzeyud/mcp-arc/internal/config"
)

func sign(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestSendSignsAndDelivers(t *testing.T) {
	var gotBody []byte
	var gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSig = r.Header.Get("X-MCPArc-Signature")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	s := New(config.AlertingConfig{
		Enabled:   true,
		Webhook:   config.WebhookConfig{URL: srv.URL, Secret: "topsecret"},
		TimeoutMs: 2000,
	})
	s.Send(EventConfigReloaded, map[string]any{"note": "hi"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if gotSig != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if gotSig == "" {
		t.Fatal("webhook was not called")
	}
	if !strings.HasPrefix(gotSig, "sha256=") {
		t.Fatalf("bad signature header: %q", gotSig)
	}
	if gotSig != "sha256="+sign(gotBody, "topsecret") {
		t.Fatal("signature mismatch")
	}
	var env map[string]any
	if err := json.Unmarshal(gotBody, &env); err != nil {
		t.Fatal(err)
	}
	if env["event"] != EventConfigReloaded {
		t.Fatalf("event field = %v", env["event"])
	}
}

func TestSendDisabledNoop(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer srv.Close()
	s := New(config.AlertingConfig{Enabled: false, Webhook: config.WebhookConfig{URL: srv.URL}})
	s.Send(EventConfigReloaded, nil)
	time.Sleep(200 * time.Millisecond)
	if hit {
		t.Fatal("disabled sender must not deliver")
	}
}

func TestEventFiltering(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer srv.Close()
	s := New(config.AlertingConfig{
		Enabled:   true,
		Webhook:   config.WebhookConfig{URL: srv.URL},
		Events:    []string{EventAuditError},
		TimeoutMs: 2000,
	})
	s.Send(EventConfigReloaded, nil) // not in allow-list
	time.Sleep(200 * time.Millisecond)
	if hit {
		t.Fatal("event not in allow-list should be dropped")
	}
	s.Send(EventAuditError, nil)
	time.Sleep(200 * time.Millisecond)
	if !hit {
		t.Fatal("allowed event should be delivered")
	}
}

func TestFailOpenOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	s := New(config.AlertingConfig{
		Enabled:   true,
		Webhook:   config.WebhookConfig{URL: srv.URL},
		TimeoutMs: 2000,
	})
	// Must not panic or block the caller.
	s.Send(EventConfigReloaded, nil)
	time.Sleep(300 * time.Millisecond)
}
