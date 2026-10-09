package presentation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/yohn-jp/matagi/internal/statefile"
)

const layoutFilename = "desktop.json"

// ErrUnsafeExistingLayout means an existing desktop.json cannot be replaced
// without an explicit ResetSavedLayout.
var ErrUnsafeExistingLayout = errors.New("existing desktop layout cannot be safely replaced; reset it explicitly first")

// ErrLayoutTooLarge reports a serialized layout that exceeds the bounded read
// limit and therefore could not be restored on the next launch.
var ErrLayoutTooLarge = errors.New("desktop layout exceeds the maximum file size")

// Store persists only Matagi's desktop presentation layout. Its per-path lock
// coordinates separate Store instances in this process.
type Store struct {
	root string
	path string
	mu   *sync.Mutex
}

var storeLocks = struct {
	sync.Mutex
	byPath map[string]*sync.Mutex
}{byPath: make(map[string]*sync.Mutex)}

type layoutRecord struct {
	Schema   string          `json:"schema"`
	Views    json.RawMessage `json:"views"`
	Selected json.RawMessage `json:"selected,omitempty"`
}

type layoutFileRecord struct {
	Schema   string        `json:"schema"`
	Views    []LayoutEntry `json:"views"`
	Selected *EndpointKey  `json:"selected,omitempty"`
}

// NewStore creates a layout store rooted in Matagi's user state folder. It
// does not create or modify files until Save is called.
func NewStore(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("Matagi desktop state root is required")
	}
	clean, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, fmt.Errorf("resolve Matagi desktop state root: %w", err)
	}
	path := filepath.Join(clean, layoutFilename)
	return &Store{root: clean, path: path, mu: lockForStore(path)}, nil
}

func lockForStore(path string) *sync.Mutex {
	storeLocks.Lock()
	defer storeLocks.Unlock()
	if lock := storeLocks.byPath[path]; lock != nil {
		return lock
	}
	lock := &sync.Mutex{}
	storeLocks.byPath[path] = lock
	return lock
}

// Load reads a bounded layout and keeps only the first valid occurrence of
// each key present in validKeys. Read, schema, and entry problems are reported
// as a bounded summary so callers can keep the management workspace usable.
// The file is never changed by Load.
func (s *Store) Load(validKeys map[EndpointKey]struct{}) (Layout, LoadSummary, error) {
	if s == nil || s.path == "" || s.mu == nil {
		return Layout{}, LoadSummary{}, errors.New("Matagi desktop store is not initialized")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var record layoutRecord
	err := statefile.ReadJSONBounded(s.path, &record, MaxLayoutBytes)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Layout{}, LoadSummary{}, nil
	case errors.Is(err, statefile.ErrTooLarge):
		return Layout{}, LoadSummary{Problem: LoadProblemTooLarge}, nil
	case err != nil:
		problem := LoadProblemUnreadable
		var syntaxError *json.SyntaxError
		var typeError *json.UnmarshalTypeError
		if errors.As(err, &syntaxError) || errors.As(err, &typeError) {
			problem = LoadProblemMalformed
		}
		return Layout{}, LoadSummary{Problem: problem}, nil
	case record.Schema == "":
		return Layout{}, LoadSummary{Problem: LoadProblemMalformed}, nil
	case record.Schema != Schema:
		return Layout{}, LoadSummary{Problem: LoadProblemUnknown}, nil
	}
	entries, err := decodeEntries(record.Views)
	if err != nil {
		return Layout{}, LoadSummary{Problem: LoadProblemMalformed}, nil
	}

	layout := Layout{Views: make([]LayoutEntry, 0, min(len(entries), MaxLayoutEntries))}
	summary := LoadSummary{}
	seen := make(map[EndpointKey]struct{}, min(len(entries), MaxLayoutEntries))
	for index, rawEntry := range entries {
		if index >= MaxLayoutEntries {
			addSkipped(&summary, len(entries)-MaxLayoutEntries)
			break
		}
		var entry LayoutEntry
		if err := json.Unmarshal(rawEntry, &entry); err != nil {
			addSkipped(&summary, 1)
			continue
		}
		if !entry.Key.Valid() || !entry.Location.valid() || (entry.Bounds != nil && (!entry.Bounds.valid() || entry.Location == LocationTab)) {
			addSkipped(&summary, 1)
			continue
		}
		if _, ok := validKeys[entry.Key]; !ok {
			addSkipped(&summary, 1)
			continue
		}
		if _, duplicate := seen[entry.Key]; duplicate {
			addSkipped(&summary, 1)
			continue
		}
		seen[entry.Key] = struct{}{}
		layout.Views = append(layout.Views, LayoutEntry{
			Key:      entry.Key,
			Location: entry.Location,
			Bounds:   cloneBounds(entry.Bounds),
		})
	}
	if len(record.Selected) != 0 && !bytes.Equal(bytes.TrimSpace(record.Selected), []byte("null")) {
		var selected EndpointKey
		if err := json.Unmarshal(record.Selected, &selected); err == nil && selected.Valid() {
			if _, ok := seen[selected]; ok {
				layout.Selected = &selected
			} else {
				addSkipped(&summary, 1)
			}
		} else {
			addSkipped(&summary, 1)
		}
	}
	return layout, summary, nil
}

