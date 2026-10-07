//go:build windows

package statefile

import (
	"os"
	"path/filepath"
)

func openReadFile(path string) (*os.File, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Open(filepath.Base(path))
}
