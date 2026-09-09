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
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Custok/sofmat/internal/config"
	"github.com/Custok/sofmat/internal/coordinator"
)

// apiKeyPath is where a minted key is kept: beside the config, so it survives a
// restart without rewriting the operator's config file (which would risk losing
// anything the config struct does not model).
func apiKeyPath(cfgPath string) string {
	dir := filepath.Dir(cfgPath)
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, ".soflink-apikey")
}

func readAPIKey(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeAPIKey(path, key string) error {
	return os.WriteFile(path, []byte(key+"\n"), 0o600)
}

// maskKey shows enough to tell two keys apart and not enough to use one.
func maskKey(k string) string {
	if len(k) < 16 {
		return "sk-soflink-..."
	}
	return k[:14] + "..." + k[len(k)-4:]
}

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
	// Default ON everywhere but the desktop platforms. Measured 2026-09-09 on
	// .51 and .63: every start ran xdg-open, which opened a Firefox tab on the
	// host's desktop and left a zombie — 113 times in one night on .63. A
	// coordinator on a server node must never pop a browser; on Windows and
	// macOS the panel pop is the intended desktop behaviour and stays.
	headless := runtime.GOOS != "windows" && runtime.GOOS != "darwin"
	noBrowser := fs.Bool("no-browser", headless, "do not open the panel in a browser (default on server platforms)")
	noAuth := fs.Bool("no-auth", false, "leave mutating routes (load/eject) open — skips fail-closed key")
	noUpdate := fs.Bool("no-update", false, "skip the GitHub auto-update check on startup")
	_ = fs.Parse(args)
	// Load config FIRST so the updater can read the GitHub token before its very
	// first API call — otherwise the startup check would run anonymous.
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	coordinator.Version = version             // so the panel header shows the running build number
	coordinator.GitHubToken = cfg.GitHubToken // authenticate the updater (5000 req/h vs anonymous 60/h)
	coordinator.SetAutoUpdate(!*noUpdate)     // the header checkbox reads/toggles this
	coordinator.UpdateNow = checkAndUpdate    // the header "actualizar todos" button triggers this
	coordinator.UpdateBlocked = blockedReason // por que NO avanza, visible en el panel
	if !*noUpdate {
		checkAndUpdate()    // self-update from GitHub Releases at startup, then re-exec (best-effort)
		go periodicUpdate() // y sigue comprobando en runtime (cada 30m) para coger releases sin reiniciar
	}
	// Fail-closed: if no API key is configured, mint one so load/eject are never
	// open on the LAN — but PERSIST it, and never print it whole.
	//
	// Both halves were bugs. Minting without persisting meant a new key on every
	// restart, so any client holding one lost it: on a node caught in an update
	// loop that was a new key every half hour, 51 of them in a day. And printing
	// it in full put a live credential in the log, which gets copied around and
	// pasted into chats when something breaks.
	if cfg.APIKey == "" && !*noAuth {
		keyPath := apiKeyPath(*cfgPath)
		if k := readAPIKey(keyPath); k != "" {
			cfg.APIKey = k
			fmt.Printf("API key leida de %s (enmascarada: %s)\n", keyPath, maskKey(cfg.APIKey))
		} else {
			cfg.APIKey = genAPIKey()
			if err := writeAPIKey(keyPath, cfg.APIKey); err != nil {
				fmt.Printf("API key generada pero NO persistida (%v): rotara en el proximo arranque\n", err)
			} else {
				fmt.Printf("API key generada y guardada en %s (enmascarada: %s)\n", keyPath, maskKey(cfg.APIKey))
			}
		}
		// Say it when the file is not actually private. Checked by STATTING
		// the file back, not by trusting the write that was supposed to make
		// it so: os.WriteFile(0600) on Windows applies no ACL and returns no
		// error, so the message "guardada" was itself the deception.
		if warn := keyPermWarning(keyPath); warn != "" {
			fmt.Println("  " + warn)
		}
		fmt.Println("  la clave completa se ve y se copia en el panel; no se escribe en el log")
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
