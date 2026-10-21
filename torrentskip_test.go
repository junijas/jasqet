package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
)

// A torrent that went into "Show (1)" because a Show folder was already there
// cleans its unticked files out of "Show (1)", never out of your older Show.
func TestSkippedTorrentFilesGoFromItsOwnFolder(t *testing.T) {
	base := t.TempDir()
	info := &metainfo.Info{Name: "Show", Files: []metainfo.FileInfo{
		{Path: []string{"ep1.mkv"}, Length: 1}, {Path: []string{"extras", "setup.lnk"}, Length: 1},
	}}
	files := info.UpvertedFiles()
	older := filepath.Join(base, "Show")                               // yours, from before
	root := filepath.Join(base, "Show (1)")                            // where this torrent went
	for _, dir := range []string{older, filepath.Join(root, "Show")} { // files land in root\Name\…
		for _, f := range files {
			p := filepath.Join(dir, filepath.Join(f.BestPath()...))
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte("x"), 0o644)
		}
	}
	removeSkipped(&Download{Dir: root}, info, files, map[int]bool{1: true})
	if !fileExists(filepath.Join(older, "extras", "setup.lnk")) {
		t.Fatal("deleted setup.lnk from the older Show folder")
	}
	if fileExists(filepath.Join(root, "Show", "extras", "setup.lnk")) {
		t.Fatal("the skipped file is still in the new download")
	}
	if !fileExists(filepath.Join(root, "Show", "ep1.mkv")) || !fileExists(filepath.Join(older, "ep1.mkv")) {
		t.Fatal("deleted a file that wasn't skipped")
	}
}
