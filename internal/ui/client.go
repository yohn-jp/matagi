package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	apiVersion       = 1
	maxResponseBytes = 1 << 20
	maxErrorMessage  = 512
)

// Client consumes only the frozen loopback HTTP v1 service contract.
type Client struct {
	baseURL *url.URL
	http    *http.Client
}

// NewClient creates a client with a bounded request timeout. Redirects are
// returned to the caller so POST operations can never be replayed by HTTP
// redirect handling.
func NewClient(baseURL string, timeout time.Duration) (*Client, error) {
	if timeout <= 0 {
		return nil, errors.New("HTTP client timeout must be positive")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid API base URL: %w", err)
	}
	if _, err := LoopbackHTTPOrigin(baseURL); err != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("API base URL must be an IPv4 loopback HTTP origin")
	}
	origin, _ := LoopbackHTTPOrigin(baseURL)
	parsed, _ := url.Parse(origin)
	return &Client{
		baseURL: parsed,
		http: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// LoopbackHTTPOrigin validates an HTTP URL on explicit IPv4 loopback and
// returns its canonical origin. It is shared by the HTTP client and desktop
// navigation policy so both apply the same loopback boundary.
func LoopbackHTTPOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Opaque != "" || u.Hostname() != "127.0.0.1" {
		return "", errors.New("URL must use IPv4 loopback HTTP")
	}
	portText := u.Port()
	if portText == "" {
		return "", errors.New("loopback URL must include a port")
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return "", errors.New("loopback URL has an invalid port")
	}
	return "http://127.0.0.1:" + strconv.FormatUint(port, 10), nil
}

// State is the presentation projection returned by GET /v1/state.
type State struct {
	Version      int           `json:"version"`
	Environments []Environment `json:"environments"`
}

type Environment struct {
	ID           string    `json:"id"`
	Connectivity string    `json:"connectivity"`
	Jinushi      string    `json:"jinushi"`
	Error        string    `json:"error"`
	Services     []Service `json:"services"`
}

type Service struct {
	ID             string     `json:"id"`
	DesiredState   string     `json:"desiredState"`
	State          string     `json:"state"`
	Process        string     `json:"process"`
	Readiness      string     `json:"readiness"`
	ProcessError   string     `json:"processError"`
	ReadinessError string     `json:"readinessError"`
	Endpoints      []Endpoint `json:"endpoints"`
}

type Endpoint struct {
	ID            string `json:"id"`
	Label         string `json:"label"`
	EndpointState string `json:"endpointState"`
	TunnelState   string `json:"tunnelState"`
	LocalURL      string `json:"localUrl"`
	Failure       string `json:"failure"`
}

type ServiceRequest struct {
	EnvironmentID string `json:"environmentId"`
	ServiceID     string `json:"serviceId"`
}

type EndpointRequest struct {
	EnvironmentID string `json:"environmentId"`
	ServiceID     string `json:"serviceId"`
	EndpointID    string `json:"endpointId"`
}

// ServiceResult is the v1 success envelope for service lifecycle operations.
type ServiceResult struct {
	Version int             `json:"version"`
	Service json.RawMessage `json:"service"`
}

type EndpointResult struct {
	Version  int      `json:"version"`
	Endpoint Endpoint `json:"endpoint"`
}

// APIError is the bounded v1 error envelope returned for non-success statuses.
type APIError struct {
	Code       string
	Message    string
	StatusCode int
}

func (e *APIError) Error() string {
	if e == nil {
		return "Matagi API error"
	}
	if e.Message == "" {
		return fmt.Sprintf("Matagi API error (%s, HTTP %d)", e.Code, e.StatusCode)
	}
	return fmt.Sprintf("Matagi API error (%s): %s", e.Code, e.Message)
}

type errorEnvelope struct {
	Version int `json:"version"`
	Error   struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) GetState(ctx context.Context) (State, error) {
	var state State
	if err := c.get(ctx, "/v1/state", &state); err != nil {
		return State{}, err
	}
	if state.Version != apiVersion {
		return State{}, fmt.Errorf("unsupported Matagi API version %d", state.Version)
	}
	return state, nil
}

