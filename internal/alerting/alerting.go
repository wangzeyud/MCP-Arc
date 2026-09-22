// Package alerting delivers outbound notifications (v0.7). The only sink today is
// an HTTP webhook; events are POSTed as JSON with an HMAC-SHA256 signature in the
// X-MCPArc-Signature header. Delivery is always best-effort and fail-open: a
// broken or slow endpoint never blocks the request path, audit writes, or masking.
package alerting

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/wangzeyud/mcp-arc/internal/config"
)

// Event types dispatched to the webhook.
const (
	EventConfigReloaded     = "config_reloaded"
	EventRestartRequired    = "restart_required"
	EventSensitiveDetected  = "sensitive_detected"
	EventRateLimited        = "rate_limited"
	EventAuditError         = "audit_error"
)

// Sender posts events to a configured webhook. It is safe for concurrent use.
type Sender struct {
	mu       sync.RWMutex
	enabled  bool
	url      string
	secret   string
	timeout  time.Duration
	eventSet map[string]bool // empty => all events
	httpc    *http.Client
}

// New builds a Sender from the alerting config. A sender is always returned (even
// when disabled) so callers can fire events unconditionally; Send is a no-op until
// the sender is enabled via Reload.
func New(cfg config.AlertingConfig) *Sender {
	s := &Sender{httpc: &http.Client{}}
	s.Reload(cfg)
	return s
}

// Reload updates the sender's configuration atomically.
func (s *Sender) Reload(cfg config.AlertingConfig) {
	to := cfg.TimeoutMs
	if to <= 0 {
		to = 5000
	}
	set := map[string]bool{}
	for _, e := range cfg.Events {
		set[e] = true
	}
	s.mu.Lock()
	s.enabled = cfg.Enabled && cfg.Webhook.URL != ""
	s.url = cfg.Webhook.URL
	s.secret = cfg.Webhook.Secret
	s.timeout = time.Duration(to) * time.Millisecond
	s.eventSet = set
	s.mu.Unlock()
}

// Enabled reports whether the sender will actually dispatch events.
func (s *Sender) Enabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.enabled
}

// Send dispatches an event asynchronously (fail-open). A disabled sender or an
// event not in the configured allow-list is silently dropped.
func (s *Sender) Send(eventType string, payload map[string]any) {
	s.mu.RLock()
	enabled := s.enabled
	url := s.url
	secret := s.secret
	timeout := s.timeout
	allowAll := len(s.eventSet) == 0
	ok := s.eventSet[eventType]
	s.mu.RUnlock()
	if !enabled || (!allowAll && !ok) {
		return
	}
	go s.deliver(eventType, url, secret, timeout, payload)
}

func (s *Sender) deliver(eventType, url, secret string, timeout time.Duration, payload map[string]any) {
	body, err := json.Marshal(map[string]any{
		"event":     eventType,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"payload":   payload,
	})
	if err != nil {
		log.Printf("warn: alerting: marshal %s: %v", eventType, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		log.Printf("warn: alerting: build request %s: %v", eventType, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req.Header.Set("X-MCPArc-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := s.httpc.Do(req)
	if err != nil {
		log.Printf("warn: alerting: deliver %s failed (ignored): %v", eventType, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("warn: alerting: deliver %s got status %d (ignored)", eventType, resp.StatusCode)
	}
}
