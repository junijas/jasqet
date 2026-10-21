//go:build !windows

package main

// Metered-network detection is only available on Windows for now.
func isMeteredNetwork() (bool, string, bool) { return false, "", false }
