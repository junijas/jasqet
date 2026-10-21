package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Uses sample archives in JASQET_ARCHIVES (skipped when not set).
func TestArchives(t *testing.T) {
	dir := os.Getenv("JASQET_ARCHIVES")
	if dir == "" {
		t.Skip("set JASQET_ARCHIVES to a folder of sample archives")
	}
	tmp := t.TempDir()
	for _, name := range []string{"test.zip", "test.7z", "test.tar.gz", "rar5-solid.rar", "rar3-solid.rar", "seektest.rar"} {
		src := filepath.Join(dir, name)
		cp := filepath.Join(tmp, name)
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(cp, b, 0o644)
		out, n, err := extractArchive(context.Background(), cp)
		if err != nil || n == 0 {
			t.Errorf("%s: %d files, %v", name, n, err)
			continue
		}
		t.Logf("%s: %d files into %s", name, n, filepath.Base(out))
	}
	for name, want := range map[string]bool{"rar5-psw.rar": true, "rar5-hpsw.rar": true, "rar3-comment-psw.rar": true, "rar5-solid.rar": false, "rar3-solid.rar": false, "locked.7z": true, "test.7z": false} {
		p := filepath.Join(dir, name)
		k := kRar
		if filepath.Ext(name) == ".7z" {
			k = k7z
		}
		if got := archiveEncrypted(p, k); got != want {
			t.Errorf("archiveEncrypted(%s) = %v, want %v", name, got, want)
		}
	}
	// a locked archive can't be unpacked: say so
	b, _ := os.ReadFile(filepath.Join(dir, "locked.7z"))
	cp := filepath.Join(tmp, "locked.7z")
	os.WriteFile(cp, b, 0o644)
	if _, _, err := extractArchive(context.Background(), cp); err == nil {
		t.Error("locked.7z should fail")
	} else {
		t.Log("locked.7z:", err)
	}
}
