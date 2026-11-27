# Spool Finalization Replay Implementation Plan

**Goal:** A successful or exhausted event must not execute again merely because its final archive move fails. Preserve event contents and incoming-event deduplication.

**Architecture:** Keep a terminal outcome in the route worker, and write a synced, atomically published sidecar receipt beside the pending event before archiving. A future worker/restart reads that receipt and retries only finalization. Process each pending snapshot once, allowing later events to proceed. Throttle archive retries per event and report spool failures instead of reporting a failed archive as healthy.

**Tech Stack:** Go 1.27, standard-library filesystem operations, existing helper-process tests; Windows sharing-lock regression.

## Task 1: Reproduce before changing production code

- Create route_finalization_test.go using newTestGateway and its no-op helper process.
- Initialize normal spool directories and create a conflicting destination directory after Spool.
- Call process twice and assert one consumer execution and no LastOK update.
- Run: go test -run TestArchiveFailureDoesNotReplayCompletedEvent -count=1 .
- Expected before fix: FAIL because two consumer executions and a false healthy timestamp occur.

## Task 2: Implement a narrow finalization state machine

- Modify route.go: add worker-owned pending finalization state, load a receipt before any command execution, retain exhausted-failure outcomes too, and make move return errors.
- Create finalization.go: typed receipt, temporary receipt + Sync + atomic rename, validation/recovery of interrupted publication, in-memory fallback, one archive attempt per minute, health/alert reporting, cleanup only after successful archival. An in-memory retry rewrites only its own temporary receipt; an invalid receipt after restart holds for reconciliation.
- Change drain to visit each file at most once per wake, rescanning for new arrivals before catch-up. Do not re-select an unmovable file in a hot loop.
- Preserve command retry behavior and ordering for events that have not completed. Do not remove event files to work around sharing violations.
- Do not claim exactly-once effects. A crash between an external effect and a durable receipt, or loss of all receipt persistence, still requires consumer idempotency.

## Task 3: Regression coverage

- Repeated process calls and Work wakes cannot rerun terminal commands.
- A second event progresses despite the first event's archive fault.
- A newRoute after a persisted receipt retries only the move; after fault removal it archives without rerunning the command and retains redelivery deduplication.
- Exhausted failed commands retain the retry cap across archive failure/restart.
- Malformed/unreadable receipts fail closed; receipt write failure plus archive failure preserves in-process suppression and exposes the durability risk.
- Windows-specific test holds FILE_SHARE_READ without FILE_SHARE_DELETE after normal initialization, then verifies release/recovery.
- Cancellation and normal archival still work; no live gateway or business endpoint is invoked.

## Task 4: Validate and document

- Run gofmt, full go test ./..., repeated targeted tests, go vet ./..., and go test -race ./... when the local C toolchain supports it.
- Read README.md spool/recovery guidance before updating it. Document receipt recovery and the existing ambiguous-outcome crash window, and manual replay precautions.
- Review the diff for behavior changes outside the bug. Keep all work in fix/spool-finalization-replay; do not merge, deploy or alter server tasks.
- Deliver a tested patch and implementation notes. A remote PR is separate from deployment.
