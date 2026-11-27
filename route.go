package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Route owns one route's spool folder and runs its command for each event,
// one at a time, oldest first. An event is written to disk before Lark is
// told it arrived, so a crash or a failing command never loses it:
//
//	spool/<route>/pending/  waiting, or being retried
//	spool/<route>/done/     ran successfully (kept keep_done_days, for replay)
//	spool/<route>/failed/   gave up after the retries; an alert was sent
type Route struct {
	cfg   *RouteCfg
	app   *AppCfg
	gw    *Gateway
	dir   string
	wake  chan struct{}
	catch atomic.Bool // a catch-up run is wanted
	tick  atomic.Bool // a tick run is in progress

	mu   sync.Mutex
	seen map[string]bool // event ids already spooled, so a redelivery is ignored

	// Owned by Work's single event worker, never by Spool or Tick. A terminal
	// command must not run again while its archive operation is being retried.
	finalizing  map[string]*finalization
	spoolErrors map[string]string

	// When the tick next runs. A command may say (see takeReport); until one
	// does, it is every tick.every.
	nextMu  sync.Mutex
	nextAt  time.Time
	reports atomic.Uint64 // how many times a command has said
	resched chan struct{}
}

// backoff between attempts of one event; the last value repeats.
var backoff = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}

func newRoute(gw *Gateway, cfg *RouteCfg) (*Route, error) {
	r := &Route{cfg: cfg, app: gw.cfg.app(cfg.App), gw: gw,
		dir: filepath.Join(gw.cfg.Gateway.SpoolDir, cfg.Name), wake: make(chan struct{}, 1), seen: map[string]bool{},
		resched: make(chan struct{}, 1)}
	for _, sub := range []string{"pending", "done", "failed"} {
		if err := os.MkdirAll(filepath.Join(r.dir, sub), 0o700); err != nil {
			return nil, err
		}
		names, _ := filepath.Glob(filepath.Join(r.dir, sub, "*.json"))
		for _, n := range names {
			if id := eventIDFromName(n); id != "" {
				r.seen[id] = true
			}
		}
	}
	return r, nil
}

func spoolName(id string) string {
	return fmt.Sprintf("%020d-%s.json", time.Now().UnixNano(), sanitize(id))
}

func eventIDFromName(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".json")
	_, id, ok := strings.Cut(base, "-")
	if !ok {
		return ""
	}
	return id
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, s)
}

// Spool stores the event for this route. It returns only once the event is
// safely on disk; an error makes the gateway refuse the event so Lark sends it
// again later.
func (r *Route) Spool(ev *Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen[ev.ID] {
		slog.Info("duplicate event ignored", "route", r.cfg.Name, "event_id", ev.ID)
		return nil
	}
	final := filepath.Join(r.dir, "pending", spoolName(ev.ID))
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(ev.Body); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, final)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	r.seen[ev.ID] = true
	r.poke()
	return nil
}

func (r *Route) poke() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// WantCatchup asks for one catch-up run after the pending events.
func (r *Route) WantCatchup() {
	if len(r.cfg.Catchup) > 0 {
		r.catch.Store(true)
		r.poke()
	}
}

// Work runs until ctx ends: pending events in order, then a catch-up if one
// was asked for, then waits.
func (r *Route) Work(ctx context.Context) {
	for ctx.Err() == nil {
		r.drain(ctx)
		if r.catch.Swap(false) {
			if err := r.run(ctx, r.cfg.Catchup, nil, "catchup", ""); err != nil {
				r.gw.health.routeFailed(r.cfg.Name, "catchup: "+err.Error())
				r.gw.alert(fmt.Sprintf("Route %s: catch-up failed: %v", r.cfg.Name, err), "catchup:"+r.cfg.Name)
			} else {
				r.gw.health.routeOK(r.cfg.Name)
			}
		}
		r.gw.health.setPending(r.cfg.Name, r.pendingCount())
		select {
		case <-ctx.Done():
		case <-r.wake:
		case <-time.After(time.Minute):
		}
	}
}

func (r *Route) pendingCount() int {
	n, _ := filepath.Glob(filepath.Join(r.dir, "pending", "*.json"))
	return len(n)
}

