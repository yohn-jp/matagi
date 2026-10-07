//go:build windows

package desktop

import "testing"

func TestDPIWindowSizing(t *testing.T) {
	for _, tc := range []struct {
		dpi           uint32
		width, height int32
	}{{96, 1100, 780}, {120, 1375, 975}, {144, 1650, 1170}, {192, 2200, 1560}} {
		if got := scaleForDPI(1100, tc.dpi); got != tc.width {
			t.Errorf("%d DPI width = %d", tc.dpi, got)
		}
		if got := scaleForDPI(780, tc.dpi); got != tc.height {
			t.Errorf("%d DPI height = %d", tc.dpi, got)
		}
	}
	if !validDPIChangeRect(&dpiRect{left: -100, top: 20, right: 100, bottom: 200}) {
		t.Fatal("negative monitor position rejected")
	}
	for _, rect := range []*dpiRect{nil, {}, {left: 10, right: 10, bottom: 20}} {
		if validDPIChangeRect(rect) {
			t.Fatal("invalid suggested rectangle accepted")
		}
	}
}
