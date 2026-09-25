package gateway

// RequestLog — a bounded ring of request records for the panel's live feed.
// Metrics by default; prompt/response CONTENT is off unless explicitly
// enabled (same policy as the HUD request log). Bounded so it never grows
// without limit. Safe for concurrent use.

import (
	"fmt"
	"sync"
	"time"
)

type Record map[string]any

type RequestLog struct {
	mu          sync.Mutex
	buf         []Record
	capacity    int
	keepContent bool
	nextID      int
	// boot tags this process's lifetime. `id` restarts at 1 with every process,
	// so on its own it names a different request after each restart (debian,
	// 25-09-2026: 67% of the ids in 24 h of sealed rows designated more than one
	// request across 4 restarts). `rid` = boot + "-" + id is the durable name.
	boot string
}

// bootTag names a process lifetime: UTC start time to the millisecond.
func bootTag(t time.Time) string {
	return t.UTC().Format("20060102T150405.000Z")
}

func NewRequestLog(capacity int, keepContent bool) *RequestLog {
	if capacity <= 0 {
		capacity = 500
	}
	return &RequestLog{capacity: capacity, keepContent: keepContent, nextID: 1, boot: bootTag(time.Now())}
}

// Boot is this log's lifetime tag (the prefix of every rid it issues).
func (l *RequestLog) Boot() string { return l.boot }

// RecordEntry appends a record. content (prompt/response) is stored only when
// the log was built with keepContent; otherwise dropped. Every record carries
// `id` (per-process counter, kept for the panel), `boot` and `rid` (durable).
func (l *RequestLog) RecordEntry(fields Record, content Record) Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec := Record{"id": l.nextID, "boot": l.boot, "rid": fmt.Sprintf("%s-%d", l.boot, l.nextID)}
	l.nextID++
	for k, v := range fields {
		rec[k] = v
	}
	if l.keepContent && content != nil {
		rec["content"] = content
	}
	l.buf = append(l.buf, rec)
	if len(l.buf) > l.capacity {
		l.buf = l.buf[len(l.buf)-l.capacity:]
	}
	return rec
}

// Tail returns the newest n records, oldest first.
func (l *RequestLog) Tail(n int) []Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n > len(l.buf) {
		n = len(l.buf)
	}
	out := make([]Record, n)
	copy(out, l.buf[len(l.buf)-n:])
	return out
}

func (l *RequestLog) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buf)
}
