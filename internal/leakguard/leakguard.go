// Package leakguard blocks private infra / secrets before a public push —
// the engine that keeps this repository publishable. Go port of the validated
// Python scanner (leak-guard/scan.py); its self-test fixtures are the
// behavioral contract.
//
// Design (important — the scanner itself must not leak):
//
//   - PUBLIC patterns are STRUCTURAL, not enumerations: private IPv4 ranges,
//     token shapes, absolute home paths, dynamic-DNS domains — shapes, never
//     one of our real values.
//   - SPECIFIC real names live in an OPTIONAL local denylist file
//     (git-ignored) and/or the SOFMAT_LEAKGUARD_DENYLIST env var (a CI
//     secret), loaded at run time. The published scanner reveals nothing.
//
// Anonymous cluster labels node-a/b/c/d and *.example.local placeholders are
// always allowed — that is the anonymisation convention.
//
// RE2 note: Go regexp has no lookaround, so the two rules that used it in
// Python (ip-octet-port's lookbehind, unsafe-deser's lookaheads) are
// implemented as match functions with an explicit secondary check. Same
// accept/reject behavior, verified by the ported fixtures.
package leakguard

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Rule is one leak pattern: Match reports whether a line trips it.
type Rule struct {
	Name  string
	Hint  string
	Match func(line string) bool
}

// Finding is one blocked spot.
type Finding struct {
	Path    string
	Line    int
	Rule    string
	Hint    string
	Excerpt string
}

// Extensions never scanned as text (binary / weights).
var skipExt = map[string]bool{
	".gguf": true, ".safetensors": true, ".bin": true, ".pt": true,
	".pth": true, ".onnx": true, ".png": true, ".jpg": true, ".jpeg": true,
	".webp": true, ".gif": true, ".ico": true, ".wav": true, ".mp3": true,
	".m4a": true, ".flac": true, ".ogg": true, ".mp4": true, ".mov": true,
	".zip": true, ".gz": true, ".tar": true, ".7z": true, ".pdf": true,
	".woff": true, ".woff2": true,
}

// Paths never CONTENT-scanned: the scanner's own engine (it necessarily
// embeds pattern fragments), its tests (deliberate fakes) and the git-ignored
// denylist — Python and Go incarnations alike.
var skipPathRe = regexp.MustCompile(
	`(^|/)(\.git/|node_modules/|\.venv/|venv/|__pycache__/` +
		`|leak-guard/scan\.py$|leak-guard/test_scan\.py$` +
		`|leak-guard/denylist\.local\.txt$` +
		`|internal/leakguard/[^/]+\.go$|cmd/leak-guard/main\.go$)`)

// Files that must NEVER be committed, whatever their content (defence in
// depth over .gitignore): blocked by NAME.
var forbiddenBasename = regexp.MustCompile(
	`(^|/)(denylist\.local\.txt` +
		`|config\.local\.(ya?ml|json|toml)` +
		`|nodes\.local\.[^/]+` +
		`|\.env(\.[^/]+)?)$`)

// Lines carrying an allow marker are waved through (example configs / docs).
var allowRe = regexp.MustCompile(
	`(?i)node-[a-z]\b` +
		`|\.example\.(local|com|org)\b` +
		`|REPLACE|PLACEHOLDER|example\.yaml` +
		`|leak-guard-allow`)

func regexRule(name, pattern, hint string) Rule {
	re := regexp.MustCompile(pattern)
	return Rule{Name: name, Hint: hint, Match: re.MatchString}
}

// ipOctetPort ports Python's `(?<!\d)\.\d{1,3}:\d{2,5}\b` (no lookbehind in
// RE2): find the shape, then reject matches preceded by a digit so plain
// decimals like "0.51" don't fire.
var ipOctetPortRe = regexp.MustCompile(`\.\d{1,3}:\d{2,5}\b`)

func ipOctetPort(line string) bool {
	for _, loc := range ipOctetPortRe.FindAllStringIndex(line, -1) {
		if loc[0] == 0 || line[loc[0]-1] < '0' || line[loc[0]-1] > '9' {
			return true
		}
	}
	return false
}

// unsafeDeser ports the A08 rule. Plain forms are a regex; torch.load /
// yaml.load need the Python lookaheads' logic: inspect the call's argument
// text (up to the closing paren on the line, as `[^)]*` did).
var unsafeDeserRe = regexp.MustCompile(
	`\b(?:pickle\.loads?|cPickle|_pickle|marshal\.loads?)`)
var torchLoadRe = regexp.MustCompile(`\btorch\.load\s*\(`)
var yamlLoadRe = regexp.MustCompile(`\byaml\.load\s*\(`)
var weightsOnlyRe = regexp.MustCompile(`weights_only\s*=\s*True`)

func callArgs(line string, loc []int) string {
	rest := line[loc[1]:]
	if i := strings.IndexByte(rest, ')'); i >= 0 {
		return rest[:i]
	}
	return rest
}

func unsafeDeser(line string) bool {
	if unsafeDeserRe.MatchString(line) {
		return true
	}
	if loc := torchLoadRe.FindStringIndex(line); loc != nil {
		if !weightsOnlyRe.MatchString(callArgs(line, loc)) {
			return true
		}
	}
	if loc := yamlLoadRe.FindStringIndex(line); loc != nil {
		if !strings.Contains(callArgs(line, loc), "Safe") {
			return true
		}
	}
	return false
}

