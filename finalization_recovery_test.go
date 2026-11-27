package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDrainAdvancesPastCompletedArchiveFault(t *testing.T) {
	gw, r, out := newTestGateway(t, "0")
	first := spoolFinalizationEvent(t, r, "first")
	blockFinalization(t, r, first, "done")
	time.Sleep(time.Millisecond)
	spoolFinalizationEvent(t, r, "second")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.drain(ctx)
	if ctx.Err() != nil {
		t.Fatal("drain stalled behind a completed event")
	}
	if finalizationRuns(out, "first") != 1 || finalizationRuns(out, "second") != 1 {
		t.Fatalf("both commands should run once; first=%d second=%d", finalizationRuns(out, "first"), finalizationRuns(out, "second"))
	}
	// A later successful command cannot hide the still-outstanding spool fault.
	h := gw.health.Routes[r.cfg.Name]
	if h.SpoolBlocked != 1 || h.SpoolError == "" {
		t.Fatalf("missing outstanding spool health: %+v", h)
	}
	if _, alerted := gw.alertSent["spool:"+r.cfg.Name]; !alerted {
		t.Fatal("no spool alert")
	}
}

func TestFinalizationSurvivesRestartAndRecoversWithoutReplay(t *testing.T) {
	gw, r, out := newTestGateway(t, "0")
	path := spoolFinalizationEvent(t, r, "restart")
	dest := blockFinalization(t, r, path, "done")
	r.process(context.Background(), path)
	if _, err := os.Stat(receiptPath(path)); err != nil {
		t.Fatalf("durable receipt missing: %v", err)
	}
	restarted, err := newRoute(gw, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	restarted.process(context.Background(), path)
	if finalizationRuns(out, "restart") != 1 {
		t.Fatal("restart reran a completed command")
	}
	if err := os.RemoveAll(dest); err != nil {
		t.Fatal(err)
	} // test-created blocker only
	restarted.finalizing[path].retryAt = time.Time{}
	restarted.process(context.Background(), path)
	if finalizationRuns(out, "restart") != 1 {
		t.Fatal("archive recovery reran the command")
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("event not archived after recovery: %v", err)
	}
	if _, err := os.Stat(receiptPath(path)); !os.IsNotExist(err) {
		t.Fatalf("receipt not cleaned up: %v", err)
	}
	if gw.health.Routes[r.cfg.Name].SpoolBlocked != 0 {
		t.Fatal("resolved spool fault not cleared")
	}
	again, err := newRoute(gw, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ev, _ := parseEvent(event("restart", "oc_bank", "user"))
	if err := again.Spool(ev); err != nil {
		t.Fatal(err)
	}
	if again.pendingCount() != 0 {
		t.Fatal("incoming redelivery was not deduplicated after restart")
	}
}

func TestFailedCommandDoesNotResetRetryBudgetAfterArchiveFault(t *testing.T) {
	gw, r, out := newTestGateway(t, "1")
	r.cfg.Retries = 1
	path := spoolFinalizationEvent(t, r, "exhausted")
	blockFinalization(t, r, path, "failed")
	r.process(context.Background(), path)
	r.process(context.Background(), path)
	restarted, err := newRoute(gw, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	restarted.process(context.Background(), path)
	if n := finalizationRuns(out, "exhausted"); n != 1 {
		t.Fatalf("exhausted command ran %d times; want 1", n)
	}
	if restarted.finalizing[path].To != "failed" {
		t.Fatal("failure outcome not restored")
	}
}

func TestWorkWakeDoesNotReplayCompletedEvent(t *testing.T) {
	_, r, out := newTestGateway(t, "0")
	path := spoolFinalizationEvent(t, r, "wake")
	blockFinalization(t, r, path, "done")
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); r.Work(ctx) }()
	t.Cleanup(func() { cancel(); <-stopped })
	waitFor(t, func() bool { _, err := os.Stat(receiptPath(path)); return err == nil })
	for i := 0; i < 5; i++ {
		r.poke()
		time.Sleep(30 * time.Millisecond)
	}
	cancel()
	<-stopped
	// Cleanup may receive from a closed channel again, which is safe.
	if n := finalizationRuns(out, "wake"); n != 1 {
		t.Fatalf("wakes reran completed command %d times", n)
	}
}

func TestArchiveAttemptsAreThrottledUntilRetryTime(t *testing.T) {
	_, r, out := newTestGateway(t, "0")
	path := spoolFinalizationEvent(t, r, "throttled")
	dest := blockFinalization(t, r, path, "done")
	r.process(context.Background(), path)
	scheduled := r.finalizing[path].retryAt
	if err := os.RemoveAll(dest); err != nil {
		t.Fatal(err)
	}
	r.process(context.Background(), path)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("unexpected archive attempt before retry time")
	}
	if !r.finalizing[path].retryAt.Equal(scheduled) {
		t.Fatal("wake changed backoff deadline")
	}
	r.finalizing[path].retryAt = time.Time{}
	r.process(context.Background(), path)
	if finalizationRuns(out, "throttled") != 1 {
		t.Fatal("move retry executed command")
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidReceiptHoldsCommand(t *testing.T) {
	for name, body := range map[string]string{"empty": "", "partial": "{", "version": "{\"version\":2,\"to\":\"done\"}", "destination": "{\"version\":1,\"to\":\"../outside\"}"} {
		t.Run(name, func(t *testing.T) {
			gw, r, out := newTestGateway(t, "0")
			path := spoolFinalizationEvent(t, r, "invalid")
			if err := os.WriteFile(receiptPath(path), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			r.process(context.Background(), path)
			if finalizationRuns(out, "invalid") != 0 {
				t.Fatal("executed through invalid completion receipt")
			}
			if gw.health.Routes[r.cfg.Name].SpoolBlocked != 1 {
				t.Fatal("invalid receipt not visible in health")
			}
		})
	}
}

func TestUnpublishedReceiptRecoveredWithoutCommand(t *testing.T) {
	_, r, out := newTestGateway(t, "0")
	path := spoolFinalizationEvent(t, r, "unpublished")
	if err := os.WriteFile(receiptPath(path)+".tmp", []byte(`{"version":1,"to":"done"}`), 0600); err != nil {
		t.Fatal(err)
	}
	r.process(context.Background(), path)
	if finalizationRuns(out, "unpublished") != 0 {
		t.Fatal("reran command after interrupted receipt publication")
	}
	if _, err := os.Stat(filepath.Join(r.dir, "done", filepath.Base(path))); err != nil {
		t.Fatal(err)
	}
}

func TestPartialUnpublishedReceiptFailsClosed(t *testing.T) {
	_, r, out := newTestGateway(t, "0")
	path := spoolFinalizationEvent(t, r, "partial")
	if err := os.WriteFile(receiptPath(path)+".tmp", []byte(`{"version":`), 0600); err != nil {
		t.Fatal(err)
	}
	r.process(context.Background(), path)
	if finalizationRuns(out, "partial") != 0 {
		t.Fatal("reran through an ambiguous partial receipt")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("ambiguous event was discarded")
	}
}

func TestMemoryGuardWhenReceiptAndArchiveBothFail(t *testing.T) {
	gw, r, out := newTestGateway(t, "0")
	path := spoolFinalizationEvent(t, r, "memory-only")
	blockFinalization(t, r, path, "done")
	if err := os.Mkdir(receiptPath(path)+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	// finish is called only after a known terminal outcome. Simulate that
	// boundary with both filesystem writes blocked, without a production effect.
	r.finish(path, "done", "")
	r.process(context.Background(), path)
	r.process(context.Background(), path)
	if finalizationRuns(out, "memory-only") != 0 {
		t.Fatal("forgot terminal state when receipt persistence failed")
	}
	if !strings.Contains(gw.health.Routes[r.cfg.Name].SpoolError, "restart may replay") {
		t.Fatal("lost durability was not explicitly reported")
	}
}

func TestCancelledDrainDoesNotExecute(t *testing.T) {
	_, r, out := newTestGateway(t, "0")
	spoolFinalizationEvent(t, r, "cancelled")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.drain(ctx)
	if finalizationRuns(out, "cancelled") != 0 {
		t.Fatal("executed after shutdown")
	}
}
