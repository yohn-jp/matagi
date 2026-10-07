//go:build !windows

package statefile

import "os"

func openReadFile(path string) (*os.File, error) { return os.Open(path) }
