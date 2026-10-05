//go:build !windows

package desktop

import (
	"context"
	"fmt"
	"os"
)

type native struct{}

func Native() Platform { return native{} }

func (native) Open(context.Context, Window) error { return ErrUnsupported }

func (native) ReportError(title, message string) {
	fmt.Fprintf(os.Stderr, "%s: %s\n", title, message)
}
