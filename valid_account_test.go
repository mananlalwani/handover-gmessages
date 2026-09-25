// Copyright (C) 2026 Handover contributors
// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"strings"
	"testing"
)

func TestValidAccountRejectsUnsafeNames(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "a.b", "a\x00b", "a\nb", strings.Repeat("x", 129)} {
		if validAccount(bad) {
			t.Errorf("account %q must be rejected", bad)
		}
	}
	for _, good := range []string{"gmessages:default", "loopback", "acc-1"} {
		if !validAccount(good) {
			t.Errorf("account %q must be accepted", good)
		}
	}
}
