// Command sofmat runs the distributed inference stack: it pools GPUs across
// hosts, splits one model over a pipeline, and serves an OpenAI-compatible API.
//
// The production stack is a single Go binary spanning three planes:
//
//   - data-plane    — transport / KV handoff        (internal/transport)
//   - control-plane — gateway, admission, solver     (internal/gateway, internal/partitioner)
//   - engine        — cgo binding to libllama         (internal/engine)
//
// orchestrated by internal/coordinator. See docs/design/go-stack.md.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Custok/sofmat/internal/config"
	"github.com/Custok/sofmat/internal/coordinator"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sofmat <serve|version>")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "version":
		fmt.Println("sofmat dev")
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "config.local.json", "path to cluster config")
	noBrowser := fs.Bool("no-browser", false, "do not open the panel in a browser (headless nodes)")
	_ = fs.Parse(args)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("sofmat serve on %s\n", cfg.Listen)
	ensureFirewall(cfg.Listen) // open the port so the LAN can reach the panel/API
	if !*noBrowser {
		go openPanel(cfg.Listen) // pop the dashboard like a desktop app
	}
	if err := coordinator.Run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
