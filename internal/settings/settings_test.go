package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yohn-jp/matagi/internal/i18n"
	"github.com/yohn-jp/matagi/internal/update"
)

func TestLocaleIsAdditiveAndPersistsAcrossNewStores(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, filename)
	legacy := `{"schema":"matagi.settings/1"}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.Locale(); err != nil || got != "" {
		t.Fatalf("legacy locale = %q, %v", got, err)
	}
	if err := store.SetLocale("fr"); err == nil {
		t.Fatal("unsupported locale was accepted")
	}
	if err := store.SetLocale("ja"); err != nil {
		t.Fatal(err)
	}
	other, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := other.Locale(); err != nil || got != i18n.Japanese {
		t.Fatalf("reloaded locale = %q, %v", got, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"schema": "matagi.settings/1"`) || !strings.Contains(string(data), `"locale": "ja"`) {
		t.Fatalf("saved settings do not contain the schema and locale: %s", data)
	}
	if err := other.SetLocale(""); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Locale(); err != nil || got != "" {
		t.Fatalf("cleared locale = %q, %v", got, err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"locale"`) {
		t.Fatalf("cleared locale remained in settings: %s", data)
	}
}

func TestLocaleAndUpdateSettingsShareOneRecord(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	want := update.Settings{Channel: update.Development, Installed: &update.Installed{Version: "1.2.3", SHA256: strings.Repeat("a", 64)}}
	if err := store.ModifyUpdateSettings(func(saved *update.Settings) error {
		*saved = want
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetLocale("ja"); err != nil {
		t.Fatal(err)
	}
	got, err := store.UpdateSettings()
	if err != nil || got.Channel != want.Channel || got.Installed == nil || *got.Installed != *want.Installed {
		t.Fatalf("update settings = %#v, %v", got, err)
	}
	// Returned pointers do not mutate the persisted settings.
	got.Installed.Version = "9.9.9"
	reloaded, err := store.UpdateSettings()
	if err != nil || reloaded.Installed.Version != want.Installed.Version {
		t.Fatalf("mutating returned settings changed storage: %#v, %v", reloaded, err)
	}
	if locale, err := store.Locale(); err != nil || locale != i18n.Japanese {
		t.Fatalf("locale = %q, %v", locale, err)
	}
}

func TestInvalidUpdateSettingsAreNotWritten(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	err = store.ModifyUpdateSettings(func(saved *update.Settings) error {
		saved.Installed = &update.Installed{Version: "not-a-version", SHA256: "bad"}
		return nil
	})
	if err == nil {
		t.Fatal("invalid update state was accepted")
	}
	if _, err := os.Stat(filepath.Join(root, filename)); !os.IsNotExist(err) {
		t.Fatalf("invalid update state wrote settings: %v", err)
	}
}

func TestNewStoreRequiresRootAndReportsCorruptSettings(t *testing.T) {
	if _, err := NewStore(" "); err == nil {
		t.Fatal("empty settings root was accepted")
	}
	root := t.TempDir()
	path := filepath.Join(root, filename)
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Locale(); err == nil {
		t.Fatal("corrupt settings were not reported")
	}
	if err := store.SetLocale("ja"); err == nil {
		t.Fatal("corrupt settings were overwritten")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "{" {
		t.Fatalf("corrupt settings changed: %q, %v", data, err)
	}
}

func TestUnsupportedLocaleInRecordIsRejected(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, filename)
	body := `{"schema":"matagi.settings/1","locale":"fr"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	store, _ := NewStore(root)
	if _, err := store.Locale(); err == nil {
		t.Fatal("unsupported saved locale was accepted")
	}
}
