// Package statefile provides the bounded JSON file operations used by
// Matagi-owned state records.
package statefile

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ReadJSON decodes a JSON file at a path selected by a repository-owned state
// authority. Callers must not expose arbitrary filesystem paths to this API.
func ReadJSON(path string, value any) error {
	file, err := openReadFile(path)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

// WriteJSON writes JSON through a synced temporary file in the destination
// directory and atomically replaces the old record. The parent directory must
// already exist; callers own its creation and path validation.
func WriteJSON(path string, value any) (err error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmpFile, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := tmpFile.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err = tmpFile.Chmod(0o600); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if _, err = tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err = tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err = tmpFile.Close(); err != nil {
		return err
	}
	err = replaceFile(tmp, path)
	if err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	return nil
}
