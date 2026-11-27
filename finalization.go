package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// A receipt says the command is terminal; only its archive operation may be
// retried. It is separate from the event file because a Windows reader can
// permit reading that event while preventing its rename or replacement.
// This is NOT a transaction with the consumer: a crash after an external
// effect but before this receipt is durable still needs consumer idempotency.
type finalization struct {
	Version   int    `json:"version"`
	To        string `json:"to"`
	Failure   string `json:"failure,omitempty"`
	persisted bool
	retryAt   time.Time
}

const archiveRetry = time.Minute

func receiptPath(path string) string { return path + ".finalized" }

// resumeFinalization returns true when this event must not execute. Corrupt or
// unreadable receipts fail closed, including an interrupted receipt publication.
func (r *Route) resumeFinalization(path string) bool {
	if state := r.finalizing[path]; state != nil {
		r.retryFinalization(path, state)
		return true
	}
	b, err := os.ReadFile(receiptPath(path))
	persisted := err == nil
	if os.IsNotExist(err) {
		b, err = os.ReadFile(receiptPath(path) + ".tmp")
	}
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		r.spoolError(path, fmt.Errorf("cannot read completion receipt; command held: %w", err))
		return true
	}
	state := &finalization{}
	if err := json.Unmarshal(b, state); err != nil {
		r.spoolError(path, fmt.Errorf("invalid completion receipt; command held: %w", err))
		return true
	}
	if state.Version != 1 || (state.To != "done" && state.To != "failed") {
		r.spoolError(path, fmt.Errorf("unsupported completion receipt; command held"))
		return true
	}
	state.persisted = persisted
	r.rememberFinalization(path, state)
	if state.To == "failed" {
		r.gw.health.routeFailed(r.cfg.Name, state.Failure)
	}
	r.retryFinalization(path, state)
	return true
}

func (r *Route) rememberFinalization(path string, state *finalization) {
	if r.finalizing == nil {
		r.finalizing = make(map[string]*finalization)
	}
	r.finalizing[path] = state
}

func (r *Route) finish(path, to, failure string) {
	state := &finalization{Version: 1, To: to, Failure: failure}
	// Remember BEFORE any filesystem action, even if the disk cannot accept
	// either a receipt or an archive. Never replay a known terminal command.
	r.rememberFinalization(path, state)
	if to == "failed" {
		r.gw.health.routeFailed(r.cfg.Name, failure)
	}
	r.retryFinalization(path, state)
}

func writeReceipt(path string, state *finalization) error {
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp := receiptPath(path) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	// Keep an incomplete temp receipt on error. On restart it holds the
	// command rather than silently treating a partial receipt as no receipt.
	if err != nil {
		return err
	}
	return os.Rename(tmp, receiptPath(path))
}

func (r *Route) retryFinalization(path string, state *finalization) {
	if time.Now().Before(state.retryAt) {
		return
	}
	state.retryAt = time.Now().Add(archiveRetry)
	var receiptErr error
	if !state.persisted {
		receiptErr = writeReceipt(path, state)
		state.persisted = receiptErr == nil
	}
	if err := r.move(path, state.To); err != nil {
		if receiptErr != nil {
			r.spoolError(path, fmt.Errorf("archive failed: %v; completion receipt unavailable: %v; command suppressed in this process, restart may replay it", err, receiptErr))
		} else {
			r.spoolError(path, fmt.Errorf("archive to %s failed; command completed, retrying only archive: %w", state.To, err))
		}
		return
	}
	// The archived event itself now preserves incoming-event deduplication on
	// restart. Never delete that event as an alternative to a failed rename.
	for _, receipt := range []string{receiptPath(path), receiptPath(path) + ".tmp"} {
		if err := os.Remove(receipt); err != nil && !os.IsNotExist(err) {
			slog.Warn("could not remove archived event's receipt; inspect before manual replay", "file", receipt, "err", err)
		}
	}
	delete(r.finalizing, path)
	delete(r.spoolErrors, path)
	r.updateSpoolHealth()
	if state.To == "done" {
		r.gw.health.routeOK(r.cfg.Name)
	}
}

func (r *Route) spoolError(path string, err error) {
	if r.spoolErrors == nil {
		r.spoolErrors = make(map[string]string)
	}
	r.spoolErrors[path] = err.Error()
	r.updateSpoolHealth()
	r.gw.health.routeFailed(r.cfg.Name, err.Error())
	slog.Error("spool finalization blocked", "route", r.cfg.Name, "file", path, "err", err)
	r.gw.alert(fmt.Sprintf("Route %s: spool finalization blocked for %s: %v", r.cfg.Name, eventIDFromName(path), err), "spool:"+r.cfg.Name)
}

func (r *Route) updateSpoolHealth() {
	var message string
	for _, err := range r.spoolErrors {
		message = err
		break
	}
	r.gw.health.spoolState(r.cfg.Name, len(r.spoolErrors), message)
}
