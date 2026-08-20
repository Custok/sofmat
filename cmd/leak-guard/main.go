// Command leak-guard is the sofmat publication gate — Go port of
// leak-guard/scan.py with the same modes, output shape and exit codes, so the
// pre-commit hook and CI keep their contract:
//
//	leak-guard --staged          scan git-staged content (pre-commit)
//	leak-guard --all [--root R]  scan the whole tree (CI)
//	leak-guard FILE...           scan explicit files
//
// Exit 0 = clean; exit 1 = blocked (findings on stderr).
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Custok/sofmat/internal/leakguard"
)

func main() {
	fs := flag.NewFlagSet("leak-guard", flag.ExitOnError)
	staged := fs.Bool("staged", false, "scan git-staged content (pre-commit)")
	all := fs.Bool("all", false, "scan the whole tree (CI)")
	root := fs.String("root", ".", "repo root for --all")
	denylist := fs.String("denylist", filepath.Join("leak-guard", "denylist.local.txt"),
		"path to the git-ignored local denylist (optional)")
	_ = fs.Parse(os.Args[1:])

	if *staged && *all {
		fmt.Fprintln(os.Stderr, "leak-guard: --staged and --all are mutually exclusive")
		os.Exit(2)
	}

	var paths []string
	switch {
	case *staged:
		var err error
		paths, err = leakguard.StagedFiles()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	case len(fs.Args()) > 0:
		paths = fs.Args()
	default:
		paths = leakguard.TreeFiles(*root)
	}

	rules := append(leakguard.StructuralRules(), leakguard.LocalRules(*denylist)...)
	findings := leakguard.ScanPaths(paths, rules)

	if len(findings) == 0 {
		fmt.Printf("leak-guard: clean (%d file(s) scanned, %d rule(s)).\n",
			len(paths), len(rules))
		return
	}

	fmt.Fprintf(os.Stderr, "\n🚫 leak-guard BLOCKED — %d potential leak(s):\n\n", len(findings))
	for _, f := range findings {
		fmt.Fprintf(os.Stderr, "  %s:%d  [%s] %s\n", f.Path, f.Line, f.Rule, f.Hint)
		fmt.Fprintf(os.Stderr, "      > %s\n", f.Excerpt)
	}
	fmt.Fprintln(os.Stderr, "\nFix: move the value to config.local.yaml / env, use a node-x label, "+
		"or (reviewed false positive) append 'leak-guard-allow' to the line.")
	os.Exit(1)
}
