package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// chromeAppWindow opens the UI as a desktop app window via Chrome/Edge/
// Chromium "app mode" (--app=URL): a chromeless native window that, unlike
// the system webviews, ships the Web Speech API — so voice input works in
// the desktop app. Blocks until the window is closed. Returns false when no
// Chromium-family browser is installed.
//
// A dedicated --user-data-dir forces a separate browser process; without it
// Chrome hands the window to an already-running instance and exits at once,
// which would make KubeMind quit while its window is still open. The profile
// is keyed by port so (a) relaunches on the same port reuse the profile and
// its remembered mic permission, and (b) a second concurrent KubeMind (on
// its fallback port) gets its own browser process instead of a hand-off.
func chromeAppWindow(url string) bool {
	if os.Getenv("KUBEMIND_WEBVIEW") == "1" {
		return false // operator explicitly wants the native webview
	}
	port := url[strings.LastIndex(url, ":")+1:]
	profile := filepath.Join(os.TempDir(), "kubemind-app-profile-"+port)
	for _, bin := range chromeCandidates() {
		path := bin
		if !filepath.IsAbs(bin) {
			p, err := exec.LookPath(bin)
			if err != nil {
				continue
			}
			path = p
		} else if _, err := os.Stat(bin); err != nil {
			continue
		}
		cmd := exec.Command(path,
			"--app="+url,
			"--user-data-dir="+profile,
			"--window-size=1440,900",
			"--no-first-run",
			"--no-default-browser-check",
		)
		if err := cmd.Start(); err != nil {
			continue
		}
		_ = cmd.Wait() // window closed → let main return and shut down
		return true
	}
	return false
}

func chromeCandidates() []string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			filepath.Join(home, "Applications/Google Chrome.app/Contents/MacOS/Google Chrome"),
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	case "windows":
		var out []string
		for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LocalAppData")} {
			if root == "" {
				continue
			}
			out = append(out,
				filepath.Join(root, `Google\Chrome\Application\chrome.exe`),
				filepath.Join(root, `Microsoft\Edge\Application\msedge.exe`),
			)
		}
		return out
	default: // linux and friends — resolved via $PATH
		return []string{"google-chrome", "google-chrome-stable", "microsoft-edge", "chromium", "chromium-browser"}
	}
}
