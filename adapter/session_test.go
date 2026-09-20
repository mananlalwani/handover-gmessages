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
