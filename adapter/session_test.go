// Copyright (C) 2026 Handover contributors
// SPDX-License-Identifier: AGPL-3.0-only
package adapter

import (
	"os"
	"testing"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
)

func TestWallClockJumpedOnlyAfterSuspendSizedGap(t *testing.T) {
	previous := time.Unix(1_700_000_000, 0)
	if wallClockJumped(previous, previous.Add(resumeCheckInterval)) {
		t.Fatal("normal timer interval must not look like resume")
	}
	if !wallClockJumped(previous, previous.Add(10*time.Minute)) {
		t.Fatal("large wall-clock jump must trigger resume recovery")
	}
}

func TestExpiredPendingCorrelationIsDropped(t *testing.T) {
	var got []Event
	sess := &Session{account: "a", emit: func(e Event) { got = append(got, e) }}
	sess.pending = map[string]pendingSend{
		"old": {requestID: "r", convID: "c", at: time.Now().Add(-pendingTTL - time.Minute)},
		"new": {requestID: "r", convID: "c", at: time.Now()},
	}
	sess.sweepPending()
	if _, ok := sess.pending["old"]; ok {
		t.Error("expired correlation must be swept")
	}
	if _, ok := sess.pending["new"]; !ok {
		t.Error("fresh correlation must survive the sweep")
	}
}

func TestClosedSessionFiresNothing(t *testing.T) {
	var got []Event
	sess := &Session{account: "a", emit: func(e Event) { got = append(got, e) }}
	sess.closed = make(chan struct{})
	sess.fire(Event{Type: "error", Account: "a", Message: "live"})
	if len(got) != 1 {
		t.Fatalf("open session must emit, got %d events", len(got))
	}
	close(sess.closed)
	sess.fire(Event{Type: "error", Account: "a", Message: "late"})
	if len(got) != 1 {
		t.Error("closed session must drop late events")
	}
}

func TestEmissionCounterIndependentOfLifecycle(t *testing.T) {
	store := &Store{dir: t.TempDir()}
	if err := store.ensureDir(); err != nil {
		t.Fatal(err)
	}
	sess := &Session{account: "test", store: store, closed: make(chan struct{})}
	lifecycle := sess.currentLifecycle()
	// Routine sync emissions must not retire the login lifecycle:
	// queued events stamped earlier and pending saves must survive.
	sess.nextGeneration()
	sess.nextGeneration()
	if got := sess.currentLifecycle(); got != lifecycle {
		t.Fatalf("emission bumped lifecycle: %d -> %d", lifecycle, got)
	}
	sess.auth = libgm.NewAuthData()
	if err := sess.saveAuthIfCurrent(lifecycle); err != nil {
		t.Fatalf("current-lifecycle save refused: %v", err)
	}
	path, ok := store.accountFile("test")
	if !ok {
		t.Fatal("test account must pass the storage gate")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("current-lifecycle save must persist, got %v", err)
	}
	// A stale lifecycle still saves nothing.
	if err := sess.saveAuthIfCurrent(lifecycle + 1000); err != nil {
		t.Fatalf("stale save must be a silent skip, got %v", err)
	}
	// Close retires the lifecycle so post-close backlogs drop.
	sess.Close()
	if got := sess.currentLifecycle(); got == lifecycle {
		t.Fatal("close must retire the lifecycle")
	}
	if err := sess.saveAuthIfCurrent(lifecycle); err != nil {
		t.Fatalf("post-close save must be a silent skip, got %v", err)
	}
}
