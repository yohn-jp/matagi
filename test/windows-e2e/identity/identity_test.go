package identity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFailClosed(t *testing.T) {
	dir := t.TempDir()
	candidate := filepath.Join(dir, "candidate")
	evidence := filepath.Join(dir, "evidence")
	os.Mkdir(candidate, 0700)
	source := strings.Repeat("a", 40)
	os.WriteFile(filepath.Join(candidate, File), []byte("MZ executable"), 0600)
	c, err := Record(candidate, source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Verify(candidate, source, c.SHA256); err != nil {
		t.Fatal(err)
	}
	if _, err = Verify(candidate, strings.Repeat("b", 40), c.SHA256); err == nil {
		t.Fatal("wrong commit accepted")
	}
	if _, err = Aggregate(candidate, evidence, source, c.SHA256, true); err == nil {
		t.Fatal("missing evidence accepted")
	}
	for _, shard := range Shards {
		path := filepath.Join(evidence, "windows-e2e-"+shard)
		os.MkdirAll(path, 0700)
		if err := RecordEvidence(filepath.Join(path, "result.json"), Evidence{shard, source, c.SHA256, true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = Aggregate(candidate, evidence, source, c.SHA256, false); err == nil {
		t.Fatal("failed job accepted")
	}
	cert, err := Aggregate(candidate, evidence, source, c.SHA256, true)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cert.json")
	SaveCertification(path, cert)
	if err = CheckCertification(path, candidate, source, c.SHA256); err != nil {
		t.Fatal(err)
	}
	if err = CheckCertification(path, candidate, source, strings.Repeat("0", 64)); err == nil {
		t.Fatal("wrong digest accepted")
	}
	os.WriteFile(filepath.Join(candidate, File), []byte("tampered"), 0600)
	if _, err = Verify(candidate, source, c.SHA256); err == nil {
		t.Fatal("tampered bytes accepted")
	}
}
