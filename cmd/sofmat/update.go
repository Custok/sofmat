package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Custok/sofmat/internal/coordinator"
)

// version is injected at build time (-ldflags "-X main.version=YYYYMMDDHHMM").
// "dev" (go run / unbuilt) never self-updates.
var version = "dev"

const releasesAPI = "https://api.github.com/repos/Custok/sofmat/releases/latest"

// assetName is this platform's release asset — must match the names uploaded to
// the GitHub release.
func assetName() string {
	switch runtime.GOOS {
	case "windows":
		return "soflink.exe"
	case "darwin":
		if runtime.GOARCH == "arm64" {
			return "soflink-macos-arm64"
		}
		return "soflink-macos-intel"
	default: // linux
		if runtime.GOARCH == "arm64" {
			return "soflink-aarch64.AppImage"
		}
		return "soflink-x86_64.AppImage"
	}
}

// selfPath is the file to replace: the AppImage bundle when running as one, else
// the executable itself.
func selfPath() string {
	if ap := os.Getenv("APPIMAGE"); ap != "" {
		return ap
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if r, e := filepath.EvalSymlinks(exe); e == nil {
		return r
	}
	return exe
}

// checkAndUpdate queries GitHub for a newer release and, if found, downloads this
// platform's asset, swaps the running file and re-execs into it. Best-effort: any
// failure just logs and continues on the current version.
func checkAndUpdate() {
	defer func() { _ = recover() }()
	if version == "dev" {
		return
	}
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get(releasesAPI)
	if err != nil || resp == nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var rel struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if json.NewDecoder(resp.Body).Decode(&rel) != nil {
		return
	}
	latest := strings.TrimPrefix(rel.Tag, "v")
	if latest == "" || latest <= version { // sortable YYYYMMDDHHMM stamps
		return
	}
	var url string
	for _, a := range rel.Assets {
		if a.Name == assetName() {
			url = a.URL
			break
		}
	}
	if url == "" {
		return
	}
	fmt.Printf("soflink: nueva version %s (tengo %s) - actualizando...\n", latest, version)
	if err := applyUpdate(client, url); err != nil {
		fmt.Printf("soflink: auto-update fallo (%v) - sigo con la version actual\n", err)
	}
}

// updateCheckEvery es cada cuánto re-comprueba GitHub una guardia en runtime.
const updateCheckEvery = 30 * time.Minute

// periodicUpdate re-lanza checkAndUpdate periódicamente mientras el proceso vive,
// para que un nodo de guardia coja releases nuevas sin esperar a un reinicio
// manual (feedback de node-b/c/d: el auto-update solo saltaba al arranque). Al
// encontrar versión nueva, checkAndUpdate hace swap + re-exec (os.Exit) aquí.
func periodicUpdate() {
	if version == "dev" {
		return
	}
	t := time.NewTicker(updateCheckEvery)
	defer t.Stop()
	for range t.C {
		if coordinator.AutoUpdateOn() { // the header checkbox can pause auto-update live
			checkAndUpdate()
		}
	}
}

func applyUpdate(client *http.Client, url string) error {
	self := selfPath()
	if self == "" {
		return fmt.Errorf("no self path")
	}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %d", resp.StatusCode)
	}
	newPath := self + ".new"
	f, err := os.OpenFile(newPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		_ = os.Remove(newPath)
		return err
	}
	if n < 1_000_000 { // sanity: a real binary is > 1 MB
		_ = os.Remove(newPath)
		return fmt.Errorf("download too small (%d bytes)", n)
	}
	// Swap: a running file can be RENAMED (even on Windows), just not overwritten.
	oldPath := self + ".old"
	_ = os.Remove(oldPath)
	if err := os.Rename(self, oldPath); err != nil {
		_ = os.Remove(newPath)
		return err
	}
	if err := os.Rename(newPath, self); err != nil {
		_ = os.Rename(oldPath, self) // roll back
		return err
	}
	_ = os.Chmod(self, 0o755)
	// Re-exec into the freshly installed binary with the same args.
	cmd := exec.Command(self, os.Args[1:]...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := cmd.Start(); err != nil {
		return err
	}
	os.Exit(0)
	return nil
}
