// Copyright (C) 2026 Handover contributors
// SPDX-License-Identifier: AGPL-3.0-only
package adapter

import (
	"testing"
	"time"
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
