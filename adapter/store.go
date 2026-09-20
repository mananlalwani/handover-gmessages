// Copyright (C) 2026 Handover contributors
// SPDX-License-Identifier: AGPL-3.0-only
package adapter

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

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

// SaveAuth persists one account's libgm session atomically with 0600
// permissions.
func (s *Store) SaveAuth(account string, auth *libgm.AuthData) error {
	path, ok := s.accountFile(account)
	if !ok {
		return errInvalidAccount
	}
	if err := s.ensureDir(); err != nil {
		return err
	}
	raw, err := json.Marshal(auth)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
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
