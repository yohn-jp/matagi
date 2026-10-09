package statefile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadJSONBoundedAcceptsExactLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var got struct {
		OK bool `json:"ok"`
	}
	if err := ReadJSONBounded(path, &got, int64(len(`{"ok":true}`))); err != nil {
		t.Fatal(err)
	}
	if !got.OK {
		t.Fatalf("decoded value = %#v", got)
	}
}

func TestReadJSONBoundedRejectsOversizedInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var got map[string]bool
	err := ReadJSONBounded(path, &got, int64(len(`{"ok":true}`))-1)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("ReadJSONBounded() error = %v, want ErrTooLarge", err)
	}
}

func TestReadJSONBoundedRejectsNonPositiveLimit(t *testing.T) {
	for _, limit := range []int64{0, -1} {
		if err := ReadJSONBounded("unused", nil, limit); !errors.Is(err, ErrInvalidLimit) {
			t.Errorf("limit %d error = %v, want ErrInvalidLimit", limit, err)
		}
	}
}
