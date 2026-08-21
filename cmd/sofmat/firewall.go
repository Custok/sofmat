package main

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// openPanel pops the embedded dashboard in the default browser shortly after
// startup, so running the binary feels like a desktop app (à la LM Studio).
// Best-effort: on a headless node the open command just no-ops.
func openPanel(listen string) {
	port := listen
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		port = listen[i+1:]
	}
	url := "http://localhost:" + port + "/"
	time.Sleep(1500 * time.Millisecond) // give the server a moment to bind
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// ensureFirewall best-effort opens the daemon's listen port so the panel and API
// are reachable from the LAN. Without it every user hits a silent timeout — the
// socket binds fine but the OS firewall drops inbound (timeout, not refused).
// Windows: add a netsh allow rule, elevating via UAC if the process isn't admin
// (the "ask for permission" prompt). Linux: try ufw. Failures only log; serving
// still starts (loopback always works).
func ensureFirewall(listen string) {
	port := listen
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		port = listen[i+1:]
	}
	if port == "" {
		return
	}
	name := "soflink-" + port
	switch runtime.GOOS {
	case "windows":
		// Already elevated → succeeds silently and idempotently.
		_ = exec.Command("netsh", "advfirewall", "firewall", "delete", "rule", "name="+name).Run()
		add := exec.Command("netsh", "advfirewall", "firewall", "add", "rule",
			"name="+name, "dir=in", "action=allow", "protocol=TCP", "localport="+port)
		if err := add.Run(); err == nil {
			fmt.Printf("firewall: puerto %s abierto (inbound TCP)\n", port)
			return
		}
		// Not admin → relaunch netsh elevated; Windows shows the UAC prompt.
		ps := fmt.Sprintf("Start-Process netsh -Verb RunAs -WindowStyle Hidden -ArgumentList "+
			"'advfirewall','firewall','add','rule','name=%s','dir=in','action=allow','protocol=TCP','localport=%s'",
			name, port)
		if err := exec.Command("powershell", "-NoProfile", "-Command", ps).Run(); err != nil {
			fmt.Printf("firewall: no pude abrir el puerto %s automaticamente — acepta el aviso de permisos o abrelo a mano\n", port)
		} else {
			fmt.Printf("firewall: pedi abrir el puerto %s (acepta el aviso de Windows)\n", port)
		}
	case "linux":
		if err := exec.Command("ufw", "allow", port+"/tcp").Run(); err == nil {
			fmt.Printf("firewall: ufw allow %s/tcp\n", port)
		}
	}
}
