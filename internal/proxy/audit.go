package proxy

import (
	"context"
	"log"
	"time"

	"github.com/dodoyu-sama/mcp-arc/internal/audit"
)

// enqueueAudit persists a call record without stalling the response path.
//
// In production Run() starts the async worker and sizes auditCh, so the write is
// handed to a buffered channel and a background goroutine; if the buffer is full
// the record is dropped because audit is best-effort (discipline #6: non-core
// work must never block request forwarding). When no worker is configured (audit
// disabled, store init failed, or unit tests) it falls back to a direct insert so
// behaviour stays fail-open either way.
func (p *Proxy) enqueueAudit(rec *audit.CallRecord) {
	if p.auditStore == nil {
		return
	}
	if p.auditCh != nil {
		select {
		case p.auditCh <- rec:
		default:
			log.Printf("warn: audit queue full, dropping call record (audit is best-effort)")
		}
		return
	}
	if err := p.auditStore.Insert(rec); err != nil {
		log.Printf("warn: audit insert failed: %v", err)
	}
}

// auditWorker drains auditCh and writes each record with a wall-clock timeout, so
// a slow or hung database can never back up the proxy's response path (which only
// enqueues). On shutdown (ctx.Done) it drains any buffered records before
// returning, so a graceful stop does not silently drop in-flight audit writes;
// done is closed once draining has finished so Run() can wait on it before
// closing the store.
func (p *Proxy) auditWorker(ctx context.Context, done chan struct{}) {
	timeout := p.auditWriteTimeout()
	defer close(done)
	for {
		select {
		case rec := <-p.auditCh:
			p.insertAuditRecord(rec, timeout)
		case <-ctx.Done():
			p.drainAudit(timeout)
			return
		}
	}
}

// drainAudit writes any records still buffered in auditCh before the process
// exits. It uses a fresh, bounded deadline so a slow or hung database cannot
// block shutdown indefinitely — discipline #6: audit is best-effort and must
// never stall request forwarding or shutdown.
func (p *Proxy) drainAudit(timeout time.Duration) {
	if p.auditCh == nil {
		return
	}
	deadline := time.After(p.auditShutdownTimeout())
	for {
		select {
		case rec := <-p.auditCh:
			p.insertAuditRecord(rec, timeout)
		case <-deadline:
			return
		default:
			return
		}
	}
}

// insertAuditRecord runs the (synchronous, context-free) store Insert in a
// goroutine and waits for it or the timeout, whichever comes first. On timeout the
// record is considered dropped — the Insert goroutine may still finish later, but
// the worker has already moved on, so a slow write can never block a fast one.
func (p *Proxy) insertAuditRecord(rec *audit.CallRecord, timeout time.Duration) {
	done := make(chan error, 1)
	go func() { done <- p.auditStore.Insert(rec) }()
	select {
	case err := <-done:
		if err != nil {
			log.Printf("warn: audit insert failed: %v", err)
		}
	case <-time.After(timeout):
		log.Printf("warn: audit insert timed out after %s, dropping call record", timeout)
	}
}

func (p *Proxy) auditQueueSize() int {
	if p.opts.Config != nil && p.opts.Config.Audit.QueueSize > 0 {
		return p.opts.Config.Audit.QueueSize
	}
	return 1024
}

func (p *Proxy) auditWriteTimeout() time.Duration {
	if p.opts.Config != nil && p.opts.Config.Audit.WriteTimeoutMs > 0 {
		return time.Duration(p.opts.Config.Audit.WriteTimeoutMs) * time.Millisecond
	}
	return 2 * time.Second
}

func (p *Proxy) auditShutdownTimeout() time.Duration {
	if p.opts.Config != nil && p.opts.Config.Audit.ShutdownFlushTimeoutMs > 0 {
		return time.Duration(p.opts.Config.Audit.ShutdownFlushTimeoutMs) * time.Millisecond
	}
	return 5 * time.Second
}