// StructuralRules are the public, shape-only rules.
func StructuralRules() []Rule {
	return []Rule{
		regexRule("private-ipv4",
			`\b(?:10(?:\.\d{1,3}){3}`+
				`|192\.168(?:\.\d{1,3}){2}`+
				`|172\.(?:1[6-9]|2\d|3[01])(?:\.\d{1,3}){2})\b`,
			"private IPv4 address — use node-x logical labels, real IPs go in config.local.yaml"),
		{Name: "ip-octet-port", Match: ipOctetPort,
			Hint: "host as .octet:port — use a node-x label; real endpoints in config.local.yaml"},
		regexRule("service-token",
			`\b(?:lst_|ghp_|gho_|ghs_|xox[baprs]-)[A-Za-z0-9_\-]{8,}`,
			"looks like a service/API token"),
		regexRule("bearer-token",
			`(?i)\b(?:bearer|token)\s+[A-Za-z0-9._\-]{16,}`,
			"hardcoded bearer/token — load from env / config.local.yaml"),
		regexRule("credential-assign",
			`(?i)\b(?:authorization|api[_-]?key|secret|password|passwd)\b\s*[:=]\s*['"]?[A-Za-z0-9._\-]{12,}`,
			"hardcoded credential — load from env / config.local.yaml"),
		regexRule("abs-home-path",
			`(?:/home/[A-Za-z0-9._-]+|/Users/[A-Za-z0-9._-]+|[Cc]:\\Users\\[A-Za-z0-9._-]+|~/\.lmstudio)\b`,
			"absolute local path — describes our lab; use a config-driven path"),
		regexRule("dyndns-host",
			`\b[A-Za-z0-9_-]+\.(?:mynetgear\.com|dyndns\.\w+|no-ip\.\w+)\b`,
			"dynamic-DNS hostname of our network"),
		{Name: "unsafe-deser", Match: unsafeDeser,
			Hint: "unsafe deserialization — use a binary framing / safe_load / weights_only=True; never on network input"},
	}
}

// LocalRules builds the literal denylist of SPECIFIC private terms from a
// git-ignored file and/or the SOFMAT_LEAKGUARD_DENYLIST env var (newline
// separated — how CI injects the list as a repository secret without ever
// committing it). Whole-word, case-insensitive: "Ada" must not fire inside
// "metadata".
func LocalRules(denylistPath string) []Rule {
	var terms []string
	if denylistPath != "" {
		if b, err := os.ReadFile(denylistPath); err == nil {
			terms = append(terms, strings.Split(string(b), "\n")...)
		}
	}
	terms = append(terms, strings.Split(os.Getenv("SOFMAT_LEAKGUARD_DENYLIST"), "\n")...)

	var rules []Rule
	for _, raw := range terms {
		term := strings.TrimSpace(raw)
		if term == "" || strings.HasPrefix(term, "#") {
			continue
		}
		rules = append(rules, regexRule("private-term",
			`(?i)\b`+regexp.QuoteMeta(term)+`\b`,
			"matches a private term (denylist.local.txt / SOFMAT_LEAKGUARD_DENYLIST)"))
	}
	return rules
}

func readText(path string) (string, bool) {
	if skipExt[strings.ToLower(filepath.Ext(path))] {
		return "", false
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	head := blob
	if len(head) > 4096 {
		head = head[:4096]
	}
	if bytes.IndexByte(head, 0) >= 0 { // binary sniff
		return "", false
	}
	return string(blob), true
}

// ScanPaths runs the rules over the given files. Forbidden basenames are
// flagged by NAME before any read; skip-listed paths are content-exempt; one
// finding per line is enough to block.
func ScanPaths(paths []string, rules []Rule) []Finding {
	var findings []Finding
	for _, path := range paths {
		norm := strings.ReplaceAll(path, `\`, "/")
		if forbiddenBasename.MatchString(norm) {
			findings = append(findings, Finding{
				Path: path, Line: 0, Rule: "forbidden-file",
				Hint:    "this file must never be committed (local secrets / real infra) — it belongs only on disk, git-ignored",
				Excerpt: filepath.Base(norm),
			})
			continue
		}
		if skipPathRe.MatchString(norm) {
			continue
		}
		text, ok := readText(path)
		if !ok {
			continue
		}
		for i, line := range strings.Split(text, "\n") {
			line = strings.TrimSuffix(line, "\r")
			if allowRe.MatchString(line) {
				continue
			}
			for _, rule := range rules {
				if rule.Match(line) {
					excerpt := strings.TrimSpace(line)
					if len(excerpt) > 120 {
						excerpt = excerpt[:117] + "..."
					}
					findings = append(findings, Finding{
						Path: path, Line: i + 1, Rule: rule.Name,
						Hint: rule.Hint, Excerpt: excerpt,
					})
					break // one finding per line is enough
				}
			}
		}
	}
	return findings
}

// StagedFiles lists git-staged files (pre-commit mode).
func StagedFiles() ([]string, error) {
	out, err := exec.Command("git", "diff", "--cached", "--name-only",
		"--diff-filter=ACM").Output()
	if err != nil {
		return nil, fmt.Errorf("leak-guard: git diff --cached: %w", err)
	}
	return splitLines(string(out)), nil
}

// TreeFiles lists the whole tree (CI mode): git ls-files when available,
// filesystem walk as fallback.
func TreeFiles(root string) []string {
	if out, err := exec.Command("git", "-C", root, "ls-files").Output(); err == nil {
		var paths []string
		for _, p := range splitLines(string(out)) {
			paths = append(paths, filepath.Join(root, p))
		}
		return paths
	}
	var paths []string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			paths = append(paths, p)
		}
		return nil
	})
	return paths
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}
