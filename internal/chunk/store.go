package chunk

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/peercdn/peercdn/internal/manifest"
)

var ErrNotFound = errors.New("chunk not found")
var ErrHashMismatch = errors.New("chunk hash mismatch")

type Store struct {
	baseDir string
}

func NewStore(baseDir string) (*Store, error) {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("create store dir: %w", err)
	}
	return &Store{baseDir: baseDir}, nil
}

func (s *Store) chunkPath(manifestID, chunkHash string) string {
	return filepath.Join(s.baseDir, manifestID, chunkHash)
}

func (s *Store) Has(manifestID, chunkHash string) bool {
	data, err := s.Get(manifestID, chunkHash)
	if err != nil {
		return false
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:]) == chunkHash
}

func (s *Store) Get(manifestID, chunkHash string) ([]byte, error) {
	path := s.chunkPath(manifestID, chunkHash)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read chunk: %w", err)
	}
	// Verify integrity
	h := sha256.Sum256(data)
	if got := hex.EncodeToString(h[:]); got != chunkHash {
		// File is corrupted
		_ = os.Remove(path)
		return nil, ErrHashMismatch
	}
	return data, nil
}

func (s *Store) Put(manifestID, chunkHash string, data []byte) error {
	h := sha256.Sum256(data)
	if got := hex.EncodeToString(h[:]); got != chunkHash {
		return ErrHashMismatch
	}
	dir := filepath.Join(s.baseDir, manifestID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create manifest dir: %w", err)
	}
	// Write to a temp file then rename for atomicity.
	tmp, err := os.CreateTemp(dir, "chunk-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp chunk: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write chunk: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close chunk: %w", err)
	}
	dest := s.chunkPath(manifestID, chunkHash)
	if err := os.Rename(tmpName, dest); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename chunk: %w", err)
	}
	return nil
}

func (s *Store) HaveChunks(m *manifest.Manifest) []bool {
	have := make([]bool, len(m.Chunks))
	for i, c := range m.Chunks {
		have[i] = s.Has(m.ID, c.Hash)
	}
	return have
}

func (s *Store) Assemble(m *manifest.Manifest, destPath string) error {
	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create output file: %w", err)
	}
	defer out.Close()

	for _, c := range m.Chunks {
		data, err := s.Get(m.ID, c.Hash)
		if err != nil {
			return fmt.Errorf("chunk %d (%s): %w", c.Index, c.Hash[:8], err)
		}
		if _, err := out.Write(data); err != nil {
			return fmt.Errorf("write chunk %d: %w", c.Index, err)
		}
	}
	return nil
}

func (s *Store) SeedFromFile(m *manifest.Manifest, filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open seed file: %w", err)
	}
	defer f.Close()

	buf := make([]byte, m.ChunkSize)
	for _, c := range m.Chunks {
		data := buf[:c.Size]
		if _, err := f.ReadAt(data, c.Offset); err != nil {
			return fmt.Errorf("read chunk %d: %w", c.Index, err)
		}
		if err := s.Put(m.ID, c.Hash, data); err != nil {
			return fmt.Errorf("store chunk %d: %w", c.Index, err)
		}
	}
	return nil
}
