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
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"

	"github.com/Custok/sofmat/internal/config"
	"github.com/Custok/sofmat/internal/coordinator"
)

// genAPIKey mints a random bearer key (Jupyter-token style).
func genAPIKey() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return "sk-soflink-" + hex.EncodeToString(b)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sofmat <serve|version>")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "version":
		fmt.Println("soflink", version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "config.local.json", "path to cluster config")
	noBrowser := fs.Bool("no-browser", false, "do not open the panel in a browser (headless nodes)")
	noAuth := fs.Bool("no-auth", false, "leave mutating routes (load/eject) open — skips fail-closed key")
	noUpdate := fs.Bool("no-update", false, "skip the GitHub auto-update check on startup")
	_ = fs.Parse(args)
	coordinator.Version = version         // so the panel header shows the running build number
	coordinator.SetAutoUpdate(!*noUpdate) // the header checkbox reads/toggles this
	coordinator.UpdateNow = checkAndUpdate // the header "actualizar ahora" button triggers this
	if !*noUpdate {
		checkAndUpdate()    // self-update from GitHub Releases at startup, then re-exec (best-effort)
		go periodicUpdate() // y sigue comprobando en runtime (cada 30m) para coger releases sin reiniciar
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Fail-closed: if no API key is configured, mint one at startup so load/eject
	// are never open on the LAN. The key shows (and copies) in the panel.
	if cfg.APIKey == "" && !*noAuth {
		cfg.APIKey = genAPIKey()
		fmt.Printf("API key generada (requerida para load/eject; visible/copiable en el panel):\n  %s\n", cfg.APIKey)
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
