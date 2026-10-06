package api

import (
	"encoding/json"
	"github.com/yohn-jp/matagi/internal/registry"
	"os"
	"testing"
)

func TestFirstDeploymentExampleValidates(t *testing.T) {
	data, err := os.ReadFile("../../examples/first-deployment.json")
	if err != nil {
		t.Fatal(err)
	}
	var input registration
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.NewSnapshot(input.Environments, input.Services)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Environments()) != 1 || len(snapshot.Services()) != 2 {
		t.Fatal("example must register development, Inari and Yokodori")
	}
}
