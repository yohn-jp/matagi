// Package config persists the validated declarative Matagi registry.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/yohn-jp/matagi/internal/registry"
)

const (
	registryFilename = "registry.json"
	currentVersion   = 1
)

type document struct {
	Version      int                    `json:"version"`
	Environments []registry.Environment `json:"environments"`
	Services     []registry.Service     `json:"services"`
}

// Store reads and writes one versioned registry document beneath root.
type Store struct {
	path string
}

// NewStore creates a store rooted at the supplied Matagi-owned state folder.
func NewStore(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("state root is required")
	}
	return &Store{path: filepath.Join(root, registryFilename)}, nil
}

// UserStateRoot selects the Matagi state folder beneath the operating system's
// user configuration directory. On Windows this is the user's AppData folder.
func UserStateRoot() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("select user configuration directory: %w", err)
	}
	return StateRootFor(configDir)
}

// StateRootFor returns the deterministic Matagi-owned child of configDir.
func StateRootFor(configDir string) (string, error) {
	if strings.TrimSpace(configDir) == "" {
		return "", errors.New("user configuration directory is required")
	}
	return filepath.Clean(filepath.Join(configDir, "Matagi")), nil
}

// NewUserStore creates a store in the current user's Matagi state folder.
func NewUserStore() (*Store, error) {
	root, err := UserStateRoot()
	if err != nil {
		return nil, err
	}
	return NewStore(root)
}

// Path returns the registry document path.
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Save writes snapshot as canonical JSON using a temporary file and atomic
// replacement. Snapshot is already validated and cannot be mutated by callers.
func (s *Store) Save(snapshot *registry.Snapshot) error {
	if s == nil || s.path == "" {
		return errors.New("registry store is not initialized")
	}
	if snapshot == nil {
		return errors.New("registry snapshot is required")
	}
	data, err := json.MarshalIndent(document{
		Version:      currentVersion,
		Environments: snapshot.Environments(),
		Services:     snapshot.Services(),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode registry: %w", err)
	}
	data = append(data, '\n')
	if err := s.writeAtomic(data); err != nil {
		return fmt.Errorf("save registry: %w", err)
	}
	return nil
}

// Load decodes and validates the entire document before returning a snapshot.
// It never exposes a partially decoded or invalid registry.
func (s *Store) Load() (*registry.Snapshot, error) {
	if s == nil || s.path == "" {
		return nil, errors.New("registry store is not initialized")
	}
	file, err := os.Open(s.path)
	if err != nil {
		return nil, fmt.Errorf("open registry: %w", err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var stored document
	if err := decoder.Decode(&stored); err != nil {
		return nil, fmt.Errorf("decode registry: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("decode registry: trailing JSON value")
		}
		return nil, fmt.Errorf("decode registry trailing data: %w", err)
	}
	if stored.Version != currentVersion {
		return nil, fmt.Errorf("unsupported registry schema version %d", stored.Version)
	}
	snapshot, err := registry.NewSnapshot(stored.Environments, stored.Services)
	if err != nil {
		return nil, fmt.Errorf("validate registry: %w", err)
	}
	return snapshot, nil
}

func (s *Store) writeAtomic(data []byte) error {
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create state root: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".registry-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary registry: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("secure temporary registry: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary registry: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary registry: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary registry: %w", err)
	}
	if err := os.Rename(temporaryName, s.path); err != nil {
		return fmt.Errorf("replace registry: %w", err)
	}
	return nil
}