func decodeEntries(raw json.RawMessage) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, errors.New("desktop layout views must be an array")
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(trimmed, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func addSkipped(summary *LoadSummary, count int) {
	if count <= 0 {
		return
	}
	remaining := MaxLayoutEntries - summary.Skipped
	if count > remaining {
		summary.Skipped = MaxLayoutEntries
		summary.MoreSkipped = true
		return
	}
	summary.Skipped += count
}

// Save atomically writes a validated layout. A malformed, oversized, or
// unknown-schema file already at the destination is preserved until the
// caller explicitly invokes ResetSavedLayout, including across Store values.
func (s *Store) Save(layout Layout, validKeys map[EndpointKey]struct{}) error {
	if s == nil || s.path == "" || s.mu == nil {
		return errors.New("Matagi desktop store is not initialized")
	}
	if err := validateLayout(layout, validKeys, true); err != nil {
		return err
	}
	copy := cloneLayout(layout)
	record := layoutFileRecord{Schema: Schema, Views: copy.Views, Selected: copy.Selected}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal desktop layout: %w", err)
	}
	data = append(data, '\n')
	if len(data) > int(MaxLayoutBytes) {
		return fmt.Errorf("%w: maximum is %d bytes", ErrLayoutTooLarge, MaxLayoutBytes)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return fmt.Errorf("create Matagi desktop state root: %w", err)
	}
	if err := s.checkExisting(); err != nil {
		return err
	}
	if err := statefile.WriteJSON(s.path, record); err != nil {
		return fmt.Errorf("write desktop layout: %w", err)
	}
	return nil
}

func (s *Store) checkExisting() error {
	var existing layoutRecord
	err := statefile.ReadJSONBounded(s.path, &existing, MaxLayoutBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnsafeExistingLayout, err)
	}
	if existing.Schema != Schema {
		return fmt.Errorf("%w: schema %q is unsupported", ErrUnsafeExistingLayout, existing.Schema)
	}
	if _, err := decodeEntries(existing.Views); err != nil {
		return fmt.Errorf("%w: malformed views: %v", ErrUnsafeExistingLayout, err)
	}
	return nil
}

// ResetSavedLayout explicitly removes only desktop.json. It does not remove
// the Matagi state root or touch settings and registry files.
func (s *Store) ResetSavedLayout() error {
	if s == nil || s.path == "" || s.mu == nil {
		return errors.New("Matagi desktop store is not initialized")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("reset desktop layout: %w", err)
	}
	return nil
}
