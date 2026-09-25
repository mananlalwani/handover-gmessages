// Copyright (C) 2026 Handover contributors
// SPDX-License-Identifier: AGPL-3.0-only
package adapter

import (
	"os"
	"path/filepath"
	"testing"
	"time"
	"unicode/utf8"

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
	for _, name := range []string{"", "../escape", "a/b", "a\\b", ".", "a.b", "x\x00y", "a\nb", "a\tb"} {
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

func TestSweepStagedBoundsRetention(t *testing.T) {
	dir := t.TempDir()
	store := &Store{dir: dir}
	staged, err := store.StageDir()
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-stagedMaxAge - time.Hour).Unix()
	write := func(name string, size int, modTime time.Time) {
		path := filepath.Join(staged, name)
		if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatal(err)
		}
	}
	fresh := time.Now()
	write("fresh.bin", 10, fresh)
	write("old.bin", 10, time.Unix(old, 0))
	if err := os.Symlink("fresh.bin", filepath.Join(staged, "link.bin")); err != nil {
		t.Fatal(err)
	}
	removed, err := store.SweepStaged()
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("sweep removed %d files, want 1 (the expired one)", removed)
	}
	for _, name := range []string{"fresh.bin", "link.bin"} {
		if _, err := os.Lstat(filepath.Join(staged, name)); err != nil {
			t.Errorf("%s must survive the sweep: %v", name, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(staged, "old.bin")); !os.IsNotExist(err) {
		t.Error("expired file must be swept")
	}
}

func TestSanitizeNameKeepsUTF8Intact(t *testing.T) {
	long := ""
	for len(long) < 300 {
		long += "✓"
	}
	got, ok := sanitizeName(long + ".jpg")
	if !ok {
		t.Fatal("unicode name must be accepted")
	}
	if len(got) > 255 {
		t.Errorf("truncated name is %d bytes", len(got))
	}
	for i := 0; i < len(got); {
		_, size := utf8.DecodeRuneInString(got[i:])
		if size <= 1 && got[i] >= 0x80 {
			t.Fatalf("name splits a rune: %q", got)
		}
		i += size
	}
}
