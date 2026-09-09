package coordinator

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestOpenFDsCounts applies the fleet's rule of 2026-09-09 to the new
// instrument: before believing a zero, prove the instrument can say "one".
// A descriptor counter that always answered the same number would report a
// leak-free daemon while it leaked, which is exactly the failure this
// endpoint exists to catch.
func TestOpenFDsCounts(t *testing.T) {
	if runtime.GOOS != "linux" {
		if openFDs() != -1 {
			t.Fatalf("off Linux openFDs must admit it cannot count, got %d", openFDs())
		}
		t.Skip("descriptor counting is Linux-only")
	}
	before := openFDs()
	if before <= 0 {
		t.Fatalf("openFDs must count this process's descriptors, got %d", before)
	}
	const n = 10
	dir := t.TempDir()
	var held []*os.File
	for i := 0; i < n; i++ {
		f, err := os.Create(filepath.Join(dir, string(rune('a'+i))))
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		held = append(held, f)
	}
	during := openFDs()
	if during < before+n {
		t.Fatalf("holding %d more files, the count went %d -> %d: it does not see them", n, before, during)
	}
	for _, f := range held {
		f.Close()
	}
	if after := openFDs(); after >= during {
		t.Fatalf("after closing %d files the count did not fall: %d -> %d", n, during, after)
	}
}

// TestGoroutineCountMoves is the same check for the other half of the
// instrument: a goroutine gauge that never moves cannot report a goroutine
// leak either.
func TestGoroutineCountMoves(t *testing.T) {
	before := runtime.NumGoroutine()
	stop := make(chan struct{})
	const n = 25
	for i := 0; i < n; i++ {
		go func() { <-stop }()
	}
	// the launched goroutines are parked on the channel, so they are all live
	if during := runtime.NumGoroutine(); during < before+n {
		close(stop)
		t.Fatalf("%d parked goroutines moved the gauge only %d -> %d", n, before, during)
	}
	close(stop)
}
