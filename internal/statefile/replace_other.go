//go:build !windows

package statefile

import "os"

func replaceFile(replacement, replaced string) error {
	return os.Rename(replacement, replaced)
}
