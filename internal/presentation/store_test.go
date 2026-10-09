package presentation

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/yohn-jp/matagi/internal/statefile"
)

func TestStoreRoundTripsBoundedLogicalLayout(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	first := testKey("env-a", "service", "web")
	second := testKey("env-b", "service", "admin")
	selected := second
	bounds := &DIPBounds{X: -1220.5, Y: 40.25, Width: 1024, Height: 768}
	want := Layout{
		Views: []LayoutEntry{
			{Key: first, Location: LocationTab},
			{Key: second, Location: LocationWindow, Bounds: bounds},
		},
		Selected: &selected,
	}
	validKeys := keySet(first, second)
	if err := store.Save(want, validKeys); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(root, layoutFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"url", "port", "token", "health", "process", "desiredState"} {
		if strings.Contains(strings.ToLower(string(data)), strings.ToLower(forbidden)) {
			t.Fatalf("saved layout contains forbidden field %q: %s", forbidden, data)
		}
	}
	var schema struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(data, &schema); err != nil || schema.Schema != Schema {
		t.Fatalf("saved schema = %q, error = %v", schema.Schema, err)
	}

	got, summary, err := store.Load(validKeys)
	if err != nil {
		t.Fatal(err)
	}
	if summary != (LoadSummary{}) {
		t.Fatalf("load summary = %#v", summary)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loaded layout = %#v, want %#v", got, want)
	}
}

func TestStoreMissingFileLoadsEmptyLayout(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	layout, summary, err := store.Load(nil)
	if err != nil || summary != (LoadSummary{}) || len(layout.Views) != 0 || layout.Selected != nil {
		t.Fatalf("missing layout = %#v, %#v, %v", layout, summary, err)
	}
}

func TestStoreFiltersInvalidAndDuplicateKeysKeepingFirstValidEntry(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	first := testKey("env-a", "service", "web")
	second := testKey("env-b", "service", "web")
	removed := testKey("removed-env", "service", "web")
	selectedRemoved := removed
	record := layoutFileRecord{
		Schema: Schema,
		Views: []LayoutEntry{
			{Key: removed, Location: LocationTab},
			{Key: first, Location: LocationTab, Bounds: &DIPBounds{Width: 500, Height: 400}}, // Invalid: tabs have no window bounds.
			{Key: first, Location: LocationWindow, Bounds: &DIPBounds{X: -40, Y: 20, Width: 700, Height: 500}},
			{Key: first, Location: LocationTab}, // Duplicate after the first valid occurrence.
			{Key: second, Location: LocationTab},
		},
		Selected: &selectedRemoved,
	}
	writeRecord(t, root, record)

	layout, summary, err := store.Load(keySet(first, second))
	if err != nil {
		t.Fatal(err)
	}
	if summary.Skipped != 4 || summary.MoreSkipped {
		t.Fatalf("filter summary = %#v, want 4 skipped", summary)
	}
	if len(layout.Views) != 2 || layout.Views[0].Key != first || layout.Views[0].Location != LocationWindow || layout.Views[1].Key != second {
		t.Fatalf("filtered layout = %#v", layout)
	}
	if layout.Selected != nil {
		t.Fatalf("removed selection was retained: %#v", layout.Selected)
	}
}

func TestStoreEnforcesEntryAndByteLimits(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]LayoutEntry, MaxLayoutEntries+1)
	keys := make(map[EndpointKey]struct{}, len(entries))
	for index := range entries {
		key := testKey("env", "service", "endpoint-"+strconv.Itoa(index))
		entries[index] = LayoutEntry{Key: key, Location: LocationTab}
		keys[key] = struct{}{}
	}
	if err := store.Save(Layout{Views: entries}, keys); err == nil {
		t.Fatal("Save() accepted more than the entry limit")
	}

	oversized := strings.Repeat(" ", int(MaxLayoutBytes+1))
	if err := os.WriteFile(filepath.Join(root, layoutFilename), []byte(oversized), 0o600); err != nil {
		t.Fatal(err)
	}
	layout, summary, err := store.Load(keys)
	if err != nil || summary.Problem != LoadProblemTooLarge || len(layout.Views) != 0 {
		t.Fatalf("oversized layout = %#v, %#v, %v", layout, summary, err)
	}
}