// Register sends a complete registry document; the API validates it before persistence.
func (c *Client) EnsureJinushi(ctx context.Context, environmentID string) error {
	var response struct {
		Version int `json:"version"`
	}
	if err := c.post(ctx, "/v1/environment/ensure-jinushi", struct {
		EnvironmentID string `json:"environmentId"`
	}{environmentID}, &response); err != nil {
		return err
	}
	if response.Version != apiVersion {
		return errors.New("invalid Jinushi response")
	}
	return nil
}

func (c *Client) Register(ctx context.Context, document json.RawMessage) error {
	var state State
	return c.post(ctx, "/v1/environment/register", document, &state)
}

func (c *Client) Start(ctx context.Context, req ServiceRequest) (ServiceResult, error) {
	return c.serviceAction(ctx, "/v1/service/start", req)
}

func (c *Client) Stop(ctx context.Context, req ServiceRequest) (ServiceResult, error) {
	return c.serviceAction(ctx, "/v1/service/stop", req)
}

func (c *Client) Restart(ctx context.Context, req ServiceRequest) (ServiceResult, error) {
	return c.serviceAction(ctx, "/v1/service/restart", req)
}

func (c *Client) serviceAction(ctx context.Context, route string, req ServiceRequest) (ServiceResult, error) {
	var result ServiceResult
	if err := c.post(ctx, route, req, &result); err != nil {
		return ServiceResult{}, err
	}
	serviceJSON := bytes.TrimSpace(result.Service)
	if result.Version != apiVersion || len(serviceJSON) == 0 || serviceJSON[0] != '{' {
		return ServiceResult{}, errors.New("invalid Matagi service response")
	}
	var service map[string]json.RawMessage
	if err := json.Unmarshal(serviceJSON, &service); err != nil || service == nil {
		return ServiceResult{}, errors.New("invalid Matagi service response")
	}
	return result, nil
}

func (c *Client) EnsureEndpoint(ctx context.Context, req EndpointRequest) (EndpointResult, error) {
	var result EndpointResult
	if err := c.post(ctx, "/v1/endpoint/ensure", req, &result); err != nil {
		return EndpointResult{}, err
	}
	if result.Version != apiVersion {
		return EndpointResult{}, errors.New("invalid Matagi endpoint response")
	}
	return result, nil
}

func (c *Client) get(ctx context.Context, route string, dst any) error {
	return c.request(ctx, http.MethodGet, route, nil, dst)
}

func (c *Client) post(ctx context.Context, route string, body, dst any) error {
	return c.request(ctx, http.MethodPost, route, body, dst)
}

func (c *Client) request(ctx context.Context, method, route string, body, dst any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding Matagi API request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	target := *c.baseURL
	target.Path = route
	req, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return fmt.Errorf("creating Matagi API request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	encoded, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("reading Matagi API response: %w", err)
	}
	if len(encoded) > maxResponseBytes {
		return errors.New("Matagi API response exceeded size limit")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		var envelope errorEnvelope
		apiErr := &APIError{StatusCode: resp.StatusCode, Code: "http_error"}
		if decodeJSON(encoded, &envelope) == nil && envelope.Version == apiVersion && envelope.Error.Code != "" {
			apiErr.Code = bounded(envelope.Error.Code, 64)
			apiErr.Message = bounded(envelope.Error.Message, maxErrorMessage)
		} else {
			apiErr.Message = fmt.Sprintf("request failed with HTTP status %d", resp.StatusCode)
		}
		return apiErr
	}
	if err := decodeJSON(encoded, dst); err != nil {
		return fmt.Errorf("decoding Matagi API response: %w", err)
	}
	return nil
}

func decodeJSON(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("response contains trailing JSON")
		}
		return err
	}
	return nil
}

func bounded(value string, limit int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}
