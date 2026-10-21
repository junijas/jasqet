//go:build windows

package main

// The desktop side on Windows: Jasqet's own window (Microsoft's WebView2,
// built into Windows 10/11) with its own taskbar icon, and a tray icon by the
// clock showing live speed. Closing the window keeps Jasqet running in the
// tray. If WebView2 isn't available, it falls back to an Edge app window.

import (
	_ "embed"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"fyne.io/systray"
	"github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

//go:embed icon.ico
var trayIcon []byte

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	procShowWindow          = user32.NewProc("ShowWindow")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procIsIconic            = user32.NewProc("IsIconic")
	procGetSystemMetrics    = user32.NewProc("GetSystemMetrics")
	procAllowSetForeground  = user32.NewProc("AllowSetForegroundWindow")
)

// allowSetForeground lets our viewer window come to the front when you click Open.
func allowSetForeground() { procAllowSetForeground.Call(^uintptr(0)) } // ASFW_ANY

// windowSize fits the window on small laptop screens too.
func windowSize() (uint, uint) {
	sw, _, _ := procGetSystemMetrics.Call(0) // SM_CXSCREEN
	sh, _, _ := procGetSystemMetrics.Call(1) // SM_CYSCREEN
	w, h := uint(1120), uint(740)
	if sw > 200 && uint(sw)-60 < w {
		w = uint(sw) - 60
	}
	if sh > 200 && uint(sh)-100 < h {
		h = uint(sh) - 100
	}
	return w, h
}

type desktop struct {
	url  string
	m    *Manager
	quit func()
	key  *apiKey // the page's key to the API (apikey.go)

	mu       sync.Mutex
	open     bool
	wv       webview2.WebView
	fallback bool
	args     string // extra settings for the window's browser engine
}

func startDesktop(appURL string, m *Manager, quit func(), showWindow bool, key *apiKey) *desktop {
	d := &desktop{url: appURL, m: m, quit: quit, key: key}
	go d.tray()
	if showWindow {
		d.Show()
	}
	return d
}

// Show opens the Jasqet window, or brings it to the front if it's open.
func (d *desktop) Show() {
	d.mu.Lock()
	if d.fallback {
		d.mu.Unlock()
		openBrowser(d.key.browserURL(d.url))
		return
	}
	if d.open {
		wv := d.wv
		d.mu.Unlock()
		if wv != nil {
			wv.Dispatch(func() {
				hwnd := uintptr(wv.Window())
				if r, _, _ := procIsIconic.Call(hwnd); r != 0 {
					procShowWindow.Call(hwnd, 9) // SW_RESTORE
				}
				procShowWindow.Call(hwnd, 5) // SW_SHOW
				procSetForegroundWindow.Call(hwnd)
			})
		}
		return
	}
	d.open = true
	d.mu.Unlock()

	go func() {
		runtime.LockOSThread()
		defer func() {
			if r := recover(); r != nil {
				d.useFallback(fmt.Sprint(r))
			}
			d.mu.Lock()
			d.open, d.wv = false, nil
			d.mu.Unlock()
		}()
		dataDir := filepath.Join(filepath.Dir(stateFilePath()), "WebView2")
		os.MkdirAll(dataDir, 0o755)
		// the built-in viewer shows inside this window: let it pick sound
		// tracks and start playing when you click Open. Fixed for the app's
		// lifetime (the engine refuses different settings for the same folder).
		d.mu.Lock()
		if d.args == "" {
			d.args = "--enable-blink-features=AudioVideoTracks --autoplay-policy=no-user-gesture-required"
			d.m.mu.Lock()
			hw := d.m.settings.Viewer.HWDecode
			d.m.mu.Unlock()
			if hw == "off" {
				d.args += " --disable-accelerated-video-decode"
			}
		}
		os.Setenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS", d.args)
		d.mu.Unlock()
		ww, wh := windowSize()
		w := webview2.NewWithOptions(webview2.WebViewOptions{
			AutoFocus: true,
			DataPath:  dataDir,
			WindowOptions: webview2.WindowOptions{
				Title: "Jasqet", Width: ww, Height: wh, IconId: appIconID, Center: true,
			},
		})
		os.Unsetenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS") // for this window only, not the programs Open starts
		if w == nil {
			d.useFallback("WebView2 is not available")
			return
		}
		d.mu.Lock()
		d.wv = w
		d.mu.Unlock()
		w.SetSize(640, 480, webview2.HintMin)
		ownTitleBar(w) // the app's top bar instead of Windows' caption strip
		// full screen for videos in the viewer, and a watch in case a file
		// ever makes the page stop responding (it reloads, downloads carry on)
		v := &viewerWin{w: w, hwnd: uintptr(w.Window())}
		w.Bind("tgFullscreen", v.fullscreen)
		w.Bind("tgPong", v.pong)
		stop := make(chan struct{})
		go v.watch(stop, func(file string) bool {
			target := d.url
			if file != "" {
				target = strings.Split(d.url, "#")[0] + "#viewcrash=" + url.QueryEscape(file)
			}
			v.expect()
			w.Dispatch(func() {
				v.fullscreen(false)
				w.Navigate(target)
			})
			return true
		})
		w.Init(d.key.windowScript(d.url)) // the page's key, before the page runs
		w.Navigate(d.url)
		w.Run() // returns when the window is closed; downloads carry on
		close(stop)
	}()
}

