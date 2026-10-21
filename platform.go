package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

func defaultDownloadDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, "Downloads", "Jasqet")
}

func stateFilePath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "Jasqet", "state.json")
}

// oldStateDir: where the list and settings were kept while Jasqet was still
// called TurboGet (up to 1.6.0).
func oldStateDir() string {
	return filepath.Join(filepath.Dir(filepath.Dir(stateFilePath())), "TurboGet")
}

// moveOldState moves TurboGet's folder (the download list, settings, tools,
// torrents, the window's own data) to Jasqet's the first time Jasqet starts,
// so nothing is lost with the new name. Once Jasqet has a list of its own,
// the old folder is left alone.
func moveOldState(oldDir, newDir string) error {
	for _, n := range []string{"state.json", "state.json.tmp", "state.json.bak"} {
		if _, err := os.Lstat(filepath.Join(newDir, n)); err == nil {
			return nil
		}
	}
	if st, err := os.Stat(oldDir); err != nil || !st.IsDir() {
		return nil // TurboGet was never here
	}
	// Usually one rename. The old window's browser parts may take a moment
	// to let go of their files.
	for try := 0; try < 3; try++ {
		if os.Rename(oldDir, newDir) == nil {
			return nil
		}
		if _, err := os.Stat(newDir); err == nil {
			break // the new folder is there already: move what's inside instead
		}
		time.Sleep(time.Second)
	}
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(oldDir)
	if err != nil {
		return err
	}
	var firstErr error
	for _, e := range entries {
		from, to := filepath.Join(oldDir, e.Name()), filepath.Join(newDir, e.Name())
		if _, err := os.Lstat(to); err == nil {
			continue // Jasqet already has its own
		}
		err := os.Rename(from, to)
		if err != nil && e.Type().IsRegular() { // a busy file (the list itself): at least a copy
			var b []byte
			if b, err = os.ReadFile(from); err == nil {
				err = os.WriteFile(to, b, 0o600)
			}
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	os.Remove(oldDir) // only goes when nothing is left in it
	return firstErr
}

// testLog: for automated tests, what would have been opened is written to
// this file instead (JASQET_OPEN_LOG); normal use never sets it.
func testLog(what, p string) bool {
	f := os.Getenv("JASQET_OPEN_LOG")
	if f == "" {
		return false
	}
	if fh, err := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		fh.WriteString(what + "\t" + p + "\n")
		fh.Close()
	}
	return true
}

// openPath opens a file with its default program.
func openPath(p string) error {
	if testLog("open", p) {
		return nil
	}
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", p).Start()
	case "darwin":
		return exec.Command("open", p).Start()
	default:
		return exec.Command("xdg-open", p).Start()
	}
}

// revealPath opens the folder containing p and highlights it when the OS allows.
func revealPath(p string) error {
	if testLog("reveal", p) {
		return nil
	}
	switch runtime.GOOS {
	case "windows":
		if _, err := os.Stat(p); err == nil {
			return exec.Command("explorer", "/select,", p).Start()
		}
		return exec.Command("explorer", filepath.Dir(p)).Start()
	case "darwin":
		return exec.Command("open", "-R", p).Start()
	default:
		return exec.Command("xdg-open", filepath.Dir(p)).Start()
	}
}

func openBrowser(u string) {
	if testLog("browser", u) {
		return
	}
	switch runtime.GOOS {
	case "windows":
		// Open as a clean app window (no tabs or address bar) using Edge or Chrome.
		for _, p := range []string{
			os.Getenv("ProgramFiles(x86)") + `\Microsoft\Edge\Application\msedge.exe`,
			os.Getenv("ProgramFiles") + `\Microsoft\Edge\Application\msedge.exe`,
			os.Getenv("ProgramFiles") + `\Google\Chrome\Application\chrome.exe`,
			os.Getenv("ProgramFiles(x86)") + `\Google\Chrome\Application\chrome.exe`,
			os.Getenv("LocalAppData") + `\Google\Chrome\Application\chrome.exe`,
		} {
			if _, err := os.Stat(p); err == nil {
				if exec.Command(p, "--app="+u, "--window-size=1100,720").Start() == nil {
					return
				}
			}
		}
		exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	case "darwin":
		if exec.Command("open", "-na", "Google Chrome", "--args", "--app="+u).Run() != nil {
			exec.Command("open", u).Start()
		}
	default:
		exec.Command("xdg-open", u).Start()
	}
}

// pickFolder shows the operating system's folder chooser. Returns "" if cancelled.
func pickFolder(start string) string {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		script := `Add-Type -AssemblyName System.Windows.Forms;` +
			`$f = New-Object System.Windows.Forms.FolderBrowserDialog;` +
			`$f.Description = 'Choose where Jasqet saves downloads';` +
			`$f.SelectedPath = $env:TG_START;` +
			`$w = New-Object System.Windows.Forms.Form -Property @{TopMost=$true};` +
			`if ($f.ShowDialog($w) -eq 'OK') { [Console]::OutputEncoding=[Text.Encoding]::UTF8; Write-Output $f.SelectedPath }`
		cmd = exec.Command("powershell", "-NoProfile", "-STA", "-Command", script)
		cmd.Env = append(os.Environ(), "TG_START="+start)
		hideWindow(cmd)
	case "darwin":
		cmd = exec.Command("osascript", "-e", `POSIX path of (choose folder with prompt "Choose where Jasqet saves downloads")`)
	default:
		cmd = exec.Command("zenity", "--file-selection", "--directory", "--filename="+start+"/")
	}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	p := string(out)
	for len(p) > 0 && (p[len(p)-1] == '\n' || p[len(p)-1] == '\r') {
		p = p[:len(p)-1]
	}
	if len(p) > 1 && (p[len(p)-1] == '/') && runtime.GOOS == "darwin" {
		p = p[:len(p)-1]
	}
	return p
}
