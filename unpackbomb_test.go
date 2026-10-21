package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testZip writes a ZIP with these files (a name ending in "/" is a folder).
func testZip(t *testing.T, p string, files map[string][]byte) {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for name, data := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(data)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// testTarGz writes a .tar.gz with these files, in this order.
func testTarGz(t *testing.T, p string, names []string, data [][]byte) {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for i, name := range names {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data[i])), Typeflag: tar.TypeReg})
		tw.Write(data[i])
	}
	tw.Close()
	gz.Close()
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// onlyFile: the folder holds just this one file (nothing was left behind).
func onlyFile(t *testing.T, dir, name string) {
	t.Helper()
	list, _ := os.ReadDir(dir)
	if len(list) != 1 || list[0].Name() != name {
		var got []string
		for _, e := range list {
			got = append(got, e.Name())
		}
		t.Fatalf("the folder should hold only %s, it holds %q", name, got)
	}
}

// A ZIP of 20 MB of zeros is about 20 KB: 1000 times its size, so a bomb.
func TestZipBombIsRefusedAndLeavesNoFolder(t *testing.T) {
	smallBombs(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "bomb.zip")
	testZip(t, p, map[string][]byte{"big.bin": make([]byte, 20<<20), "note.txt": []byte("hi")})
	if st, _ := os.Stat(p); st.Size()*unpackRatio > 20<<20 {
		t.Fatalf("the test ZIP is %d bytes: not small enough to be a bomb", st.Size())
	}
	_, _, err := extractArchive(context.Background(), p)
	if !errors.Is(err, errUnpackBomb) {
		t.Fatalf("want the zip bomb error, got %v", err)
	}
	onlyFile(t, dir, "bomb.zip")
}

// A .tar.gz doesn't list its sizes up front: it stops inside the huge file,
// and the half-written folder is removed.
func TestTarBombStopsInsideTheHugeFileAndLeavesNoFolder(t *testing.T) {
	smallBombs(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "bomb.tar.gz")
	testTarGz(t, p, []string{"a.txt", "big.bin"}, [][]byte{[]byte("first"), make([]byte, 20<<20)})
	_, n, err := extractArchive(context.Background(), p)
	if !errors.Is(err, errUnpackBomb) {
		t.Fatalf("want the zip bomb error, got %v", err)
	}
	if n == 0 {
		t.Fatal("a.txt came out first, so the folder had something in it to remove")
	}
	onlyFile(t, dir, "bomb.tar.gz")
}

// One huge file is cut off at the limit, and a stop is noticed between pieces.
func TestUnpackCopyStopsEarly(t *testing.T) {
	var out unpackCounter
	var total int64
	buf := make([]byte, 64<<10)
	endless := io.LimitReader(bombZeros{}, 1<<40)
	err := unpackCopy(context.Background(), &out, endless, buf, &total, 1<<20, errUnpackBomb)
	if !errors.Is(err, errUnpackBomb) || out.n > 1<<20 {
		t.Fatalf("got %v after writing %d bytes, want the bomb error by 1 MB", err, out.n)
	}
	ctx, cancel := context.WithCancel(context.Background())
	out, total = unpackCounter{}, 0
	err = unpackCopy(ctx, &out, readThen{bombZeros{}, cancel}, buf, &total, 1<<40, errUnpackBomb)
	if !errors.Is(err, context.Canceled) || out.n > int64(len(buf)) {
		t.Fatalf("got %v after writing %d bytes, want it to stop after one piece", err, out.n)
	}
}

type unpackCounter struct{ n int64 }

func (w *unpackCounter) Write(p []byte) (int, error) { w.n += int64(len(p)); return len(p), nil }

// readThen calls stop after its first read.
type readThen struct {
	r    io.Reader
	stop func()
}

func (r readThen) Read(p []byte) (int, error) { n, err := r.r.Read(p); r.stop(); return n, err }

func TestNormalZipStillUnpacks(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "photos.zip")
	files := map[string][]byte{
		"photos/":           nil,
		"photos/readme.txt": []byte("holiday pictures"),
		"photos/a/b.txt":    bytes.Repeat([]byte("some text that packs well "), 40),
	}
	testZip(t, p, files)
	dest, n, err := extractArchive(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("unpacked %d files, want 2", n)
	}
	for name, want := range files {
		if strings.HasSuffix(name, "/") {
			continue
		}
		got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(name)))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: %v, %q", name, err, got)
		}
	}
}