func (d *desktop) useFallback(why string) {
	d.mu.Lock()
	first := !d.fallback
	d.fallback = true
	d.mu.Unlock()
	if first {
		fmt.Println("native window unavailable:", why, "- using an Edge window instead")
		openBrowser(d.key.browserURL(d.url))
	}
}

func (d *desktop) tray() {
	runtime.LockOSThread()
	defer func() { recover() }() // a tray problem must never stop downloads
	systray.Run(func() {
		systray.SetIcon(trayIcon)
		systray.SetTitle("Jasqet")
		systray.SetTooltip("Jasqet")
		systray.SetOnTapped(d.Show)
		mOpen := systray.AddMenuItem("Open Jasqet", "")
		systray.AddSeparator()
		mPause := systray.AddMenuItem("Pause all", "")
		mResume := systray.AddMenuItem("Resume all", "")
		mSched := systray.AddMenuItemCheckbox("Scheduler on", "Run scheduled downloads at the set times", false)
		mQuiet := systray.AddMenuItemCheckbox("Quiet mode (pause while I browse)", "Pause everything until you untick this", false)
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Quit Jasqet", "")

		go func() {
			t := time.NewTicker(2 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-mOpen.ClickedCh:
					d.Show()
				case <-mPause.ClickedCh:
					d.m.PauseAll()
				case <-mResume.ClickedCh:
					d.m.ResumeAll()
				case <-mSched.ClickedCh:
					d.m.ToggleSchedule()
				case <-mQuiet.ClickedCh:
					d.m.SetQuiet(!d.m.Quiet())
				case <-mQuit.ClickedCh:
					systray.Quit()
					d.quit()
					return
				case <-t.C:
				}
				sum := d.m.Summary()
				tip := "Jasqet: idle"
				if sum.Active > 0 {
					tip = fmt.Sprintf("Jasqet: ↓ %s/s · %d downloading", humanSize(sum.Speed), sum.Active)
				} else if sum.Scheduled > 0 && sum.NextRun != "" {
					tip = fmt.Sprintf("Jasqet: %d scheduled, %s", sum.Scheduled, sum.NextRun)
				}
				if sum.BudgetLeft >= 0 {
					tip += fmt.Sprintf(" · %s data left", humanSize(sum.BudgetLeft))
				}
				systray.SetTooltip(tip)
				if sum.SchedOn {
					mSched.Check()
				} else {
					mSched.Uncheck()
				}
				if sum.Quiet {
					mQuiet.Check()
				} else {
					mQuiet.Uncheck()
				}
			}
		}()
	}, nil)
}
