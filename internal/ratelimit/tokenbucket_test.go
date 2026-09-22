package ratelimit

import (
	"testing"
	"time"
)

// A disabled manager is a no-op: every call is allowed regardless of volume or
// the configured quota. The proxy relies on this so that `rate_limit.enabled: false`
// removes all throttling without special-casing the caller.
func TestAllowDisabledIsNoOp(t *testing.T) {
	m := NewTokenBucketManager(10, 1000, false)
	for i := 0; i < 200; i++ {
		if !m.Allow("c1") {
			t.Fatal("disabled manager must always allow")
		}
	}
	// Quota is also ignored when disabled.
	m2 := NewTokenBucketManager(0, 1, false)
	if !m2.Allow("c1") {
		t.Fatal("disabled manager must ignore the daily quota")
	}
}

// Daily quota bounds the number of allowed calls per client per day. Once the
// quota is spent, further calls are denied, while an untouched client still has
// its own budget.
func TestAllowDailyQuota(t *testing.T) {
	m := NewTokenBucketManager(1e6, 3, true) // huge QPS so only the quota limits
	for i := 0; i < 3; i++ {
		if !m.Allow("c1") {
			t.Fatalf("call %d within quota should be allowed", i)
		}
	}
	if m.Allow("c1") {
		t.Fatal("call beyond the daily quota must be denied")
	}
	if !m.Allow("c2") {
		t.Fatal("a fresh client must still have its own quota")
	}
}

// QPS sets a burst capacity of 10x; without time passing, the bucket empties
// after exactly that many instant calls. This is deterministic (no timers), so
// it pins the capacity formula (qps*10, min 1).
func TestAllowQPSBurstCapacity(t *testing.T) {
	m := NewTokenBucketManager(1, 1e9, true) // capacity = 1*10 = 10
	for i := 0; i < 10; i++ {
		if !m.Allow("c1") {
			t.Fatalf("burst call %d of 10 should be allowed", i)
		}
	}
	if m.Allow("c1") {
		t.Fatal("11th instant call must be denied (tokens exhausted)")
	}
}

// Tokens refill over time: after draining, a short wait lets another call through.
func TestAllowRefillsOverTime(t *testing.T) {
	m := NewTokenBucketManager(1000, 1e9, true)
	if !m.Allow("c1") {
		t.Fatal("first call allowed")
	}
	for m.Allow("c1") { // drain the bucket
	}
	if m.Allow("c1") {
		t.Fatal("bucket should be empty immediately after draining")
	}
	time.Sleep(20 * time.Millisecond) // 1000/s => ~20 tokens back
	if !m.Allow("c1") {
		t.Fatal("bucket should have refilled after the wait")
	}
}
