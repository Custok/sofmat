package coordinator

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
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
	// The counter is process-global and the rest of the package keeps test
	// servers alive, so descriptors open and close underneath this measurement:
	// asserting a strict drop made the test pass alone and fail in the suite —
	// caught on 2026-09-09, and a test that fails sometimes is worse than no
	// test, because it teaches people to ignore a red run. Half the files is
	// still far more movement than the noise, so a counter stuck at a constant
	// fails while ordinary churn does not.
	want := during - n/2
	var after int
	deadline := time.Now().Add(2 * time.Second)
	for {
		after = openFDs()
		if after <= want || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if after > want {
		t.Fatalf("after closing %d files the count did not fall: %d -> %d (esperaba <= %d)", n, during, after, want)
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

// TestChildCountsSeesAZombie is the same rule again, applied to the counter the
// two node devs asked for: it must be able to say "one". A zombie counter stuck
// at zero would report a clean daemon while orphans piled up — which is exactly
// how 990 of them went unnoticed on .51 until they were measured from outside
// with ps.
func TestChildCountsSeesAZombie(t *testing.T) {
	if runtime.GOOS != "linux" {
		if c, z := childCounts(); c != -1 || z != -1 {
			t.Fatalf("off Linux childCounts must admit it cannot count, got %d/%d", c, z)
		}
		t.Skip("child accounting is Linux-only")
	}
	_, zBefore := childCounts()

	// a child that exits and is deliberately NOT waited for: the zombie this
	// endpoint exists to make visible
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skipf("no /bin/sh here: %v", err)
	}
	var zAfter int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, z := childCounts(); z > zBefore {
			zAfter = z
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if zAfter <= zBefore {
		_ = cmd.Wait()
		t.Fatalf("an unreaped exited child did not show up: zombies %d -> %d", zBefore, zAfter)
	}

	// and reaping it must make the count fall again — otherwise the counter is
	// only counting up and would never show a leak being FIXED
	_ = cmd.Wait()
	if _, z := childCounts(); z >= zAfter {
		t.Fatalf("after reaping, zombies did not fall: %d -> %d", zAfter, z)
	}
}
