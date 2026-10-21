package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// "Clear finished" can be undone: the entries come back as they were.
// Unfinished ones are never touched.
func TestClearFinishedCanBeUndone(t *testing.T) {
	isolateConfig(t)
	m := newTestManager(t)
	for id, st := range map[string]Status{"a": StDone, "b": StDone, "c": StQueued} {
		m.mu.Lock()
		m.downloads[id] = &Download{ID: id, FileName: id + ".zip", Status: st}
		m.mu.Unlock()
	}
	if n := m.ClearDone(); n != 2 {
		t.Fatalf("cleared %d, want the 2 finished ones", n)
	}
	if m.get("a") != nil || m.get("b") != nil || m.get("c") == nil {
		t.Fatal("clear should take only the finished ones off the list")
	}
	if n := m.UndoClear(); n != 2 || m.get("a") == nil || m.get("b") == nil {
		t.Fatalf("undo brought back %d", n)
	}
	if n := m.UndoClear(); n != 0 {
		t.Fatal("the same clear was undone twice")
	}
}

// After the undo time they're gone for good, including what Delete removes
// besides the entry (a finished torrent's saved .torrent file), and not before.
func TestClearFinishedIsFinalAfterTheUndoTime(t *testing.T) {
	isolateConfig(t)
	old := clearUndoFor
	clearUndoFor = 100 * time.Millisecond
	defer func() { clearUndoFor = old }()
	m := newTestManager(t)
	os.MkdirAll(torrentsDir(), 0o755)
	saved := filepath.Join(torrentsDir(), "ab12.torrent")
	os.WriteFile(saved, []byte("d4:infod4:name1:xee"), 0o644)
	m.mu.Lock()
	m.downloads["t"] = &Download{ID: "t", Kind: "torrent", InfoHash: "ab12", FileName: "x", Status: StDone}
	m.mu.Unlock()
	m.ClearDone()
	if !fileExists(saved) {
		t.Fatal("the .torrent file went before the undo time was over")
	}
	deadline := time.Now().Add(5 * time.Second)
	for fileExists(saved) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if fileExists(saved) {
		t.Fatal("the .torrent file is still there after the undo time")
	}
	if n := m.UndoClear(); n != 0 || m.get("t") != nil {
		t.Fatal("undo still worked after the undo time")
	}
}

// The window's button: "clear" says how many it took off, and "unclear" puts
// them back.
func TestClearFinishedButtonAndUndo(t *testing.T) {
	isolateConfig(t)
	s, h := newTestServer(t)
	s.m.mu.Lock()
	s.m.downloads["a"] = &Download{ID: "a", FileName: "a.pdf", Status: StDone}
	s.m.mu.Unlock()
	app := "http://127.0.0.1:9614"
	count := func(action string) int {
		w := call(h, "POST", "/api/action", app, `{"id":"*","action":"`+action+`"}`, keyHeader, s.key.window)
		var r struct {
			OK    bool `json:"ok"`
			Count int  `json:"count"`
		}
		if json.Unmarshal(w.Body.Bytes(), &r) != nil || !r.OK {
			t.Fatalf("%s: %d %s", action, w.Code, w.Body)
		}
		return r.Count
	}
	if n := count("clear"); n != 1 || s.m.get("a") != nil {
		t.Fatalf("clear took %d off", n)
	}
	if n := count("unclear"); n != 1 || s.m.get("a") == nil {
		t.Fatalf("undo put back %d", n)
	}
}
