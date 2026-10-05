package desktop

import (
	"errors"
	"sync"

	"github.com/yohn-jp/matagi/internal/ui"
)

// Policy admits the Matagi UI origin and exact loopback origins returned by
// successful endpoint-ensure operations in this process.
type Policy struct {
	mu       sync.RWMutex
	uiOrigin string
	ensured  map[string]struct{}
}

func NewPolicy(uiURL string) (*Policy, error) {
	origin, err := ui.LoopbackHTTPOrigin(uiURL)
	if err != nil {
		return nil, errors.New("Matagi UI URL must be an IPv4 loopback HTTP URL with a port")
	}
	return &Policy{uiOrigin: origin, ensured: make(map[string]struct{})}, nil
}

// AdmitEnsuredEndpoint is called only after the HTTP client receives a
// successful endpoint-ensure response.
func (p *Policy) AdmitEnsuredEndpoint(localURL string) error {
	origin, err := ui.LoopbackHTTPOrigin(localURL)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.ensured[origin] = struct{}{}
	p.mu.Unlock()
	return nil
}

func (p *Policy) AllowNavigation(uri string) bool {
	if p == nil {
		return false
	}
	origin, err := ui.LoopbackHTTPOrigin(uri)
	if err != nil {
		return false
	}
	if origin == p.uiOrigin {
		return true
	}
	p.mu.RLock()
	_, ok := p.ensured[origin]
	p.mu.RUnlock()
	return ok
}

func (*Policy) AllowNewWindow(string) bool { return false }
