//go:build windows

package main

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// isMeteredNetwork asks Windows whether the current connection is metered
// (phone hotspots and capped networks are marked this way).
func isMeteredNetwork() (bool, string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	script := `$ErrorActionPreference='SilentlyContinue';` +
		`[void][Windows.Networking.Connectivity.NetworkInformation,Windows.Networking.Connectivity,ContentType=WindowsRuntime];` +
		`$p=[Windows.Networking.Connectivity.NetworkInformation]::GetInternetConnectionProfile();` +
		`if($p){$c=$p.GetConnectionCost();Write-Output ("{0}|{1}|{2}|{3}" -f $c.NetworkCostType,$c.ApproachingDataLimit,$c.OverDataLimit,$p.ProfileName)}else{'none'}`
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return false, "", false
	}
	line := strings.TrimSpace(string(out))
	if line == "" || line == "none" {
		return false, "", true
	}
	f := strings.SplitN(line, "|", 4)
	if len(f) < 4 {
		return false, "", false
	}
	metered := f[0] == "Fixed" || f[0] == "Variable" || f[1] == "True" || f[2] == "True"
	return metered, f[3], true
}
