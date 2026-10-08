package main

import (
	"reflect"
	"testing"
)

func TestParseRemoteCommand(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    []string
	}{
		{
			name:    "Jinushi command",
			command: `'jinushi' 'run' '--correlation=owner=fixture service'`,
			want:    []string{"jinushi", "run", "--correlation=owner=fixture service"},
		},
		{
			name:    "empty and shell-sensitive arguments",
			command: `'' 'single'\''quote' '$(touch /tmp/marker); ` + "`touch /tmp/marker`" + `' 'backslash\value' 'café 日本語'`,
			want: []string{
				"", "single'quote", "$(touch /tmp/marker); `touch /tmp/marker`", `backslash\value`, "café 日本語",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseRemoteCommand(test.command)
			if err != nil {
				t.Fatalf("parseRemoteCommand(%q) error = %v", test.command, err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("parseRemoteCommand(%q) = %#v, want %#v", test.command, got, test.want)
			}
		})
	}
}

func TestParseRemoteCommandRejectsNonCanonicalShellSyntax(t *testing.T) {
	for _, command := range []string{"", "'unterminated", "'quoted' extra", "'quoted'\\x", "'one'  'two'"} {
		t.Run(command, func(t *testing.T) {
			if _, err := parseRemoteCommand(command); err == nil {
				t.Fatalf("parseRemoteCommand(%q) succeeded; want malformed command error", command)
			}
		})
	}
}