func TestStoreRejectsOversizedSaveWithoutChangingPreviousFile(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	initial := testKey("env", "service", "web")
	if err := store.Save(Layout{Views: []LayoutEntry{{Key: initial, Location: LocationTab}}}, keySet(initial)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, layoutFilename)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	entries := make([]LayoutEntry, MaxLayoutEntries)
	validKeys := make(map[EndpointKey]struct{}, len(entries))
	for index := range entries {
		key := testKey(strings.Repeat("e", 1200)+strconv.Itoa(index), strings.Repeat("s", 1200), "endpoint")
		entries[index] = LayoutEntry{Key: key, Location: LocationTab}
		validKeys[key] = struct{}{}
	}
	err = store.Save(Layout{Views: entries}, validKeys)
	if !errors.Is(err, ErrLayoutTooLarge) {
		t.Fatalf("oversized Save() error = %v, want ErrLayoutTooLarge", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("oversized save changed previous file: %v", err)
	}
}

func TestStoreSkipsTypeInvalidEntriesWithoutLosingValidNeighbors(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	first := testKey("env-a", "service", "web")
	second := testKey("env-b", "service", "admin")
	data := []byte(`{"schema":"matagi.desktop/1","views":[` +
		`{"key":{"environmentId":"env-a","serviceId":"service","endpointId":"web"},"location":"tab"},` +
		`{"key":{"environmentId":"env-b","serviceId":7,"endpointId":"admin"},"location":"tab"},` +
		`{"key":{"environmentId":"env-b","serviceId":"service","endpointId":"admin"},"location":false}],` +
		`"selected":{"environmentId":"env-a","serviceId":"service","endpointId":"web"}}`)
	if err := os.WriteFile(filepath.Join(root, layoutFilename), data, 0o600); err != nil {
		t.Fatal(err)
	}

	layout, summary, err := store.Load(keySet(first, second))
	if err != nil {
		t.Fatal(err)
	}
	if summary.Skipped != 2 || summary.Problem != "" {
		t.Fatalf("type-invalid entry summary = %#v", summary)
	}
	if len(layout.Views) != 1 || layout.Views[0].Key != first || layout.Selected == nil || *layout.Selected != first {
		t.Fatalf("valid neighboring entry was lost: %#v", layout)
	}
}

func TestStoreBlocksCorruptAndUnknownFilesAcrossInstancesUntilReset(t *testing.T) {
	valid := testKey("env", "service", "web")
	goodLayout := Layout{Views: []LayoutEntry{{Key: valid, Location: LocationTab}}}
	cases := []struct {
		name    string
		content []byte
		problem LoadProblem
	}{
		{name: "malformed", content: []byte(`{"schema":"matagi.desktop/1","views":[`), problem: LoadProblemMalformed},
		{name: "unknown schema", content: []byte(`{"schema":"matagi.desktop/2","views":[]}`), problem: LoadProblemUnknown},
		{name: "oversized", content: []byte(strings.Repeat("x", int(MaxLayoutBytes+1))), problem: LoadProblemTooLarge},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, layoutFilename)
			if err := os.WriteFile(path, test.content, 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			firstStore, _ := NewStore(root)
			loaded, summary, err := firstStore.Load(keySet(valid))
			if err != nil || summary.Problem != test.problem || len(loaded.Views) != 0 {
				t.Fatalf("Load() = %#v, %#v, %v", loaded, summary, err)
			}
			secondStore, _ := NewStore(root)
			if err := secondStore.Save(goodLayout, keySet(valid)); !errors.Is(err, ErrUnsafeExistingLayout) {
				t.Fatalf("Save() error = %v, want ErrUnsafeExistingLayout", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) {
				t.Fatalf("protected file changed: %v", err)
			}
			if err := secondStore.ResetSavedLayout(); err != nil {
				t.Fatal(err)
			}
			if err := secondStore.Save(goodLayout, keySet(valid)); err != nil {
				t.Fatalf("Save() after explicit reset: %v", err)
			}
		})
	}
}

func TestStoreResetAndSaveLeaveOtherStateFilesUnchanged(t *testing.T) {
	root := t.TempDir()
	settingsPath := filepath.Join(root, "settings.json")
	registryPath := filepath.Join(root, "registry.json")
	settings := []byte(`{"schema":"matagi.settings/1","locale":"ja"}`)
	registry := []byte(`{"schema":"matagi.registry/1"}`)
	if err := os.WriteFile(settingsPath, settings, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registryPath, registry, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	key := testKey("env", "service", "web")
	if err := store.Save(Layout{Views: []LayoutEntry{{Key: key, Location: LocationTab}}}, keySet(key)); err != nil {
		t.Fatal(err)
	}
	if err := store.ResetSavedLayout(); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][]byte{settingsPath: settings, registryPath: registry} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != string(want) {
			t.Fatalf("state file %s changed: got %q, error %v", filepath.Base(path), got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, layoutFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("desktop layout remains after reset: %v", err)
	}
}

func TestStoreSaveFailureLeavesFileAndModelUsable(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	key := testKey("env", "service", "web")
	validKeys := keySet(key)
	original := Layout{Views: []LayoutEntry{{Key: key, Location: LocationTab}}}
	if err := store.Save(original, validKeys); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, layoutFilename)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	model := NewModel()
	reserved, err := model.ReserveOpen(key, LocationTab)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := model.CompleteOpen(reserved.Operation); !ok {
		t.Fatal("CompleteOpen() rejected current reservation")
	}
	invalid := Layout{Views: []LayoutEntry{{Key: key, Location: "invalid"}}}
	if err := store.Save(invalid, validKeys); err == nil {
		t.Fatal("Save() accepted an invalid layout")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("failed save changed previous file: %v", err)
	}
	if got := model.Snapshot().Views[0].State; got != ViewCreated {
		t.Fatalf("failed persistence made model unusable: state = %q", got)
	}
}

func TestStoreRejectsInvalidKeysOnSave(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := testKey("env", "service", "web")
	if err := store.Save(Layout{Views: []LayoutEntry{{Key: key, Location: LocationTab}}}, nil); err == nil {
		t.Fatal("Save() accepted a key outside the injected valid-key set")
	}
}

func writeRecord(t *testing.T, root string, record layoutFileRecord) {
	t.Helper()
	if err := statefile.WriteJSON(filepath.Join(root, layoutFilename), record); err != nil {
		t.Fatal(err)
	}
}

func keySet(keys ...EndpointKey) map[EndpointKey]struct{} {
	result := make(map[EndpointKey]struct{}, len(keys))
	for _, key := range keys {
		result[key] = struct{}{}
	}
	return result
}
