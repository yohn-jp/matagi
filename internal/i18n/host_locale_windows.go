//go:build windows

package i18n

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var getUserDefaultLocaleName = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetUserDefaultLocaleName")

func nativeLocale() string {
	var name [85]uint16
	result, _, _ := getUserDefaultLocaleName.Call(uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)))
	if result == 0 {
		return ""
	}
	return windows.UTF16ToString(name[:])
}
