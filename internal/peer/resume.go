package peer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type downloadState struct {
	ManifestID   string `json:"manifestId"`
	ManifestPath string `json:"manifestPath"`
	OutputPath   string `json:"outputPath"`
	TrackerURL   string `json:"trackerURL"`
	OriginURL    string `json:"originURL"`
	StartedAt    int64  `json:"startedAt"`
	CompletedAt  *int64 `json:"completedAt"`
}

func stateFilePath(storeDir, manifestID string) string {
	return filepath.Join(storeDir, manifestID+".download.json")
}

func lockFilePath(storeDir, manifestID string) string {
	return filepath.Join(storeDir, manifestID+".lock")
}

func acquireLock(storeDir, manifestID string) (*os.File, error) {
	path := lockFilePath(storeDir, manifestID)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("another download of this manifest is already running (lockfile: %s)", path)
		}
		return nil, fmt.Errorf("create lockfile: %w", err)
	}
	return f, nil
}

func releaseLock(f *os.File, storeDir, manifestID string) {
	_ = f.Close()
	_ = os.Remove(lockFilePath(storeDir, manifestID))
}

func LoadState(storeDir, manifestID string) (*downloadState, error) {
	path := stateFilePath(storeDir, manifestID)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state file: %w", err)
	}
	var s downloadState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse state file: %w", err)
	}
	return &s, nil
}

func saveState(storeDir string, s *downloadState) error {
	s.StartedAt = time.Now().Unix()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(stateFilePath(storeDir, s.ManifestID), data, 0o644)
}

func markComplete(storeDir, manifestID string) error {
	s, err := LoadState(storeDir, manifestID)
	if err != nil || s == nil {
		return err
	}
	now := time.Now().Unix()
	s.CompletedAt = &now
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(stateFilePath(storeDir, manifestID), data, 0o644)
}