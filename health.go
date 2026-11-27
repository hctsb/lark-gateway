package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Health is what /health and state\health.json show: is each app connected,
// when did each route last succeed or fail, and how much is waiting.
type Health struct {
	mu      sync.Mutex
	Started time.Time               `json:"started"`
	Apps    map[string]*AppHealth   `json:"apps"`
	Routes  map[string]*RouteHealth `json:"routes"`
}

type AppHealth struct {
	Connected   bool            `json:"connected"`
	Since       time.Time       `json:"since"` // when Connected last changed
	LastEvent   time.Time       `json:"last_event,omitempty"`
	LastError   string          `json:"last_error,omitempty"`
	Subscribed  map[string]bool `json:"subscribed,omitempty"` // Base token -> Lark has agreed to send its changes
	alertedDown bool
}

type RouteHealth struct {
	Pending  int       `json:"pending"`
	LastOK   time.Time `json:"last_ok,omitempty"`
	LastFail time.Time `json:"last_fail,omitempty"`
	LastErr  string    `json:"last_error,omitempty"`
	NextTick time.Time `json:"next_tick,omitempty"`
	// Outstanding spool faults remain visible even when another event or tick succeeds.
	SpoolBlocked int    `json:"spool_blocked,omitempty"`
	SpoolError   string `json:"spool_error,omitempty"`
}

func newHealth(cfg *Config) *Health {
	h := &Health{Started: time.Now(), Apps: map[string]*AppHealth{}, Routes: map[string]*RouteHealth{}}
	for _, a := range cfg.Apps {
		h.Apps[a.Name] = &AppHealth{Since: time.Now()}
	}
	for _, r := range cfg.Routes {
		h.Routes[r.Name] = &RouteHealth{}
	}
	return h
}

func (h *Health) connected(app string, up bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.Apps[app]
	if a.Connected != up {
		a.Connected, a.Since = up, time.Now()
	}
	if up {
		a.alertedDown = false
	}
}

func (h *Health) subscribed(app, base string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.Apps[app]
	if a.Subscribed == nil {
		a.Subscribed = map[string]bool{}
	}
	a.Subscribed[base] = ok
}

func (h *Health) appError(app, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Apps[app].LastError = msg
}

func (h *Health) event(app string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Apps[app].LastEvent = time.Now()
}

func (h *Health) routeOK(route string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Routes[route].LastOK = time.Now()
}

func (h *Health) routeFailed(route, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.Routes[route]
	r.LastFail, r.LastErr = time.Now(), msg
}

func (h *Health) setNextTick(route string, at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Routes[route].NextTick = at
}

func (h *Health) spoolState(route string, blocked int, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Routes[route].SpoolBlocked, h.Routes[route].SpoolError = blocked, msg
}

func (h *Health) setPending(route string, n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Routes[route].Pending = n
}

// downTooLong returns the apps that have been disconnected for longer than
// limit and have not been alerted about yet, and marks them alerted.
func (h *Health) downTooLong(limit time.Duration) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for name, a := range h.Apps {
		if !a.Connected && !a.alertedDown && time.Since(a.Since) > limit {
			a.alertedDown = true
			out = append(out, name)
		}
	}
	return out
}

func (h *Health) snapshot() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	b, _ := json.MarshalIndent(h, "", "  ")
	return b
}

func (h *Health) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(h.snapshot())
}

// writeFile keeps state\health.json current, for anyone who would rather read
// a file than call the endpoint.
func (h *Health) writeFile(dir string) {
	_ = os.MkdirAll(dir, 0o700)
	tmp := filepath.Join(dir, "health.json.tmp")
	if os.WriteFile(tmp, h.snapshot(), 0o600) == nil {
		_ = os.Rename(tmp, filepath.Join(dir, "health.json"))
	}
}
