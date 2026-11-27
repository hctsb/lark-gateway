package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func spoolFinalizationEvent(t *testing.T, r *Route, id string) string {
	t.Helper()
	ev, err := parseEvent(event(id, "oc_bank", "user"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Spool(ev); err != nil {
		t.Fatal(err)
	}
	names, err := filepath.Glob(filepath.Join(r.dir, "pending", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range names {
		if eventIDFromName(path) == id {
			return path
		}
	}
	t.Fatal("spooled event not found")
	return ""
}

// A post-startup destination conflict forces a rename failure without violating
// newRoute's normal directory-creation preconditions.
func blockFinalization(t *testing.T, r *Route, path, to string) string {
	t.Helper()
	dest := filepath.Join(r.dir, to, filepath.Base(path))
	if err := os.Mkdir(dest, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "blocker"), []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	return dest
}

func finalizationRuns(out, id string) int {
	b, _ := os.ReadFile(out)
	return strings.Count(string(b), "event|"+id+"|")
}

func TestArchiveFailureDoesNotReplayCompletedEvent(t *testing.T) {
	gw, r, out := newTestGateway(t, "0")
	r.cfg.Retries = 1
	path := spoolFinalizationEvent(t, r, "archive-locked")
	blockFinalization(t, r, path, "done")
	r.process(context.Background(), path)
	r.process(context.Background(), path) // another wake of the same pending event
	if n := finalizationRuns(out, "archive-locked"); n != 1 {
		t.Errorf("completed command executed %d times; want 1", n)
	}
	if !gw.health.Routes[r.cfg.Name].LastOK.IsZero() {
		t.Error("failed archival must not be reported as healthy")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("event must be retained: %v", err)
	}
}
