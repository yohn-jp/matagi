// Package identity binds a portable Windows candidate and its certification evidence.
package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const File = "matagi.exe"

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type Candidate struct {
	Source string `json:"source"`
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func hash(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func read(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err = d.Decode(v); err != nil {
		return err
	}
	var rest any
	if d.Decode(&rest) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func write(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0644)
}

func Record(dir, source string) (Candidate, error) {
	if !commitPattern.MatchString(source) {
		return Candidate{}, errors.New("invalid source SHA")
	}
	sum, n, err := hash(filepath.Join(dir, File))
	if err != nil {
		return Candidate{}, err
	}
	if n == 0 {
		return Candidate{}, errors.New("empty candidate")
	}
	c := Candidate{source, File, sum, n}
	return c, write(filepath.Join(dir, "candidate.json"), c)
}

func Verify(dir, source, sum string) (Candidate, error) {
	if !commitPattern.MatchString(source) || !hashPattern.MatchString(sum) {
		return Candidate{}, errors.New("missing or invalid out-of-band identity")
	}
	var c Candidate
	if err := read(filepath.Join(dir, "candidate.json"), &c); err != nil {
		return c, err
	}
	if c.Source != source || c.SHA256 != sum || c.File != File || c.Size <= 0 {
		return c, errors.New("candidate identity mismatch")
	}
	actual, n, err := hash(filepath.Join(dir, c.File))
	if err != nil {
		return c, err
	}
	if actual != sum || n != c.Size {
		return c, errors.New("candidate bytes mismatch")
	}
	return c, nil
}

type Evidence struct {
	Shard  string `json:"shard"`
	Source string `json:"source"`
	SHA256 string `json:"sha256"`
	Passed bool   `json:"passed"`
}

func RecordEvidence(path string, e Evidence) error { return write(path, e) }

var Shards = []string{"startup", "surface", "transport", "lifecycle", "single-instance"}

type Certification struct {
	Source string `json:"source"`
	SHA256 string `json:"sha256"`
	File   string `json:"file"`
	Passed bool   `json:"passed"`
}

func Aggregate(candidateDir, evidenceDir, source, sum string, jobsOK bool) (Certification, error) {
	c, err := Verify(candidateDir, source, sum)
	if err != nil {
		return Certification{}, fmt.Errorf("candidate: %w", err)
	}
	if !jobsOK {
		return Certification{}, errors.New("required jobs did not succeed")
	}
	for _, shard := range Shards {
		var e Evidence
		if err := read(filepath.Join(evidenceDir, "windows-e2e-"+shard, "result.json"), &e); err != nil {
			return Certification{}, fmt.Errorf("%s: %w", shard, err)
		}
		if e.Shard != shard || e.Source != source || e.SHA256 != sum || !e.Passed {
			return Certification{}, fmt.Errorf("%s evidence failed identity or status", shard)
		}
	}
	return Certification{source, sum, c.File, true}, nil
}

func SaveCertification(path string, c Certification) error { return write(path, c) }

func CheckCertification(path, dir, source, sum string) error {
	if _, err := Verify(dir, source, sum); err != nil {
		return err
	}
	var c Certification
	if err := read(path, &c); err != nil {
		return err
	}
	if !c.Passed || c.Source != source || c.SHA256 != sum || c.File != File {
		return errors.New("certification does not cover candidate")
	}
	return nil
}
