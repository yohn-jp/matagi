package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/yohn-jp/matagi/internal/jinushi"
	"github.com/yohn-jp/matagi/internal/registry"
)

const maxEndpointDescriptorBytes = 4 * 1024

var endpointURLPattern = regexp.MustCompile(`https?://[^\s"'<>]+`)

const (
	endpointEvidenceMissing   = "Dynamic endpoint evidence is missing. Start the managed service and check its descriptor and Run output."
	endpointEvidenceInvalid   = "Dynamic endpoint evidence is invalid. Check the JSON URL descriptor and managed Run output."
	endpointEvidenceAmbiguous = "Dynamic endpoint evidence is ambiguous. Keep one correlated managed Run and publish one endpoint URL."
	endpointEvidenceStale     = "Dynamic endpoint evidence is stale or conflicting. Restart the managed service."
	endpointOutputIncomplete  = "Dynamic endpoint Run output is incomplete. Restart the managed service to retain its endpoint URL."
	endpointApplicationDown   = "The remote application endpoint is not responding through the tunnel."
)

type endpointTarget struct {
	port   uint16
	origin string
}

func (r *Runtime) resolveEndpoint(ctx context.Context, service registry.Service, endpoint registry.Endpoint) (uint16, error) {
	if endpoint.Resolution == nil {
		return uint16(endpoint.RemotePort), nil
	}
	binding, exists := r.bindings[string(service.EnvironmentID)]
	if !exists {
		return 0, errors.New(endpointEvidenceMissing)
	}
	owner := service.CorrelationOwner()
	before, err := binding.observe.Status(ctx, owner)
	if err != nil {
		return 0, runEvidenceFailure(err)
	}
	if before.Run == nil || before.Run.State != jinushi.StateRunning {
		return 0, errors.New(endpointEvidenceMissing)
	}
	runID := before.Run.ID
	output, err := binding.observe.ReadRunOutput(ctx, runID)
	if err != nil {
		return 0, errors.New(endpointOutputIncomplete)
	}
	descriptorResult, err := r.ssh.Run(ctx, binding.environment.SSHHost, []string{
		"head", "-c", strconv.Itoa(maxEndpointDescriptorBytes + 1), "--", endpoint.Resolution.Path,
	}, commandTimeout)
	if err != nil || descriptorResult.ExitCode != 0 {
		return 0, errors.New(endpointEvidenceMissing)
	}
	if len(descriptorResult.Stdout) > maxEndpointDescriptorBytes {
		return 0, errors.New(endpointEvidenceInvalid)
	}
	descriptorURL, err := decodeEndpointDescriptor(descriptorResult.Stdout)
	if err != nil {
		return 0, errors.New(endpointEvidenceInvalid)
	}
	descriptorTarget, err := parseEndpointURL(descriptorURL)
	if err != nil {
		return 0, errors.New(endpointEvidenceInvalid)
	}
	runTarget, err := runEndpointTarget(output)
	if err != nil {
		return 0, err
	}
	if runTarget.origin != descriptorTarget.origin || runTarget.port != descriptorTarget.port {
		return 0, errors.New(endpointEvidenceStale)
	}
	after, err := binding.observe.Status(ctx, owner)
	if err != nil {
		return 0, runEvidenceFailure(err)
	}
	if after.Run == nil || after.Run.ID != runID || after.Run.State != jinushi.StateRunning {
		return 0, errors.New(endpointEvidenceStale)
	}
	return descriptorTarget.port, nil
}

func runEvidenceFailure(err error) error {
	var failure *jinushi.Failure
	if errors.As(err, &failure) && failure.Kind == jinushi.KindAmbiguous {
		return errors.New(endpointEvidenceAmbiguous)
	}
	return errors.New(endpointEvidenceMissing)
}

func decodeEndpointDescriptor(data []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return "", errors.New("endpoint descriptor must be a JSON object")
	}
	var endpointURL string
	seenURL := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return "", errors.New("endpoint descriptor is malformed")
		}
		key, ok := keyToken.(string)
		if !ok || key != "url" || seenURL {
			return "", errors.New("endpoint descriptor must contain one url field")
		}
		if err := decoder.Decode(&endpointURL); err != nil {
			return "", errors.New("endpoint descriptor url is malformed")
		}
		seenURL = true
	}
	if _, err := decoder.Token(); err != nil || !seenURL || endpointURL == "" {
		return "", errors.New("endpoint descriptor has no url")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return "", errors.New("endpoint descriptor contains trailing data")
	}
	return endpointURL, nil
}

func parseEndpointURL(raw string) (endpointTarget, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil || parsed.Scheme != "http" || parsed.Opaque != "" || parsed.User != nil || parsed.Hostname() != "127.0.0.1" || parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return endpointTarget{}, errors.New("endpoint URL must be a loopback HTTP origin")
	}
	portText := parsed.Port()
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return endpointTarget{}, errors.New("endpoint URL has an invalid port")
	}
	return endpointTarget{port: uint16(port), origin: fmt.Sprintf("http://127.0.0.1:%d", port)}, nil
}

func runEndpointTarget(output []byte) (endpointTarget, error) {
	seen := map[string]endpointTarget{}
	for _, match := range endpointURLPattern.FindAll(output, -1) {
		candidate := strings.TrimRight(string(match), ".,;:!?)]}")
		parsed, err := url.Parse(candidate)
		if err != nil || parsed == nil || parsed.Hostname() != "127.0.0.1" {
			continue
		}
		target, err := parseEndpointURL(candidate)
		if err != nil {
			return endpointTarget{}, errors.New(endpointEvidenceInvalid)
		}
		seen[target.origin] = target
	}
	if len(seen) == 0 {
		return endpointTarget{}, errors.New(endpointEvidenceMissing)
	}
	if len(seen) > 1 {
		return endpointTarget{}, errors.New(endpointEvidenceAmbiguous)
	}
	for _, target := range seen {
		return target, nil
	}
	return endpointTarget{}, errors.New(endpointEvidenceMissing)
}
