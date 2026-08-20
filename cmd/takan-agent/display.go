package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const maxDisplayHTML = 1 << 20 // 1 MiB, matches hub/MCP cap

const kioskPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width,initial-scale=1"/>
<title>Takan display</title>
<style>
html,body,#f{margin:0;height:100%;background:#14171a}
iframe{border:0;width:100%;height:100%;display:block}
</style>
</head>
<body>
<iframe id="f" src="/content" title="content"></iframe>
<script>
const f=document.getElementById('f');
function reload(){ f.src='/content?t='+Date.now(); }
try {
  const es=new EventSource('/events');
  es.addEventListener('update', reload);
} catch (e) {}
</script>
</body>
</html>`

const idlePage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width,initial-scale=1"/>
<title>Takan</title>
<style>
html,body{margin:0;height:100%;background:#14171a;color:#8b949e;
  display:grid;place-items:center;font:500 15px/1.45 system-ui,sans-serif}
.mark{font-weight:650;letter-spacing:-.02em;color:#eef0f2;font-size:1.1rem}
</style>
</head>
<body><div><div class="mark">Takan</div><div>waiting</div></div></body>
</html>`

type displayServer struct {
	addr string
	dir  string

	mu   sync.RWMutex
	html []byte
	ver  int
	subs map[chan int]struct{}
}

func newDisplayServer(addr, dir string) *displayServer {
	d := &displayServer{addr: addr, dir: dir, html: []byte(idlePage), subs: make(map[chan int]struct{})}
	if dir != "" {
		if b, err := os.ReadFile(filepath.Join(dir, "index.html")); err == nil && len(b) > 0 {
			d.html = b
		}
	}
	return d
}

func (d *displayServer) start() error {
	if d == nil || d.addr == "" {
		return nil
	}
	ln, err := net.Listen("tcp", d.addr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", d.kiosk)
	mux.HandleFunc("GET /content", d.content)
	mux.HandleFunc("GET /events", d.events)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("display listening on http://%s", d.addr)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("display server: %v", err)
		}
	}()
	return nil
}

func (d *displayServer) setHTML(html string) error {
	if len(html) > maxDisplayHTML {
		return fmt.Errorf("html too large (%d bytes, max %d)", len(html), maxDisplayHTML)
	}
	body := []byte(html)
	if len(body) == 0 {
		body = []byte(idlePage)
	}
	if d.dir != "" {
		if err := os.MkdirAll(d.dir, 0o700); err == nil {
			_ = os.WriteFile(filepath.Join(d.dir, "index.html"), body, 0o600)
		}
	}
	d.mu.Lock()
	d.html = body
	d.ver++
	ver := d.ver
	subs := make([]chan int, 0, len(d.subs))
	for ch := range d.subs {
		subs = append(subs, ch)
	}
	d.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- ver:
		default:
		}
	}
	return nil
}

func (d *displayServer) kiosk(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(kioskPage))
}

func (d *displayServer) content(w http.ResponseWriter, _ *http.Request) {
	d.mu.RLock()
	body := d.html
	d.mu.RUnlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

func (d *displayServer) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flush", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ch := make(chan int, 4)
	d.mu.Lock()
	d.subs[ch] = struct{}{}
	ver := d.ver
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.subs, ch)
		d.mu.Unlock()
	}()
	fmt.Fprintf(w, "event: update\ndata: %d\n\n", ver)
	fl.Flush()
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case v := <-ch:
			fmt.Fprintf(w, "event: update\ndata: %d\n\n", v)
			fl.Flush()
		case <-tick.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			fl.Flush()
		}
	}
}

func defaultDisplayDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.TempDir()
	}
	return filepath.Join(home, ".takan", "display")
}
