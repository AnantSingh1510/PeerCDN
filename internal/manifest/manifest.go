// Package manifest defines the data structures shared across all PeerCDN
// components. A Manifest describes how a file has been split into chunks,
// with enough information for any peer to verify integrity and reassemble.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

const (
	// DefaultChunkSize is 512 KB
	DefaultChunkSize = 512 * 1024

	Version = 1
)

type Chunk struct {
	Index  int    `json:"index"`
	Hash   string `json:"hash"` 
	Offset int64  `json:"offset"`
	Size   int    `json:"size"`
}

// Manifest is the top-level descriptor for a chunked file.
// It is produced by the chunker and consumed by peers to coordinate downloads.
type Manifest struct {
	Version   int     `json:"version"`
	ID        string  `json:"id"`        // SHA-256 of the full file content
	Name      string  `json:"name"`      // original filename
	Size      int64   `json:"size"`      // total file size in bytes
	ChunkSize int     `json:"chunkSize"` // nominal chunk size (last chunk may differ)
	Chunks    []Chunk `json:"chunks"`
	CreatedAt int64   `json:"createdAt"` // unix timestamp
	MimeType  string  `json:"mimeType,omitempty"`
}

func (m *Manifest) ChunkCount() int {
	return len(m.Chunks)
}

func (m *Manifest) Validate() error {
	if m.Version != Version {
		return fmt.Errorf("unsupported manifest version %d", m.Version)
	}
	if m.ID == "" {
		return fmt.Errorf("manifest ID is empty")
	}
	if len(m.Chunks) == 0 {
		return fmt.Errorf("manifest has no chunks")
	}
	for i, c := range m.Chunks {
		if len(c.Hash) != 64 {
			return fmt.Errorf("chunk %d has invalid hash length %d", i, len(c.Hash))
		}
		if c.Index != i {
			return fmt.Errorf("chunk %d has mismatched index %d", i, c.Index)
		}
	}
	return nil
}

func (m *Manifest) Save(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create manifest file: %w", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

func Load(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	defer f.Close()
	var m Manifest
	if err := json.NewDecoder(f).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("invalid manifest: %w", err)
	}
	return &m, nil
}

func HashBytes(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func HashReader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func NewManifest(filePath string, chunkSize int, mimeType string) (*Manifest, error) {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}

	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat file: %w", err)
	}

	fileHasher := sha256.New()
	tee := io.TeeReader(f, fileHasher)

	buf := make([]byte, chunkSize)
	var chunks []Chunk
	var offset int64

	for {
		n, err := io.ReadFull(tee, buf)
		if n > 0 {
			data := buf[:n]
			chunks = append(chunks, Chunk{
				Index:  len(chunks),
				Hash:   HashBytes(data),
				Offset: offset,
				Size:   n,
			})
			offset += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read file: %w", err)
		}
	}

	fileHash := hex.EncodeToString(fileHasher.Sum(nil))

	return &Manifest{
		Version:   Version,
		ID:        fileHash,
		Name:      info.Name(),
		Size:      info.Size(),
		ChunkSize: chunkSize,
		Chunks:    chunks,
		CreatedAt: time.Now().Unix(),
		MimeType:  mimeType,
	}, nil
}