// Over 100,000 files (fewer here) is refused: a ZIP before anything is
// written, a .tar.gz as it goes, and then nothing is left behind either.
func TestArchiveWithTooManyFilesIsRefused(t *testing.T) {
	old := unpackMaxEntries
	unpackMaxEntries = 5
	defer func() { unpackMaxEntries = old }()
	dir := t.TempDir()
	files := map[string][]byte{}
	var names []string
	var data [][]byte
	for _, c := range "abcdefghij" {
		files[string(c)+".txt"] = []byte("x")
		names = append(names, string(c)+".txt")
		data = append(data, []byte("x"))
	}
	testZip(t, filepath.Join(dir, "many.zip"), files)
	testTarGz(t, filepath.Join(dir, "many.tar.gz"), names, data)
	for _, name := range []string{"many.zip", "many.tar.gz"} {
		if _, _, err := extractArchive(context.Background(), filepath.Join(dir, name)); !errors.Is(err, errUnpackTooMany) {
			t.Errorf("%s: want the too many files error, got %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "many")); err == nil {
		t.Fatal("the half-unpacked folder was left behind")
	}
}

// At least 1 GB stays free on the drive (here: more than any drive has).
func TestUnpackLeavesFreeSpaceOnTheDrive(t *testing.T) {
	dir := t.TempDir()
	if diskFree(dir) < 0 {
		t.Skip("this system doesn't tell the free space")
	}
	old := unpackKeepFree
	unpackKeepFree = 1 << 62
	defer func() { unpackKeepFree = old }()
	p := filepath.Join(dir, "small.zip")
	testZip(t, p, map[string][]byte{"a.txt": []byte("hello")})
	if _, _, err := extractArchive(context.Background(), p); !errors.Is(err, errUnpackNoSpace) {
		t.Fatalf("want the free space error, got %v", err)
	}
	onlyFile(t, dir, "small.zip")
}

// The archive itself passes its checks, but a program inside must still show
// on its badge and in what the rule reports.
func TestDangerousFileInsideAZipShowsOnItsBadge(t *testing.T) {
	m := newTestManager(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "holiday.zip")
	exe := append([]byte("MZ"), make([]byte, 200)...)
	testZip(t, p, map[string][]byte{"holiday.mp4.exe": exe, "notes.txt": []byte("hi")})
	d := &Download{Dir: dir, FileName: "holiday.zip", Safety: &Safety{Level: "ok"}}
	res, err := m.runRule(context.Background(), d, &AfterRule{On: true, Unzip: true}, &p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "unzipped 2 files") || !strings.Contains(res, "careful with what's inside: holiday.mp4.exe") {
		t.Fatalf("the rule should report the program inside: %q", res)
	}
	if d.Safety.Level != "danger" || len(d.Safety.Notes) == 0 || !strings.Contains(d.Safety.Notes[0], "holiday.mp4.exe") {
		t.Fatalf("the badge should warn about the program inside: %+v", d.Safety)
	}
	// a ZIP of harmless files keeps its "Checked"
	p2 := filepath.Join(dir, "texts.zip")
	testZip(t, p2, map[string][]byte{"a.txt": []byte("hello")})
	d2 := &Download{Dir: dir, FileName: "texts.zip", Safety: &Safety{Level: "ok"}}
	res, err = m.runRule(context.Background(), d2, &AfterRule{On: true, Unzip: true}, &p2)
	if err != nil || strings.Contains(res, "careful") || d2.Safety.Level != "ok" {
		t.Fatalf("harmless files gave %q, %v, %+v", res, err, d2.Safety)
	}
}

// A file taken out of a ZIP on a server never inflates past the size the ZIP
// lists for it.
func TestRemoteZipFileStopsAtItsListedSize(t *testing.T) {
	dir := t.TempDir()
	deflated := func(data []byte) string {
		var b bytes.Buffer
		fw, _ := flate.NewWriter(&b, flate.DefaultCompression)
		fw.Write(data)
		fw.Close()
		part := filepath.Join(dir, "part")
		os.WriteFile(part, b.Bytes(), 0o644)
		return part
	}
	final := filepath.Join(dir, "file.bin")
	// the ZIP says 1000 bytes, but 8 MB come out
	small := make([]byte, 1000)
	z := &ZipPart{Method: zip.Deflate, Size: 1000, CRC32: crc32.ChecksumIEEE(small)}
	err := unpackZipPart(deflated(make([]byte, 8<<20)), final, z)
	if err == nil || !strings.Contains(err.Error(), "bigger than the ZIP says") {
		t.Fatalf("want the bigger than listed error, got %v", err)
	}
	for _, f := range []string{final, final + ".unpack"} {
		if _, err := os.Stat(f); err == nil {
			t.Fatalf("%s was left behind", filepath.Base(f))
		}
	}
	// the right size still unpacks
	if err := unpackZipPart(deflated(small), final, z); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(final); !bytes.Equal(got, small) {
		t.Fatal("the file differs")
	}
}

// bombZeros reads zeros without end.
type bombZeros struct{}

func (bombZeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// smallBombs: tests use bombs of a few MB, so the 1 GB that's always fine is
// turned off for them.
func smallBombs(t *testing.T) {
	old := unpackAtLeast
	unpackAtLeast = 0
	t.Cleanup(func() { unpackAtLeast = old })
}

// A small archive of very repetitive data (logs, say) unpacks: up to 1 GB is
// always fine, however much it shrank.
func TestSmallRepetitiveArchiveStillUnpacks(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "logs.zip")
	testZip(t, p, map[string][]byte{"server.log": make([]byte, 20<<20)})
	out, _, err := extractArchive(context.Background(), p)
	if err != nil {
		t.Fatalf("a 20 MB log in a small ZIP was refused: %v", err)
	}
	if st, err := os.Stat(filepath.Join(out, "server.log")); err != nil || st.Size() != 20<<20 {
		t.Fatalf("unpacked log: %v", err)
	}
}