func (r *Route) drain(ctx context.Context) {
	// Visit each file at most once per wake, including new arrivals before
	// catch-up. A completed event awaiting archival cannot spin or block others.
	visited := make(map[string]bool)
	for ctx.Err() == nil {
		names, _ := filepath.Glob(filepath.Join(r.dir, "pending", "*.json"))
		sort.Strings(names) // the name starts with the arrival time
		r.gw.health.setPending(r.cfg.Name, len(names))
		progress := false
		for _, path := range names {
			if ctx.Err() != nil {
				return
			}
			if visited[path] {
				continue
			}
			visited[path], progress = true, true
			r.process(ctx, path)
		}
		if !progress {
			return
		}
	}
}

// process runs one spooled event, retrying with backoff; the route waits
// meanwhile, so events are always handled in the order they arrived.
func (r *Route) process(ctx context.Context, path string) {
	if ctx.Err() != nil || r.resumeFinalization(path) {
		return
	}
	body, err := os.ReadFile(path)
	if err != nil {
		slog.Error("unreadable spool file; awaiting failed archive", "route", r.cfg.Name, "file", path, "err", err)
		r.finish(path, "failed", "unreadable spool file: "+err.Error())
		return
	}
	id := eventIDFromName(path)
	var lastErr error
	for attempt := 1; attempt <= r.cfg.Retries; attempt++ {
		if lastErr = r.run(ctx, r.cfg.Command, body, "event", id); lastErr == nil {
			r.finish(path, "done", "")
			return
		}
		if ctx.Err() != nil {
			return // shutting down: the event stays pending for next start
		}
		if attempt < r.cfg.Retries {
			wait := backoff[min(attempt-1, len(backoff)-1)]
			slog.Warn("event failed; will retry", "route", r.cfg.Name, "event_id", id, "attempt", attempt, "retry_in", wait.String(), "err", lastErr)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}
	r.finish(path, "failed", lastErr.Error())
	msg := fmt.Sprintf("Route %s: event %s failed %d times; no more command retries (archive target spool\\%s\\failed). Last error: %v",
		r.cfg.Name, id, r.cfg.Retries, r.cfg.Name, lastErr)
	r.gw.health.routeFailed(r.cfg.Name, lastErr.Error())
	r.gw.alert(msg, "failed:"+r.cfg.Name)
}

func (r *Route) move(path, to string) error {
	dest := filepath.Join(r.dir, to, filepath.Base(path))
	return os.Rename(path, dest)
}

// Tick runs the route's tick command when it is next due: every tick.every,
// unless a command has said when it next needs one (takeReport). A beat is
// skipped while the previous tick is still going. It runs beside events, not
// between them: a tick may start work that takes hours.
func (r *Route) Tick(ctx context.Context) {
	r.setNext(time.Now().Add(r.cfg.every))
	for {
		r.nextMu.Lock()
		at := r.nextAt
		r.nextMu.Unlock()
		timer := time.NewTimer(time.Until(at))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-r.resched:
			timer.Stop()
			continue
		case <-timer.C:
		}
		// Look again in tick.every unless the run below, or another command
		// meanwhile, says otherwise. While a tick is still going this only
		// wakes the loop; no command starts.
		r.setNext(time.Now().Add(r.cfg.every))
		if !r.tick.CompareAndSwap(false, true) {
			continue
		}
		go func() {
			defer r.tick.Store(false)
			said := r.reports.Load()
			if err := r.run(ctx, r.cfg.Tick.Command, nil, "tick", ""); err != nil {
				r.gw.health.routeFailed(r.cfg.Name, "tick: "+err.Error())
				// A tick has no retries and no failed folder: without this, a
				// scheduled job could fail every run and only the health page
				// would know. alert() sends at most one per key every 30 minutes.
				r.gw.alert(fmt.Sprintf("Route %s: scheduled run failed: %v", r.cfg.Name, err), "tick:"+r.cfg.Name)
			}
			if r.reports.Load() == said {
				r.setNext(time.Now().Add(r.cfg.every)) // a long tick: count tick.every from its end
			}
		}()
	}
}

func (r *Route) setNext(at time.Time) {
	r.nextMu.Lock()
	r.nextAt = at
	r.nextMu.Unlock()
	r.gw.health.setNextTick(r.cfg.Name, at)
	select {
	case r.resched <- struct{}{}:
	default:
	}
}

