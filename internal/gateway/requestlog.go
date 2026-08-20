package gateway

// RequestLog — a bounded ring of request records for the panel's live feed.
// Metrics by default; prompt/response CONTENT is off unless explicitly
// enabled (same policy as the HUD request log). Bounded so it never grows
// without limit. Safe for concurrent use.

import "sync"

type Record map[string]any

type RequestLog struct {
	mu          sync.Mutex
	buf         []Record
	capacity    int
	keepContent bool
	nextID      int
}

func NewRequestLog(capacity int, keepContent bool) *RequestLog {
	if capacity <= 0 {
		capacity = 500
	}
	return &RequestLog{capacity: capacity, keepContent: keepContent, nextID: 1}
}

// RecordEntry appends a record. content (prompt/response) is stored only when
// the log was built with keepContent; otherwise dropped.
func (l *RequestLog) RecordEntry(fields Record, content Record) Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec := Record{"id": l.nextID}
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
