package runtime

import (
	"strings"
	"testing"
)

func TestDecodeEndpointDescriptorRequiresOneURLField(t *testing.T) {
	for _, test := range []struct {
		name string
		data string
		want string
		ok   bool
	}{
		{name: "valid descriptor", data: `{"url":"http://127.0.0.1:34299/"}`, want: "http://127.0.0.1:34299/", ok: true},
		{name: "duplicate url", data: `{"url":"http://127.0.0.1:34299","url":"http://127.0.0.1:45789"}`},
		{name: "unknown field", data: `{"url":"http://127.0.0.1:34299","port":34299}`},
		{name: "wrong shape", data: `[]`},
		{name: "trailing object", data: `{"url":"http://127.0.0.1:34299"}{}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := decodeEndpointDescriptor([]byte(test.data))
			if test.ok {
				if err != nil || got != test.want {
					t.Fatalf("decodeEndpointDescriptor() = %q, %v; want %q", got, err, test.want)
				}
			} else if err == nil {
				t.Fatalf("decodeEndpointDescriptor(%s) unexpectedly succeeded with %q", test.data, got)
			}
		})
	}
}

func TestParseEndpointURLRequiresLoopbackHTTPOrigin(t *testing.T) {
	for _, raw := range []string{
		"http://example.com:34299/",
		"https://127.0.0.1:34299/",
		"http://127.0.0.1/",
		"http://user@127.0.0.1:34299/",
		"http://127.0.0.1:34299/other",
		"http://127.0.0.1:34299/?query=1",
		"http://127.0.0.1:70000/",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseEndpointURL(raw); err == nil {
				t.Fatalf("parseEndpointURL(%q) succeeded", raw)
			}
		})
	}
	got, err := parseEndpointURL("http://127.0.0.1:34299")
	if err != nil || got.port != 34299 || got.origin != "http://127.0.0.1:34299" {
		t.Fatalf("parseEndpointURL() = %#v, %v", got, err)
	}
}

func TestRunEndpointTargetRequiresOneLoopbackURL(t *testing.T) {
	for _, test := range []struct {
		name    string
		output  string
		port    uint16
		wantErr string
	}{
		{name: "dynamic success", output: "Yokodori dashboard: http://127.0.0.1:34299/\n", port: 34299},
		{name: "missing evidence", output: "service started\n", wantErr: endpointEvidenceMissing},
		{name: "multiple instances", output: "http://127.0.0.1:34299/ http://127.0.0.1:45789/", wantErr: endpointEvidenceAmbiguous},
		{name: "malformed loopback URL", output: "http://127.0.0.1:0/", wantErr: endpointEvidenceInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := runEndpointTarget([]byte(test.output))
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("runEndpointTarget() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil || got.port != test.port || got.origin != "http://127.0.0.1:34299" {
				t.Fatalf("runEndpointTarget() = %#v, %v", got, err)
			}
		})
	}
}
