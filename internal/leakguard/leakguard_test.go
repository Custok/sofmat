package leakguard

// Self-test — the Python suite's fixtures (test_scan.py) ported 1:1: every
// MUST-BLOCK snippet must produce >= 1 finding, every MUST-PASS must stay
// clean, plus the by-NAME block/pass path sets and the RE2-workaround edges.

import (
	"os"
	"path/filepath"
	"testing"
)

func scanSnippet(t *testing.T, text string) int {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "snippet.txt")
	if err := os.WriteFile(path, []byte(text+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return len(ScanPaths([]string{path}, StructuralRules()))
}

func TestMustBlockContent(t *testing.T) {
	cases := map[string]string{
		"private-ip":        "coordinator = 10.0.0.30",
		"private-ip-192":    "gateway: 192.168.1.1",
		"service-token":     `TOKEN = "lst_nMPp3xQ72zaBcd1234"`,
		"bearer":            "Authorization: Bearer abcDEF1234567890xyz",
		"api-key-assign":    `api_key = "sk-9f8e7d6c5b4a3210"`,
		"home-path":         "weights at /home/someuser/models/big.gguf",
		"lmstudio-path":     "load ~/.lmstudio/models/foo",
		"dyndns":            "reach it at mybox.mynetgear.com",
		"pickle-loads":      "act = pickle.loads(frame)",
		"torch-load-unsafe": "state = torch.load(path)",
		"yaml-load-unsafe":  "cfg = yaml.load(open(p))",
		"ip-octet-port":     "rpc at .51:50052 up",
	}
	for label, snippet := range cases {
		if scanSnippet(t, snippet) == 0 {
			t.Errorf("MUST-BLOCK %q was NOT caught: %q", label, snippet)
		}
	}
}

func TestMustPassContent(t *testing.T) {
	cases := map[string]string{
		"anon-label":       "master: node-a  # logical label",
		"example-host":     "host: node-b.example.local",
		"placeholder-path": "path: /REPLACE/WITH/LOCAL/MODEL/PATH",
		"public-ip-doc":    "example public DNS 8.8.8.8 in a comment",
		"port-number":      "port: 50051",
		"allow-marker":     "note: 10.0.0.0/24 is our lab range  # leak-guard-allow",
		"plain-prose":      "The runtime measures ms per layer and reports it.",
		"torch-load-safe":  "state = torch.load(path, weights_only=True)",
		"yaml-safe":        "cfg = yaml.safe_load(open(p))",
		"digit-prefixed":   "spec rev 1.51:8080 naming scheme", // lookbehind port edge: digit before the dot must not fire
	}
	for label, snippet := range cases {
		if n := scanSnippet(t, snippet); n != 0 {
			t.Errorf("MUST-PASS %q false-positived (%d): %q", label, n, snippet)
		}
	}
}

func TestMustBlockPathsByName(t *testing.T) {
	paths := []string{
		"leak-guard/denylist.local.txt",
		"config.local.yaml",
		"some/dir/config.local.json",
		".env",
		".env.production",
		"nodes.local.map",
	}
	for _, p := range paths {
		if len(ScanPaths([]string{p}, StructuralRules())) == 0 {
			t.Errorf("MUST-BLOCK-PATH not caught: %s", p)
		}
	}
}

func TestMustPassPathsByName(t *testing.T) {
	paths := []string{
		"leak-guard/denylist.local.example.txt",
		"config.example.yaml",
		"runtime/worker.py",
	}
	for _, p := range paths {
		// nonexistent on disk: clean pass = name ok, content unread.
		if n := len(ScanPaths([]string{p}, StructuralRules())); n != 0 {
			t.Errorf("MUST-PASS-PATH false-positived (%d): %s", n, p)
		}
	}
}

func TestScannerOwnSourceIsContentExempt(t *testing.T) {
	// The Go engine embeds pattern fragments; it must be skip-listed like
	// scan.py was, or the guard blocks its own repository.
	for _, p := range []string{
		"internal/leakguard/leakguard.go",
		"internal/leakguard/leakguard_test.go",
		"cmd/leak-guard/main.go",
	} {
		if !skipPathRe.MatchString(p) {
			t.Errorf("scanner source not content-exempt: %s", p)
		}
	}
}

func TestLocalRulesWordBoundaryAndSources(t *testing.T) {
	dir := t.TempDir()
	deny := filepath.Join(dir, "denylist.local.txt")
	if err := os.WriteFile(deny, []byte("# comment\nAda\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rules := LocalRules(deny)
	if len(rules) != 1 {
		t.Fatalf("want 1 rule from file, got %d", len(rules))
	}
	if !rules[0].Match("the Ada avatar") || !rules[0].Match("ada speaks") {
		t.Error("term must match whole-word, case-insensitive")
	}
	if rules[0].Match("validated metadata") {
		t.Error("term must NOT match inside another word")
	}
	t.Setenv("SOFMAT_LEAKGUARD_DENYLIST", "secretname\n# no\n")
	if got := len(LocalRules("")); got != 1 {
		t.Errorf("env-injected denylist: want 1 rule, got %d", got)
	}
}

func TestOneFindingPerLineAndExcerptCap(t *testing.T) {
	// A line tripping two rules yields ONE finding; long excerpts are capped.
	long := "x 10.0.0.30 " + string(make([]byte, 0))
	for i := 0; i < 150; i++ {
		long += "y"
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	os.WriteFile(path, []byte("token = lst_abcdefghij123 at /home/u/x\n"+long+"\n"), 0o644)
	fs := ScanPaths([]string{path}, StructuralRules())
	if len(fs) != 2 {
		t.Fatalf("want 2 findings (one per line), got %d: %+v", len(fs), fs)
	}
	if len(fs[1].Excerpt) > 120 {
		t.Errorf("excerpt not capped: %d chars", len(fs[1].Excerpt))
	}
}

func TestBinaryAndSkippedExtensionsUnread(t *testing.T) {
	dir := t.TempDir()
	gguf := filepath.Join(dir, "w.gguf")
	os.WriteFile(gguf, []byte("10.0.0.30"), 0o644) // leak inside skipped ext
	bin := filepath.Join(dir, "blob.dat")
	os.WriteFile(bin, append([]byte{0}, []byte("10.0.0.30")...), 0o644) // NUL sniff
	if n := len(ScanPaths([]string{gguf, bin}, StructuralRules())); n != 0 {
		t.Errorf("binary/skipped files must not be content-scanned: %d", n)
	}
}
