package ratelimit

import (
	"sync"
	"sync/atomic"
	"time"
)

// TokenBucketManager enforces per-client rate limits using a token bucket
// (for QPS) plus a daily quota counter.
type TokenBucketManager struct {
	mu         sync.Mutex
	enabled    atomic.Bool
	qps        float64
	dailyQuota int
	buckets    map[string]*tokenBucket
	counts     map[string]*dailyCount
}

type tokenBucket struct {
	capacity float64
	tokens   float64
	rate     float64
	last     time.Time
}

type dailyCount struct {
	date  string
	count int
}

// NewTokenBucketManager creates a manager. When enabled is false, Allow always
// returns true (no limiting).
func NewTokenBucketManager(qps float64, dailyQuota int, enabled bool) *TokenBucketManager {
	m := &TokenBucketManager{
		qps:        qps,
		dailyQuota: dailyQuota,
		buckets:    map[string]*tokenBucket{},
		counts:     map[string]*dailyCount{},
	}
	m.enabled.Store(enabled)
	return m
}

// Reload swaps the active limits atomically (config hot-reload, v0.7). In-flight
// Allow calls may observe a mix of old and new values for one tick, which is
// harmless for rate limiting. Per-client state is dropped so the new limits take
// effect immediately instead of being diluted by stale buckets / quota counters.
func (m *TokenBucketManager) Reload(qps float64, dailyQuota int, enabled bool) {
	m.mu.Lock()
	m.enabled.Store(enabled)
	m.qps = qps
	m.dailyQuota = dailyQuota
	m.buckets = map[string]*tokenBucket{}
	m.counts = map[string]*dailyCount{}
	m.mu.Unlock()
}

// Allow reports whether the given client may proceed right now.
func (m *TokenBucketManager) Allow(clientID string) bool {
	if !m.enabled.Load() {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	today := time.Now().Format("2006-01-02")
	dc := m.counts[clientID]
	if dc == nil || dc.date != today {
		dc = &dailyCount{date: today}
		m.counts[clientID] = dc
	}
	if m.dailyQuota > 0 && dc.count >= m.dailyQuota {
		return false
	}

	tb := m.buckets[clientID]
	if tb == nil {
		capacity := m.qps * 10 // allow bursts up to 10x QPS
		if capacity < 1 {
			capacity = 1
		}
		tb = &tokenBucket{
			capacity: capacity,
			tokens:   capacity,
			rate:     m.qps,
			last:     time.Now(),
		}
		m.buckets[clientID] = tb
	}

	now := time.Now()
	elapsed := now.Sub(tb.last).Seconds()
	tb.tokens += elapsed * tb.rate
	if tb.tokens > tb.capacity {
		tb.tokens = tb.capacity
	}
	tb.last = now

	if tb.tokens >= 1 {
		tb.tokens--
		dc.count++
		return true
	}
	return false
}
