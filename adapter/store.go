// Copyright (C) 2026 Handover contributors
// SPDX-License-Identifier: AGPL-3.0-only
package adapter

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
)

// errInvalidAccount rejects unsafe account names before they touch the
// filesystem. It names no secret material.
var errInvalidAccount = errors.New("invalid account name")

// Store persists adapter state with restrictive permissions. Auth/session
// material (cookies, tokens, keys) rests in one 0600 file per account;
// nothing here ever logs file contents.
type Store struct {
	dir string

	sweepMu   sync.Mutex
	lastSweep time.Time
}

// maybeSweep runs a staging sweep at most every sweepInterval.
// Staging ops call it after writing; a non-blocking lock keeps a
// slow sweep off the transfer path.
func (s *Store) maybeSweep() {
	if !s.sweepMu.TryLock() {
		return
	}
	defer s.sweepMu.Unlock()
	if time.Since(s.lastSweep) < sweepInterval {
		return
	}
	s.lastSweep = time.Now()
	if removed, err := s.SweepStaged(); err != nil {
		_ = removed
	}
}

// NewStore resolves the adapter state directory. It honors
// XDG_STATE_HOME like the rest of the desktop and refuses relative
// resolutions.
func NewStore() (*Store, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		base = filepath.Join(home, ".local", "state")
	}
	if !filepath.IsAbs(base) {
		base, _ = filepath.Abs(base)
	}
	return &Store{dir: filepath.Join(base, "handover", "gmessages")}, nil
}

func (s *Store) accountFile(account string) (string, bool) {
	if account == "" || len(account) > 128 || strings.ContainsAny(account, "/\\.") ||
		strings.Contains(account, "\x00") {
		return "", false
	}
	return filepath.Join(s.dir, account+".session.json"), true
}

func (s *Store) ensureDir() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(s.dir, 0o700)
}

// SaveAuth persists one account's libgm session with 0600
// permissions. The write goes to a unique temp file in the same
// directory before an atomic rename: concurrent saves for one
// account must never share (and clobber) a single temp path.
func (s *Store) SaveAuth(account string, auth *libgm.AuthData) error {
	raw, err := json.Marshal(auth)
	if err != nil {
		return err
	}
	return s.WriteAuth(account, raw)
}

// WriteAuth persists pre-marshaled session bytes through a unique
// temp file and atomic rename. Split from SaveAuth so callers can
// marshal and gate outside their own locks before touching disk.
func (s *Store) WriteAuth(account string, raw []byte) error {
	path, ok := s.accountFile(account)
	if !ok {
		return errInvalidAccount
	}
	if err := s.ensureDir(); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".session-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best effort cleanup; the rename below removes the common path.
	defer os.Remove(tmpName)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// LoadAuth reads one account's session. A missing file is not an error.
func (s *Store) LoadAuth(account string) (*libgm.AuthData, error) {
	path, ok := s.accountFile(account)
	if !ok {
		return nil, errInvalidAccount
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var auth libgm.AuthData
	if err := json.Unmarshal(raw, &auth); err != nil {
		return nil, err
	}
	return &auth, nil
}

// DeleteAuth removes one account's session. Missing files are fine.
func (s *Store) DeleteAuth(account string) error {
	path, ok := s.accountFile(account)
	if !ok {
		return errInvalidAccount
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Accounts lists accounts with persisted sessions.
func (s *Store) Accounts() []string {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".session.json") {
			out = append(out, strings.TrimSuffix(name, ".session.json"))
		}
	}
	return out
}

// StageDir returns the adapter's attachment staging directory.
func (s *Store) StageDir() (string, error) {
	dir := filepath.Join(s.dir, "staged")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// Staged retention bounds: transfer data is transient, but nothing
// deletes it after delivery. Files older than this are garbage.
const stagedMaxAge = 7 * 24 * time.Hour

// stagedMaxBytes caps the staging directory. Beyond the cap the
// oldest files go first, so one large transfer cannot pin the disk.
const stagedMaxBytes = 256 << 20

// sessionTmpMaxAge bounds crash-left session temp files. Live saves
// use unique names and rename away promptly; anything older is debris.
const sessionTmpMaxAge = time.Hour

// sweepInterval spaces background staging sweeps. Startup sweeps
// (see main) handle crashed runs; this bounds long-lived processes
// whose staging would otherwise grow without restart.
const sweepInterval = time.Hour

// SweepStaged deletes staged attachments older than stagedMaxAge,
// enforces stagedMaxBytes oldest-first, and clears crash-left session
// temp files. It returns the number of files removed. Only regular
// non-symlink files are ever deleted.
func (s *Store) SweepStaged() (int, error) {
	removed := 0
	dir, err := s.StageDir()
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		path    string
		size    int64
		modTime time.Time
	}
	var kept []candidate
	var total int64
	cutoff := time.Now().Add(-stagedMaxAge)
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(path); err == nil {
				removed++
			}
			continue
		}
		kept = append(kept, candidate{path: path, size: info.Size(), modTime: info.ModTime()})
		total += info.Size()
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].modTime.Before(kept[j].modTime) })
	for _, c := range kept {
		if total <= stagedMaxBytes {
			break
		}
		if err := os.Remove(c.path); err == nil {
			removed++
			total -= c.size
		}
	}
	// Crash-left session temps share the store directory, not staging.
	sessionEntries, err := os.ReadDir(s.dir)
	if err != nil {
		return removed, nil
	}
	tmpCutoff := time.Now().Add(-sessionTmpMaxAge)
	for _, entry := range sessionEntries {
		name := entry.Name()
		if !strings.HasPrefix(name, ".session-") || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || !info.ModTime().Before(tmpCutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, name)); err == nil {
			removed++
		}
	}
	return removed, nil
}
