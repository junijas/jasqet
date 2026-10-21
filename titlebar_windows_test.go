//go:build windows

package main

import (
	"runtime"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestTheWindowFindsItsIcon(t *testing.T) {
	var h windows.Handle
	windows.GetModuleHandleEx(0, nil, &h)
	icon, _, _ := user32.NewProc("LoadImageW").Call(uintptr(h), appIconID, 1, 0, 0, 0x40|0x8000) // IMAGE_ICON, LR_DEFAULTSIZE|LR_SHARED
	if icon == 0 {
		t.Fatalf("no icon number %d in the program: the window and the taskbar would show Windows' blank one", appIconID)
	}
}

func TestOwnTitleBarTakesOnlyTheCaptionAway(t *testing.T) {
	runtime.LockOSThread() // a window answers on the thread that made it
	defer runtime.UnlockOSThread()
	class, _ := windows.UTF16PtrFromString("STATIC")
	hwnd, _, _ := user32.NewProc("CreateWindowExW").Call(0, uintptr(unsafe.Pointer(class)), 0, wsOverlappedWindow, 100, 100, 800, 600, 0, 0, 0, 0)
	if hwnd == 0 {
		t.Skip("can't make a window here")
	}
	defer user32.NewProc("DestroyWindow").Call(hwnd) // never shown
	// where the window's inside starts, against its outer edge
	inset := func() (top, left int32) {
		var w winRect
		var p struct{ X, Y int32 }
		user32.NewProc("GetWindowRect").Call(hwnd, uintptr(unsafe.Pointer(&w)))
		user32.NewProc("ClientToScreen").Call(hwnd, uintptr(unsafe.Pointer(&p)))
		return p.Y - w.Top, p.X - w.Left
	}
	top, left := inset()
	if top < 20 || left <= 0 {
		t.Fatalf("a normal window first: caption %d, side %d", top, left)
	}
	takeCaption(hwnd, nil)
	if top2, left2 := inset(); top2 != 0 || left2 != left {
		t.Fatalf("with Jasqet's title bar: inside starts %d px down (want 0), side %d (want %d, Windows' own resizing)", top2, left2, left)
	}
	// full screen in the viewer takes the frame off: then nothing is changed
	setStyle(hwnd, getStyle(hwnd)&^wsOverlappedWindow)
	procSetWindowPos.Call(hwnd, 0, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoZOrder|swpNoOwnerZOrder|swpFrameChanged)
	if top3, left3 := inset(); top3 != 0 || left3 != 0 {
		t.Fatalf("frameless: inside starts at %d,%d", left3, top3)
	}
}
