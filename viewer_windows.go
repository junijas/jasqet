//go:build windows

package main

// The viewer window on Windows: its own process (started by Jasqet with
// -viewer), so a file that crashes the player can never stop the downloads.
// It remembers its size and position, and if the page inside stops
// responding (a file crashed it) it shows "This file could not be displayed".

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/jchv/go-webview2"
)

var (
	procGetWindowPlacement = user32.NewProc("GetWindowPlacement")
	procSetWindowPlacement = user32.NewProc("SetWindowPlacement")
	procGetWindowLongPtr   = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtr   = user32.NewProc("SetWindowLongPtrW")
	procGetWindowLong      = user32.NewProc("GetWindowLongW")
	procSetWindowLong      = user32.NewProc("SetWindowLongW")
	procSetWindowPos       = user32.NewProc("SetWindowPos")
	procMonitorFromWindow  = user32.NewProc("MonitorFromWindow")
	procMonitorFromRect    = user32.NewProc("MonitorFromRect")
	procGetMonitorInfo     = user32.NewProc("GetMonitorInfoW")
)

type winRect struct{ Left, Top, Right, Bottom int32 }

type winPlacement struct {
	Length, Flags, ShowCmd uint32
	MinX, MinY, MaxX, MaxY int32
	Normal                 winRect
}

type monitorInfo struct {
	Size    uint32
	Monitor winRect
	Work    winRect
	Flags   uint32
}

const (
	gwlStyle           = ^uintptr(15) // -16
	wsOverlappedWindow = 0x00CF0000
	swpNoSize          = 0x0001
	swpNoMove          = 0x0002
	swpNoZOrder        = 0x0004
	swpFrameChanged    = 0x0020
	swpNoOwnerZOrder   = 0x0200
)

func getStyle(hwnd uintptr) uintptr {
	if procGetWindowLongPtr.Find() == nil {
		r, _, _ := procGetWindowLongPtr.Call(hwnd, gwlStyle)
		return r
	}
	r, _, _ := procGetWindowLong.Call(hwnd, gwlStyle)
	return r
}

func setStyle(hwnd, v uintptr) {
	if procSetWindowLongPtr.Find() == nil {
		procSetWindowLongPtr.Call(hwnd, gwlStyle, v)
		return
	}
	procSetWindowLong.Call(hwnd, gwlStyle, v)
}

type savedGeometry struct {
	Left, Top, Right, Bottom int32
	Max                      bool
}

func viewerGeometryFile() string {
	return filepath.Join(filepath.Dir(stateFilePath()), "viewer-window.json")
}

type viewerWin struct {
	w    webview2.WebView
	hwnd uintptr

	mu         sync.Mutex
	full       bool
	savedStyle uintptr
	savedPlace winPlacement
	geometry   *savedGeometry
	lastPong   time.Time
	ponged     bool
	file       string
	geo        bool // remember this window's size and place
}

func (v *viewerWin) placement() winPlacement {
	wp := winPlacement{Length: uint32(unsafe.Sizeof(winPlacement{}))}
	procGetWindowPlacement.Call(v.hwnd, uintptr(unsafe.Pointer(&wp)))
	return wp
}

// snapshot remembers where the window is (called on the window's thread).
func (v *viewerWin) snapshot() {
	v.mu.Lock()
	defer v.mu.Unlock()
	wp := v.placement()
	if v.full {
		wp = v.savedPlace
	}
	if wp.Normal.Right-wp.Normal.Left < 200 || wp.Normal.Bottom-wp.Normal.Top < 150 {
		return
	}
	v.geometry = &savedGeometry{wp.Normal.Left, wp.Normal.Top, wp.Normal.Right, wp.Normal.Bottom, wp.ShowCmd == 3}
}

func (v *viewerWin) saveGeometry() {
	v.mu.Lock()
	g := v.geometry
	v.mu.Unlock()
	if g != nil {
		b, _ := json.Marshal(g)
		writeFileAtomic(viewerGeometryFile(), b)
	}
}

func (v *viewerWin) restoreGeometry() {
	b, err := os.ReadFile(viewerGeometryFile())
	if err != nil {
		return
	}
	var g savedGeometry
	if json.Unmarshal(b, &g) != nil || g.Right-g.Left < 200 || g.Bottom-g.Top < 150 {
		return
	}
	r := winRect{g.Left, g.Top, g.Right, g.Bottom}
	if m, _, _ := procMonitorFromRect.Call(uintptr(unsafe.Pointer(&r)), 0); m == 0 {
		return // that screen isn't connected any more
	}
	wp := v.placement()
	wp.Normal = r
	wp.ShowCmd = 1
	if g.Max {
		wp.ShowCmd = 3
	}
	procSetWindowPlacement.Call(v.hwnd, uintptr(unsafe.Pointer(&wp)))
	v.geometry = &g
}

