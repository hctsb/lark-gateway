package main

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestWindowsSharingLockDoesNotReplayCompletedEvent(t *testing.T) {
	gw, r, out := newTestGateway(t, "0")
	r.cfg.Retries = 1
	path := spoolFinalizationEvent(t, r, "windows-locked")
	ptr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := syscall.CreateFile(ptr, syscall.GENERIC_READ, syscall.FILE_SHARE_READ, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			syscall.CloseHandle(handle)
		}
	}()
	if _, err := os.ReadFile(path); err != nil {
		t.Fatal("lock must still allow event reads", err)
	}
	r.process(context.Background(), path)
	r.process(context.Background(), path)
	restarted, err := newRoute(gw, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	restarted.process(context.Background(), path)
	if n := finalizationRuns(out, "windows-locked"); n != 1 {
		t.Fatalf("real Windows lock caused %d executions; want 1", n)
	}
	if !gw.health.Routes[r.cfg.Name].LastOK.IsZero() {
		t.Fatal("locked archive marked healthy")
	}
	if err := syscall.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	closed = true
	restarted.finalizing[path].retryAt = time.Time{}
	restarted.process(context.Background(), path)
	if finalizationRuns(out, "windows-locked") != 1 {
		t.Fatal("unlock recovery reran command")
	}
	if _, err := os.Stat(filepath.Join(r.dir, "done", filepath.Base(path))); err != nil {
		t.Fatal(err)
	}
	if gw.health.Routes[r.cfg.Name].SpoolBlocked != 0 {
		t.Fatal("lock recovery did not clear spool fault")
	}
}
