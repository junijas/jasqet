package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func main() {
	port := flag.String("port", "9614", "port for the app's local page")
	noBrowser := flag.Bool("no-browser", false, "start in the tray without opening the window")
	quitOther := flag.Bool("quit", false, "close a running Jasqet (used by the installer)")
	connect := flag.Bool("connect-browser", false, "open the 'connect your browser' guide")
	viewer := flag.String("viewer", "", "show the built-in viewer window (used by Jasqet itself)")
	flag.Parse()
	if *viewer != "" {
		runViewer(*viewer) // its own process: a file that crashes it can't stop downloads
		return
	}
	proto := "" // a jasqet:// link (the buttons on Jasqet's notifications)
	if a := flag.Arg(0); strings.HasPrefix(strings.ToLower(a), "jasqet://") {
		proto = a
	}

	addr := "127.0.0.1:" + *port
	appURL := "http://" + addr + "/"

	c := http.Client{Timeout: 3 * time.Second}
	stateDir := filepath.Dir(stateFilePath())
	cmdKey := readCommandKey(stateDir, *port) // what a running Jasqet lets another copy do (apikey.go)
	if *quitOther {
		if resp, err := postLocal(&c, appURL+"api/quit", cmdKey, "{}"); err == nil {
			resp.Body.Close()
			// wait (up to 10 s) until it has saved and closed: the installer replaces its files next
			for start := time.Now(); time.Since(start) < 10*time.Second; time.Sleep(100 * time.Millisecond) {
				r, err := c.Get(appURL + "api/ping")
				if err != nil {
					break
				}
				r.Body.Close()
			}
		}
		return
	}
	page := appURL
	if *connect {
		page = appURL + "#connect"
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if proto != "" {
			b, _ := json.Marshal(map[string]string{"url": proto})
			if resp, perr := postLocal(&c, appURL+"api/protocol", cmdKey, string(b)); perr == nil {
				resp.Body.Close()
				return
			}
		}
		// Already running? Ask it to bring its window to the front.
		body := `{}`
		if *connect {
			body = `{"page":"connect"}`
		}
		if resp, perr := postLocal(&c, appURL+"api/show", cmdKey, body); perr == nil {
			resp.Body.Close()
			return
		}
		log.Fatalf("can't start on %s: %v", addr, err)
	}

	// Jasqet was called TurboGet: its list and settings come along (only
	// now, when no other copy is running and holding them)
	if err := moveOldState(oldStateDir(), stateDir); err != nil {
		log.Printf("moving TurboGet's settings to Jasqet's folder: %v", err)
	}
	m := NewManager(stateFilePath())
	s := &Server{m: m, port: *port, quit: make(chan struct{}), hub: newViewerHub(), key: newAPIKey()}
	if err := s.key.saveCommandKey(stateDir, *port); err != nil {
		log.Printf("saving the key for other copies of Jasqet: %v", err)
	}
	m.autoOpen = s.autoOpen
	srv := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	go m.rebuildLibrary()
	go m.startExtras()
	go registerProtocol()
	go sweepCookieFiles(10 * time.Minute) // left by a crash: no yt-dlp of this run uses them yet

	fmt.Printf("Jasqet %s running at %s\n", version, appURL)
	var once sync.Once
	quit := func() { once.Do(func() { close(s.quit) }) }
	s.desk = startDesktop(page, m, quit, !*noBrowser && proto == "", s.key)
	if proto != "" {
		go s.handleProtocol(proto)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	select {
	case <-sig:
	case <-s.quit:
	}
	m.closing.Store(true) // flush every download now, so no progress is downloaded twice
	m.save()              // in-progress downloads continue next time the app starts
}
