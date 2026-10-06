//go:build !windows

package desktop

func HideOwnedConsole() bool { return false }
