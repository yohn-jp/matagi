// Package settings persists Matagi-owned operator preferences and update
// state beneath the user's Matagi state root.
package settings

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/yohn-jp/matagi/internal/i18n"
	"github.com/yohn-jp/matagi/internal/statefile"
	"github.com/yohn-jp/matagi/internal/update"
)

// Schema versions the settings record.
const Schema = "matagi.settings/1"

const filename = "settings.json"

// Store owns the locale preference and the update subsystem's persisted
// settings. It does not own registered environments or desktop preferences.
type Store struct {
	path string
	mu   sync.Mutex
}

type record struct {
	Schema  string           `json:"schema"`
	Locale  i18n.Locale      `json:"locale,omitempty"`
	Updates *update.Settings `json:"updates,omitempty"`
}

// NewStore creates a store rooted in the Matagi-owned user state folder. It
// does not create or modify files until a preference is saved.
func NewStore(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("Matagi settings root is required")
	}
	return &Store{path: filepath.Join(filepath.Clean(root), filename)}, nil
}

// Locale returns the explicit saved display-language selection. An empty
// locale means that the caller should resolve from the host locale.
func (s *Store) Locale() (i18n.Locale, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.load()
	return r.Locale, err
}

// SetLocale saves an explicit display-language selection. An empty value
// clears it so the interface can resolve from the host locale again.
func (s *Store) SetLocale(locale string) error {
	if locale != "" && !i18n.Valid(locale) {
		return fmt.Errorf("locale %q is not supported (en or ja)", locale)
	}
	return s.modify(func(r *record) error {
		r.Locale = i18n.Locale(locale)
		return nil
	})
}

// UpdateSettings reads the updater's channel, last explicit check and
// installed identity. Missing state yields the updater's zero settings.
func (s *Store) UpdateSettings() (update.Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.load()
	if r.Updates == nil {
		return update.Settings{}, err
	}
	return r.Updates.Clone(), err
}

// ModifyUpdateSettings validates and persists a change made by the updater.
// The updater owns update policy; this store only records its state.
func (s *Store) ModifyUpdateSettings(modify func(*update.Settings) error) error {
	return s.modify(func(r *record) error {
		var current update.Settings
		if r.Updates != nil {
			current = r.Updates.Clone()
		}
		if err := modify(&current); err != nil {
			return err
		}
		if err := current.Validate(); err != nil {
			return err
		}
		r.Updates = &current
		return nil
	})
}

func (s *Store) load() (record, error) {
	if s == nil || s.path == "" {
		return record{}, errors.New("Matagi settings store is not initialized")
	}
	var r record
	err := statefile.ReadJSON(s.path, &r)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return record{Schema: Schema}, nil
	case err != nil:
		return record{}, fmt.Errorf("read settings %s: %w", s.path, err)
	case r.Schema != Schema:
		return record{}, fmt.Errorf("settings %s: unknown schema %q", s.path, r.Schema)
	}
	if r.Locale != "" && !i18n.Valid(string(r.Locale)) {
		return record{}, fmt.Errorf("settings %s: locale %q is not supported (en or ja)", s.path, r.Locale)
	}
	if r.Updates != nil {
		if err := r.Updates.Validate(); err != nil {
			return record{}, fmt.Errorf("settings %s: updates: %w", s.path, err)
		}
	}
	return r, nil
}

func (s *Store) modify(change func(*record) error) error {
	if s == nil || s.path == "" {
		return errors.New("Matagi settings store is not initialized")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.load()
	if err != nil {
		return err
	}
	if err := change(&r); err != nil {
		return err
	}
	if r.Locale != "" && !i18n.Valid(string(r.Locale)) {
		return fmt.Errorf("locale %q is not supported (en or ja)", r.Locale)
	}
	if r.Updates != nil {
		if err := r.Updates.Validate(); err != nil {
			return fmt.Errorf("updates: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create settings root: %w", err)
	}
	r.Schema = Schema
	if err := statefile.WriteJSON(s.path, r); err != nil {
		return fmt.Errorf("write settings %s: %w", s.path, err)
	}
	return nil
}

var _ update.Store = (*Store)(nil)
