//go:build windows

package main

// No white flash when the window opens. Windows would show the new window
// blank (white) while the browser engine starts, then the engine's own white
// until the page has drawn. So the window is created cloaked (there, but not
// shown), the engine paints in the page's own background, and the page
// uncloaks the window once it has drawn (tgShown; desktop_windows.go).

import (
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	dwmapi                    = windows.NewLazySystemDLL("dwmapi.dll")
	procDwmSetWindowAttribute = dwmapi.NewProc("DwmSetWindowAttribute")
	gdi32                     = windows.NewLazySystemDLL("gdi32.dll")
	procCreateSolidBrush      = gdi32.NewProc("CreateSolidBrush")
	procSetWindowsHookEx      = user32.NewProc("SetWindowsHookExW")
	procUnhookWindowsHookEx   = user32.NewProc("UnhookWindowsHookEx")
	procCallNextHookEx        = user32.NewProc("CallNextHookEx")
	procGetClassName          = user32.NewProc("GetClassNameW")
	procSetClassLongPtr       = user32.NewProc("SetClassLongPtrW")
)

const (
	whCBT                     = 5
	hcbtCreateWnd             = 3
	hcbtActivate              = 5
	dwmwaUseImmersiveDarkMode = 20
	dwmwaCloak                = 13
	gclpHbrBackground         = ^uintptr(9) // -10
)

// pageBackground: the page's background (index.html --bg) in its theme
// ("light", "dark", or "" like Windows), as WebView2 and as GDI want it.
func pageBackground(theme string) (argb string, colorref uintptr, dark bool) {
	dark = theme == "dark" || theme == "" && windowsIsDark()
	if dark {
		return "FF0E1116", 0x0016110E, true // #0e1116
	}
	return "FFF4F5F7", 0x00F7F5F4, false // #f4f5f7
}

// windowsIsDark: "Choose your app mode" is Dark.
func windowsIsDark() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("AppsUseLightTheme")
	return err == nil && v == 0
}

var (
	splashMu    sync.Mutex
	splashBrush [2]uintptr // light, dark: kept, as the window class holds on to them
	splashDark  bool       // the window being made is dark
	splashProc  = sync.OnceValue(func() uintptr { return windows.NewCallback(splashCBT) })
)

// startCloaked: windows of the webview class this thread makes from now on
// start cloaked and in the page's colours, until the returned func is called
// (right after the window is made). Also sets the engine's background.
// Call on the thread that makes the window.
func startCloaked(theme string) (done func()) {
	argb, colorref, dark := pageBackground(theme)
	splashMu.Lock()
	if i := b2i(dark); splashBrush[i] == 0 {
		splashBrush[i], _, _ = procCreateSolidBrush.Call(colorref)
	}
	splashDark = dark
	splashMu.Unlock()
	os.Setenv("WEBVIEW2_DEFAULT_BACKGROUND_COLOR", argb)
	hook, _, _ := procSetWindowsHookEx.Call(whCBT, splashProc(), 0, uintptr(windows.GetCurrentThreadId()))
	return func() {
		if hook != 0 {
			procUnhookWindowsHookEx.Call(hook)
		}
		os.Unsetenv("WEBVIEW2_DEFAULT_BACKGROUND_COLOR") // for this window only, not the programs Open starts
	}
}

func splashCBT(code, wp, lp uintptr) uintptr {
	on := int32(1)
	switch {
	case code == hcbtActivate && className(wp) == "webview":
		// as it's shown, before it's drawn (Windows doesn't take it any
		// earlier: at create it doesn't know the window yet)
		procDwmSetWindowAttribute.Call(wp, dwmwaCloak, uintptr(unsafe.Pointer(&on)), 4)
		splashMu.Lock()
		dark := splashDark
		splashMu.Unlock()
		if dark { // the window's thin frame too
			procDwmSetWindowAttribute.Call(wp, dwmwaUseImmersiveDarkMode, uintptr(unsafe.Pointer(&on)), 4)
		}
	case code == hcbtCreateWnd && className(wp) == "webview":
		splashMu.Lock()
		brush := splashBrush[b2i(splashDark)]
		splashMu.Unlock()
		if brush != 0 { // Windows paints it in the page's colour, not white
			procSetClassLongPtr.Call(wp, gclpHbrBackground, brush)
		}
	}
	r, _, _ := procCallNextHookEx.Call(0, code, wp, lp)
	return r
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func className(hwnd uintptr) string {
	var buf [64]uint16
	n, _, _ := procGetClassName.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf[:n])
}

// uncloak shows a window startCloaked hid, in front: Windows doesn't bring
// a cloaked window to the front when it opens.
func uncloak(hwnd uintptr) {
	off := int32(0)
	procDwmSetWindowAttribute.Call(hwnd, dwmwaCloak, uintptr(unsafe.Pointer(&off)), 4)
	procSetForegroundWindow.Call(hwnd)
}