// takeReport reads what a command wrote to GATEWAY_NEXT_RUN_FILE: when the
// route's tick is next needed (RFC 3339), or "none" (nothing until an event).
// The time is kept between tick.every and tick.max_wait from now: a command
// saying "now" over and over can't make the tick run faster than it used to,
// and "none" still looks every max_wait, in case something changed that no
// event reports (a file edited by hand). A command that writes nothing
// leaves the schedule as it was.
func (r *Route) takeReport(path, reason string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	os.Remove(path)
	now, said := time.Now(), strings.TrimSpace(string(b))
	at := now.Add(r.cfg.maxWait)
	if said != "none" {
		t, err := time.Parse(time.RFC3339, said)
		if err != nil {
			slog.Warn("next run time not understood; ignored", "route", r.cfg.Name, "run", reason, "said", said)
			return
		}
		if t.Before(at) {
			at = t
		}
		if soonest := now.Add(r.cfg.every); at.Before(soonest) {
			at = soonest
		}
	}
	slog.Info("next tick", "route", r.cfg.Name, "at", at.Format(time.RFC3339), "said", said, "by", reason)
	r.reports.Add(1)
	r.setNext(at)
}

// PruneDone deletes done files older than keep_done_days.
func (r *Route) PruneDone(keep time.Duration) {
	names, _ := filepath.Glob(filepath.Join(r.dir, "done", "*.json"))
	cutoff := time.Now().Add(-keep)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range names {
		if st, err := os.Stat(n); err == nil && st.ModTime().Before(cutoff) {
			if os.Remove(n) == nil {
				delete(r.seen, eventIDFromName(n))
			}
		}
	}
}

// run starts a command with the event on stdin and only the environment the
// route allows, and logs what it prints.
func (r *Route) run(ctx context.Context, argv []string, stdin []byte, reason, eventID string) error {
	timeout := r.cfg.timeout
	if reason == "tick" {
		timeout = 0 // a tick may legitimately run for hours
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = r.cfg.Workdir
	cmd.Env = r.env(reason, eventID)
	if r.cfg.every > 0 {
		// Any run of a ticking route may say when its tick is next needed.
		report := filepath.Join(r.dir, fmt.Sprintf("next-run-%d-%s.txt", time.Now().UnixNano(), reason))
		cmd.Env = append(cmd.Env, "GATEWAY_NEXT_RUN_FILE="+report)
		defer r.takeReport(report, reason)
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	start := time.Now()
	out, err := cmd.CombinedOutput()
	sc := bufio.NewScanner(bytes.NewReader(out))
	lines := 0
	for sc.Scan() {
		if line := strings.TrimRight(sc.Text(), "\r"); strings.TrimSpace(line) != "" {
			if lines++; lines <= 200 {
				slog.Info(line, "route", r.cfg.Name, "run", reason, "event_id", eventID)
			}
		}
	}
	took := time.Since(start).Round(time.Millisecond).String()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			err = fmt.Errorf("timed out after %s", timeout)
		}
		slog.Warn("command failed", "route", r.cfg.Name, "run", reason, "event_id", eventID, "took", took, "err", err)
		return err
	}
	slog.Info("command ok", "route", r.cfg.Name, "run", reason, "event_id", eventID, "took", took)
	return nil
}

// baseEnv is what every command gets from the gateway's own environment:
// enough for Windows and Python to work, nothing else.
var baseEnv = []string{"SystemRoot", "SystemDrive", "windir", "PATH", "PATHEXT", "TEMP", "TMP",
	"USERPROFILE", "APPDATA", "LOCALAPPDATA", "ProgramData", "ProgramFiles", "ProgramFiles(x86)",
	"COMPUTERNAME", "HOMEDRIVE", "HOMEPATH", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE"}

func (r *Route) env(reason, eventID string) []string {
	var env []string
	for _, k := range baseEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	env = append(env, "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1",
		"GATEWAY_ROUTE="+r.cfg.Name, "GATEWAY_APP="+r.cfg.App, "GATEWAY_REASON="+reason,
		"GATEWAY_EVENT_TYPE="+r.cfg.Event, "GATEWAY_EVENT_ID="+eventID, "GATEWAY_LARK_DOMAIN="+r.app.Domain)
	for _, k := range r.cfg.Env {
		env = append(env, k+"="+r.gw.cfg.secret(k))
	}
	if r.cfg.PassCred {
		env = append(env, "GATEWAY_LARK_APP_ID="+r.app.AppID, "GATEWAY_LARK_APP_SECRET="+r.gw.cfg.secret(r.app.SecretEnv))
	}
	return env
}