func (v *viewerWin) fullscreen(on bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if on == v.full {
		return
	}
	if on {
		v.savedStyle = getStyle(v.hwnd)
		v.savedPlace = v.placement()
		mon, _, _ := procMonitorFromWindow.Call(v.hwnd, 2)
		mi := monitorInfo{Size: uint32(unsafe.Sizeof(monitorInfo{}))}
		procGetMonitorInfo.Call(mon, uintptr(unsafe.Pointer(&mi)))
		setStyle(v.hwnd, v.savedStyle&^wsOverlappedWindow)
		r := mi.Monitor
		procSetWindowPos.Call(v.hwnd, 0, uintptr(r.Left), uintptr(r.Top), uintptr(r.Right-r.Left), uintptr(r.Bottom-r.Top), swpNoOwnerZOrder|swpFrameChanged)
	} else {
		setStyle(v.hwnd, v.savedStyle)
		procSetWindowPlacement.Call(v.hwnd, uintptr(unsafe.Pointer(&v.savedPlace)))
		procSetWindowPos.Call(v.hwnd, 0, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoZOrder|swpNoOwnerZOrder|swpFrameChanged)
	}
	v.full = on
}

func (v *viewerWin) front() {
	if r, _, _ := procIsIconic.Call(v.hwnd); r != 0 {
		procShowWindow.Call(v.hwnd, 9) // SW_RESTORE
	}
	procShowWindow.Call(v.hwnd, 5)
	procSetForegroundWindow.Call(v.hwnd)
}

// expect: a page is loading; it must answer within the time limit, even if
// it breaks before its first answer.
func (v *viewerWin) expect() {
	v.mu.Lock()
	v.lastPong, v.ponged = time.Now(), true
	v.mu.Unlock()
}

func appRunning(start string) bool {
	u, err := url.Parse(start)
	if err != nil {
		return false
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(u.Scheme + "://" + u.Host + "/api/ping")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

func (v *viewerWin) pong(file string) {
	v.mu.Lock()
	v.lastPong, v.ponged, v.file = time.Now(), true, file
	v.mu.Unlock()
}

// watch notices when the page has stopped answering (a file crashed or froze
// it). onStuck gets the file that was showing; it returns false to stop
// watching.
func (v *viewerWin) watch(stop <-chan struct{}, onStuck func(file string) bool) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	last := time.Now()
	v.expect()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		if time.Since(last) > 10*time.Second { // the computer was asleep
			v.mu.Lock()
			v.lastPong = time.Now()
			v.mu.Unlock()
		}
		last = time.Now()
		v.w.Dispatch(func() {
			if v.geo {
				v.snapshot()
			}
			v.w.Eval("window.tgPing && window.tgPing()")
		})
		v.mu.Lock()
		stuck := v.ponged && time.Since(v.lastPong) > 20*time.Second
		file := v.file
		if stuck {
			v.ponged = false
		}
		v.mu.Unlock()
		if stuck && !onStuck(file) {
			return
		}
	}
}

func runViewer(u string) {
	if !strings.HasPrefix(u, "http://127.0.0.1:") {
		return
	}
	runtime.LockOSThread()
	defer func() {
		if r := recover(); r != nil {
			openBrowser(u)
		}
	}()
	dataDir := filepath.Join(filepath.Dir(stateFilePath()), "WebView2-viewer")
	os.MkdirAll(dataDir, 0o755)
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		DataPath:  dataDir,
		WindowOptions: webview2.WindowOptions{
			Title: "Jasqet Viewer", Width: 1000, Height: 680, IconId: appIconID, Center: true,
		},
	})
	if w == nil {
		openBrowser(u)
		return
	}
	v := &viewerWin{w: w, hwnd: uintptr(w.Window()), geo: true}
	if q, err := url.Parse(u); err == nil {
		v.file = q.Query().Get("f")
	}
	w.SetSize(420, 300, webview2.HintMin)
	v.restoreGeometry()
	w.Bind("tgClose", func() {
		v.snapshot()
		v.saveGeometry()
		w.Terminate()
	})
	w.Bind("tgFront", v.front)
	w.Bind("tgFullscreen", v.fullscreen)
	w.Bind("tgTitle", func(t string) { w.SetTitle(t) })
	w.Bind("tgPong", v.pong)
	stop := make(chan struct{})
	go v.watch(stop, func(file string) bool {
		if !appRunning(u) { // Jasqet itself was closed: nothing to show
			v.w.Dispatch(func() { v.w.Terminate() })
			return false
		}
		if q, err := url.Parse(u); err == nil {
			vals := q.Query()
			vals.Set("f", file)
			vals.Set("crashed", "1")
			q.RawQuery = vals.Encode()
			target := q.String()
			v.expect()
			v.w.Dispatch(func() {
				v.fullscreen(false)
				v.w.Navigate(target)
			})
		}
		return true
	})
	w.Navigate(u)
	w.Run()
	close(stop)
	v.saveGeometry()
}
