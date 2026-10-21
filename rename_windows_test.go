//go:build windows

package main

import "testing"

// The turboget:// links are only removed when they're this app's own.
func TestOldProtocolIsOurs(t *testing.T) {
	exe := `C:\Users\A\AppData\Local\Programs\TurboGet\Jasqet.exe`
	gone := func(string) bool { return false }
	there := func(string) bool { return true }
	for _, c := range []struct {
		cmd    string
		exists func(string) bool
		want   bool
	}{
		{`"C:\Users\A\AppData\Local\Programs\TurboGet\TurboGet.exe" "%1"`, there, true}, // installed over it
		{`"c:\users\a\appdata\local\programs\turboget\TURBOGET.EXE" "%1"`, there, true},
		{`"D:\Apps\TurboGet\TurboGet.exe" "%1"`, gone, true},           // the old program was deleted
		{`"D:\Apps\TurboGet\TurboGet.exe" "%1"`, there, false},         // still there: maybe still used
		{`"C:\Program Files\Turbo\TurboGetPro.exe" "%1"`, gone, false}, // another app's
		{`"C:\Users\A\AppData\Local\Programs\TurboGet\other.exe" "%1"`, there, false},
		{``, gone, false},
	} {
		if got := oldProtocolIsOurs(c.cmd, exe, c.exists); got != c.want {
			t.Errorf("oldProtocolIsOurs(%s) = %v, want %v", c.cmd, got, c.want)
		}
	}
}
