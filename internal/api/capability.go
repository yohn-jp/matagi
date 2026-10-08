package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const capabilityHeader = "Authorization"

// Capability is an in-memory bearer capability for the local API. Its secret
// is never included in its formatted representation.
type Capability struct {
	secret [32]byte
	port   string
}

// NewCapability creates a fresh capability bound to one IPv4 loopback API
// listener address. The caller should keep the value in memory and pass it
// directly to the API handler and any authorized in-process client; it must
// not be serialized or exposed to the UI.
func NewCapability(apiAddress string) (Capability, error) {
	port, ok := loopbackRequestPort(apiAddress)
	if !ok {
		return Capability{}, fmt.Errorf("API capability requires an IPv4 loopback listener address")
	}
	for {
		capability := Capability{port: port}
		if _, err := rand.Read(capability.secret[:]); err != nil {
			return Capability{}, fmt.Errorf("reading random bytes: %w", err)
		}
		if !capability.empty() {
			return capability, nil
		}
	}
}

// AddToRequest attaches this capability only to POST requests for the IPv4
// loopback HTTP API. In particular, it cannot accompany a request to a
// tunneled product origin.
func (c Capability) AddToRequest(req *http.Request) {
	if c.empty() || req == nil || req.URL == nil || req.Method != http.MethodPost || req.URL.Scheme != "http" || req.URL.User != nil {
		return
	}
	port, ok := loopbackRequestPort(req.URL.Host)
	if !ok || port != c.port {
		return
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	token := base64.RawURLEncoding.EncodeToString(c.secret[:])
	req.Header.Set(capabilityHeader, "Bearer "+token)
}

func (c Capability) permits(values []string, requestHost string) bool {
	port, ok := loopbackRequestPort(requestHost)
	if c.empty() || !ok || port != c.port || len(values) != 1 {
		return false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.TrimSpace(values[0]) != values[0] || strings.ContainsAny(token, " \t\r\n") {
		return false
	}
	expected := base64.RawURLEncoding.EncodeToString(c.secret[:])
	return subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1
}

func (c Capability) empty() bool {
	var zero [32]byte
	return subtle.ConstantTimeCompare(c.secret[:], zero[:]) == 1
}

func (Capability) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "[redacted API capability]")
}
