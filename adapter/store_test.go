// Copyright (C) 2026 Handover contributors
// SPDX-License-Identifier: AGPL-3.0-only
package adapter

import (
	"os"
	"path/filepath"
	"testing"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
)

func testAuth() *libgm.AuthData {
	auth := libgm.NewAuthData()
	auth.TachyonAuthToken = []byte("token-bytes")
	return auth
}

func TestStoreRoundTripWithStrictPermissions(t *testing.T) {
	dir := t.TempDir()
	store := &Store{dir: filepath.Join(dir, "handover", "gmessages")}
	auth := testAuth()
	if err := store.SaveAuth("work", auth); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := store.LoadAuth("work")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded == nil || string(loaded.TachyonAuthToken) != "token-bytes" {
		t.Fatalf("round trip mismatch: %+v", loaded)
	}
	info, err := os.Stat(filepath.Join(store.dir, "work.session.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("session perms = %o, want 600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("dir perms = %o, want 700", dirInfo.Mode().Perm())
	}
	if err := store.DeleteAuth("work"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if loaded, _ := store.LoadAuth("work"); loaded != nil {
		t.Error("deleted session still loads")
	}
	if accounts := store.Accounts(); len(accounts) != 0 {
		t.Errorf("accounts after delete = %v", accounts)
	}
}

func TestStoreRejectsUnsafeAccountNames(t *testing.T) {
	store := &Store{dir: t.TempDir()}
	for _, name := range []string{"", "../escape", "a/b", "a\\b", ".", "a.b", "x\x00y"} {
		if _, ok := store.accountFile(name); ok {
			t.Errorf("account %q must be rejected", name)
		}
		if err := store.SaveAuth(name, testAuth()); err == nil {
			t.Errorf("save %q must fail", name)
		}
	}
}

func TestCursorRoundTripAndForeignRejection(t *testing.T) {
	cursor := mintCursor("m9", 1758000000000000)
	id, ts, err := parseCursor(cursor)
	if err != nil || id != "m9" || ts != 1758000000000000 {
		t.Errorf("round trip = %q,%d,%v", id, ts, err)
	}
	for _, foreign := range []string{"", "m9", "m9:abc", ":123", "a:b:c"} {
		if _, _, err := parseCursor(foreign); err == nil {
			t.Errorf("foreign cursor %q must be rejected", foreign)
		}
	}
}

func TestSanitizeName(t *testing.T) {
	if got, ok := sanitizeName("photo ✓.jpg"); !ok || got != "photo ✓.jpg" {
		t.Errorf("safe name = %q,%v", got, ok)
	}
	for _, bad := range []string{"", ".", "..", "a\x00b", "a\nb"} {
		if _, ok := sanitizeName(bad); ok {
			t.Errorf("name %q must be rejected", bad)
		}
	}
	// Paths reduce to their basename; traversal never survives.
	if got, ok := sanitizeName("/tmp/../x"); !ok || got != "x" {
		t.Errorf("path name = %q,%v", got, ok)
	}
	long := ""
	for len(long) < 300 {
		long += "x"
	}
	if got, ok := sanitizeName(long); !ok || len(got) != 255 {
		t.Errorf("long name = %d,%v", len(got), ok)
	}
}
