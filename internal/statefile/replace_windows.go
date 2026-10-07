//go:build windows

package statefile

import (
	"errors"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var replaceFileW = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")

func replaceFile(replacement, replaced string) error {
	dst, err := windows.UTF16PtrFromString(replaced)
	if err != nil {
		return err
	}
	src, err := windows.UTF16PtrFromString(replacement)
	if err != nil {
		return err
	}
	var last error
	for attempt := 0; attempt < 50; attempt++ {
		ok, _, callErr := replaceFileW.Call(
			uintptr(unsafe.Pointer(dst)),
			uintptr(unsafe.Pointer(src)),
			0, 0, 0, 0,
		)
		if ok != 0 {
			return nil
		}
		last = callErr
		if errors.Is(callErr, windows.ERROR_FILE_NOT_FOUND) || errors.Is(callErr, windows.ERROR_PATH_NOT_FOUND) {
			return os.Rename(replacement, replaced)
		}
		if !errors.Is(callErr, windows.ERROR_ACCESS_DENIED) &&
			!errors.Is(callErr, windows.ERROR_SHARING_VIOLATION) &&
			!errors.Is(callErr, windows.ERROR_LOCK_VIOLATION) {
			return callErr
		}
		time.Sleep(10 * time.Millisecond)
	}
	return last
}
