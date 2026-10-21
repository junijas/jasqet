//go:build windows

package main

// Jasqet's own title bar on Windows. The window keeps its normal frame, so
// resizing at the edges, the shadow, the rounded corners and Snap stay
// Windows' own, but Windows' grey caption strip goes: the app's top bar takes
// its place, with its own minimise, maximise/restore and close buttons
// (web/index.html). Dragging the bar hands over to Windows' own moving, so
// dragging to a screen edge still snaps.

import (
	"fmt"
	"sync"
	"unsafe"

	"github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

// appIconID: the icon's number inside Jasqet.exe (rsrc_windows_*.syso).
const appIconID = 2

var (
	procCallWindowProc         = user32.NewProc("CallWindowProcW")
	procDefWindowProc          = user32.NewProc("DefWindowProcW")
	procIsZoomed               = user32.NewProc("IsZoomed")
	procReleaseCapture         = user32.NewProc("ReleaseCapture")
	procSendMessage            = user32.NewProc("SendMessageW")
	procPostMessage            = user32.NewProc("PostMessageW")
	procGetCursorPos           = user32.NewProc("GetCursorPos")
	procGetDpiForWindow        = user32.NewProc("GetDpiForWindow")
	procGetSystemMetricsForDpi = user32.NewProc("GetSystemMetricsForDpi")
)

const (
	gwlpWndProc     = ^uintptr(3) // -4
	wsThickFrame    = 0x00040000
	wmSize          = 0x0005
	wmClose         = 0x0010
	wmNCDestroy     = 0x0082
	wmNCCalcSize    = 0x0083
	wmNCLButtonDown = 0x00A1
	sizeRestored    = 0
	sizeMaximized   = 2
	swMinimize      = 6
	swMaximize      = 3
	swRestore       = 9
	smCyFrame       = 33
	smCxPaddedBordr = 92
)

// What the page's top bar may ask Windows to do with the mouse: move the
// window, or size it from its top edge (the other edges are Windows' own).
var titleBarHits = map[string]uintptr{"drag": 2, "top": 12, "topleft": 13, "topright": 14}

type titleBar struct {
	w    webview2.WebView
	hwnd uintptr
	old  uintptr // the window's own message handler (go-webview2's)
	max  bool
}

var (
	titleBars    sync.Map // window → *titleBar
	titleBarProc = sync.OnceValue(func() uintptr { return windows.NewCallback(titleBarMsg) })
)

// ownTitleBar takes Windows' caption strip off w's window and lets its page
// drive the window through tgWin (min, max, close, drag, top…, state).
func ownTitleBar(w webview2.WebView) {
	t := takeCaption(uintptr(w.Window()), w)
	w.Bind("tgWin", t.do)
}

// takeCaption takes the caption strip off a window and keeps the rest of its frame.
func takeCaption(hwnd uintptr, w webview2.WebView) *titleBar {
	t := &titleBar{w: w, hwnd: hwnd, max: zoomed(hwnd)}
	titleBars.Store(hwnd, t)
	t.old, _, _ = procSetWindowLongPtr.Call(hwnd, gwlpWndProc, titleBarProc())
	procSetWindowPos.Call(hwnd, 0, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoZOrder|swpNoOwnerZOrder|swpFrameChanged)
	return t
}

func titleBarMsg(hwnd, msg, wp uintptr, lp unsafe.Pointer) uintptr {
	v, ok := titleBars.Load(hwnd)
	if !ok {
		r, _, _ := procDefWindowProc.Call(hwnd, msg, wp, uintptr(lp))
		return r
	}
	t := v.(*titleBar)
	switch msg {
	case wmNCCalcSize:
		// not while the viewer is full screen (the frame is off then)
		if wp != 0 && getStyle(hwnd)&wsThickFrame != 0 {
			r := (*winRect)(lp)
			top := r.Top
			res, _, _ := procCallWindowProc.Call(t.old, hwnd, msg, wp, uintptr(lp))
			r.Top = top // no caption strip: the page's top bar is the title bar
			if zoomed(hwnd) {
				r.Top += frameThickness(hwnd) // a maximised window reaches past the screen's edge
			}
			return res
		}
	case wmSize:
		if t.w != nil && (wp == sizeMaximized || wp == sizeRestored) {
			if max := wp == sizeMaximized; max != t.max {
				t.max = max
				t.w.Dispatch(func() { t.w.Eval(fmt.Sprintf("window.tgWinState && tgWinState(%v)", max)) })
			}
		}
	case wmNCDestroy:
		titleBars.Delete(hwnd)
	}
	res, _, _ := procCallWindowProc.Call(t.old, hwnd, msg, wp, uintptr(lp))
	return res
}

// do: what the page's top bar asks for. It answers "max" or "normal".
func (t *titleBar) do(action string) string {
	switch action {
	case "min":
		t.w.Dispatch(func() { procShowWindow.Call(t.hwnd, swMinimize) })
	case "max":
		t.w.Dispatch(func() {
			if zoomed(t.hwnd) {
				procShowWindow.Call(t.hwnd, swRestore)
			} else {
				procShowWindow.Call(t.hwnd, swMaximize)
			}
		})
	case "close": // as Windows' own X: the window closes, Jasqet stays by the clock
		procPostMessage.Call(t.hwnd, wmClose, 0, 0)
	default:
		if hit, ok := titleBarHits[action]; ok {
			t.w.Dispatch(func() {
				var pt struct{ X, Y int32 }
				procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
				procReleaseCapture.Call()
				// Windows' own moving and sizing (Snap included) take over from here
				procSendMessage.Call(t.hwnd, wmNCLButtonDown, hit, uintptr(uint16(pt.X))|uintptr(uint16(pt.Y))<<16)
			})
		}
	}
	if zoomed(t.hwnd) {
		return "max"
	}
	return "normal"
}

func zoomed(hwnd uintptr) bool {
	r, _, _ := procIsZoomed.Call(hwnd)
	return r != 0
}

// frameThickness: how far a maximised window's edge sits past the screen's.
func frameThickness(hwnd uintptr) int32 {
	if procGetDpiForWindow.Find() == nil && procGetSystemMetricsForDpi.Find() == nil {
		dpi, _, _ := procGetDpiForWindow.Call(hwnd)
		f, _, _ := procGetSystemMetricsForDpi.Call(smCyFrame, dpi)
		p, _, _ := procGetSystemMetricsForDpi.Call(smCxPaddedBordr, dpi)
		return int32(f + p)
	}
	f, _, _ := procGetSystemMetrics.Call(smCyFrame)
	p, _, _ := procGetSystemMetrics.Call(smCxPaddedBordr)
	return int32(f + p)
}
