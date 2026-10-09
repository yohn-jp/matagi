package statefile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

var (
	// ErrInvalidLimit reports a non-positive state-file read limit.
	ErrInvalidLimit = errors.New("state file read limit must be positive")
	// ErrTooLarge reports a state file that exceeds the caller's byte limit.
	ErrTooLarge = errors.New("state file exceeds read limit")
)

// ReadJSONBounded decodes a JSON file using the platform-specific statefile
// opener and rejects input larger than maxBytes before decoding it.
func ReadJSONBounded(path string, value any, maxBytes int64) error {
	if maxBytes <= 0 {
		return ErrInvalidLimit
	}
	file, err := openReadFile(path)
	if err != nil {
		return err
	}
	defer file.Close()

	limited := &io.LimitedReader{R: file, N: maxBytes}
	data, err := io.ReadAll(limited)
	if err != nil {
		return err
	}
	if int64(len(data)) == maxBytes {
		var probe [1]byte
		n, probeErr := file.Read(probe[:])
		if n > 0 {
			return fmt.Errorf("%w: maximum is %d bytes", ErrTooLarge, maxBytes)
		}
		if probeErr != nil && probeErr != io.EOF {
			return probeErr
		}
	}
	return json.Unmarshal(data, value)
}
