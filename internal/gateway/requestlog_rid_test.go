package gateway

import (
	"strings"
	"testing"
	"time"
)

// `id` restarts at 1 with every process; `rid` (boot + id) must not: two logs
// standing for two process lifetimes issue distinct rids for the same id.
func TestRequestRIDIsDurableAcrossRestarts(t *testing.T) {
	a := NewRequestLog(10, false)
	time.Sleep(2 * time.Millisecond) // a restart is never within the same millisecond
	b := NewRequestLog(10, false)
	ra := a.RecordEntry(Record{"x": 1}, nil)
	rb := b.RecordEntry(Record{"x": 2}, nil)
	if ra["id"] != 1 || rb["id"] != 1 {
		t.Fatalf("per-process ids should both restart at 1: %v %v", ra["id"], rb["id"])
	}
	if ra["rid"] == rb["rid"] {
		t.Fatalf("rid collided across two lifetimes: %v", ra["rid"])
	}
	if ra["boot"] != a.Boot() || !strings.HasPrefix(ra["rid"].(string), a.Boot()+"-") {
		t.Fatalf("rid must be boot-id: boot=%v rid=%v", ra["boot"], ra["rid"])
	}
	r2 := a.RecordEntry(Record{}, nil)
	if r2["rid"] != a.Boot()+"-2" {
		t.Fatalf("second rid of a lifetime = %v, want %s-2", r2["rid"], a.Boot())
	}
}

func TestBootTagIsUTCMillis(t *testing.T) {
	tag := bootTag(time.Date(2026, 9, 25, 19, 42, 27, 123_000_000, time.FixedZone("CEST", 2*3600)))
	if tag != "20260925T174227.123Z" {
		t.Fatalf("bootTag = %q, want UTC with milliseconds", tag)
	}
}
