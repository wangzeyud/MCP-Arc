// Package llm implements the optional LLM-assisted masking pass.
//
// It talks to any OpenAI-compatible /chat/completions endpoint and asks the
// model to point at the paths in a tool-call payload that still contain PII or
// secrets after the static rules have run. It is deliberately dependency-free
// and fails open: on error, timeout, or oversized payload the static result
// stands and the tool call proceeds.
package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dodoyu-sama/mcp-arc/internal/mask"
)

// Config is the resolved runtime configuration (see config.LLMConfig).
type Config struct {
	Endpoint      string
	APIKey        string
	Model         string
	Timeout       time.Duration
	MaxBytes      int
	CacheTTL      time.Duration
	CacheMax      int
	ApplyToResult bool
}

type cacheEntry struct {
	findings []mask.Finding
	expiry   time.Time
}

// brokenFor is how long a failing endpoint is skipped before being retried. It
// keeps one bad call from adding latency to every subsequent tool call, without
// permanently disabling the feature after a transient blip.
const brokenFor = 60 * time.Second

// Client is a Detector that consults a chat-completions endpoint.
type Client struct {
	cfg         Config
	http        *http.Client
	mu          sync.Mutex
	cache       map[string]cacheEntry
	fifo        []string
	failedUntil time.Time // endpoint cooling down after a failure
}

// New builds a client. It returns an error only for unusable configuration.
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("llm.endpoint is required when llm.enabled = true")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 3 * time.Second
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 8192
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 5 * time.Minute
	}
	if cfg.CacheMax <= 0 {
		cfg.CacheMax = 512
	}
	if cfg.Model == "" {
		cfg.Model = "gpt-4o-mini"
	}
	return &Client{
		cfg:   cfg,
		http:  &http.Client{Timeout: cfg.Timeout + time.Second},
		cache: make(map[string]cacheEntry, 64),
	}, nil
}

const systemPrompt = `You are a privacy filter for Model Context Protocol tool calls.
You receive a JSON object that has already been through regex-based redaction.
Find any value that still contains personally identifiable information or a secret:
real names, phone numbers, postal addresses, ID numbers, bank accounts, credentials,
private keys, or free text revealing someone's identity.

Reply with JSON only, no prose:
{"findings":[{"path":"<dotted JSON path>","type":"<person|phone|address|id|secret|other>"}]}

Rules:
- "path" is relative to the root of the given object, e.g. "user.email" or "items[1].note".
- Only report paths that exist in the object.
- Report the containing value, not substrings.
- If nothing is sensitive, reply {"findings":[]}.`

// Detect implements mask.Detector.
func (c *Client) Detect(value map[string]any) ([]mask.Finding, error) {
	if c == nil || len(value) == 0 {
		return nil, nil
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(payload) > c.cfg.MaxBytes {
		return nil, nil // too big to be worth asking; static rules already applied
	}
	key := hash(payload)

	if v, ok := c.get(key); ok {
		return v, nil
	}
	if c.isBroken() {
		return nil, nil
	}

	findings, err := c.call(payload)
	if err != nil {
		c.markBroken()
		log.Printf("warn: llm detect failed, falling back to static rules: %v", err)
		return nil, err
	}
	c.put(key, findings)
	return findings, nil
}

func (c *Client) call(payload []byte) ([]mask.Finding, error) {
	body, err := json.Marshal(map[string]any{
		"model":       c.cfg.Model,
		"temperature": 0,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": string(payload)},
		},
	})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatURL(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("llm endpoint returned %s: %s", resp.Status, truncate(string(raw), 200))
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode llm response: %w", err)
	}
	if len(out.Choices) == 0 {
		return nil, errors.New("llm response contained no choices")
	}
	return parseFindings(out.Choices[0].Message.Content)
}

// parseFindings extracts the first JSON object from a model reply. Models often
// wrap JSON in markdown fences despite instructions, so we scan rather than
// unmarshalling the whole reply.
func parseFindings(content string) ([]mask.Finding, error) {
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start < 0 || end <= start {
		return nil, nil // no JSON at all: treat as "no findings"
	}
	var parsed struct {
		Findings []mask.Finding `json:"findings"`
	}
	if err := json.Unmarshal([]byte(content[start:end+1]), &parsed); err != nil {
		return nil, fmt.Errorf("decode findings: %w", err)
	}
	out := make([]mask.Finding, 0, len(parsed.Findings))
	for _, f := range parsed.Findings {
		f.Path = normalizePath(f.Path)
		if f.Path == "" {
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

// normalizePath strips the decorations models like to add ("$.", "arguments.").
func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "$")
	p = strings.TrimPrefix(p, ".")
	for _, prefix := range []string{"arguments.", "params.", "parameters."} {
		p = strings.TrimPrefix(p, prefix)
	}
	return strings.Trim(p, "\"'`")
}

// chatURL accepts either a full /chat/completions URL or a bare base URL.
func (c *Client) chatURL() string {
	if strings.HasSuffix(c.cfg.Endpoint, "/chat/completions") {
		return c.cfg.Endpoint
	}
	return strings.TrimSuffix(c.cfg.Endpoint, "/") + "/chat/completions"
}

func (c *Client) get(key string) ([]mask.Finding, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cache[key]
	if !ok || time.Now().After(e.expiry) {
		return nil, false
	}
	return e.findings, true
}

func (c *Client) put(key string, findings []mask.Finding) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.cache[key]; !exists {
		c.fifo = append(c.fifo, key)
	}
	c.cache[key] = cacheEntry{findings: findings, expiry: time.Now().Add(c.cfg.CacheTTL)}
	for len(c.fifo) > c.cfg.CacheMax {
		delete(c.cache, c.fifo[0])
		c.fifo = c.fifo[1:]
	}
}

// isBroken / markBroken implement a simple circuit breaker so a misconfigured
// endpoint does not add latency to every single tool call.
func (c *Client) isBroken() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Before(c.failedUntil)
}

func (c *Client) markBroken() {
	c.mu.Lock()
	c.failedUntil = time.Now().Add(brokenFor)
	c.mu.Unlock()
	log.Printf("warn: llm masking paused for %s after a failed call", brokenFor)
}

func hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
