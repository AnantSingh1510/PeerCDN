package main

import (
	"fmt"

	"github.com/peercdn/peercdn/internal/chunk"
	"github.com/peercdn/peercdn/internal/manifest"
)

func assembleChunks(storeDir string, m *manifest.Manifest, destPath string) error {
	store, err := chunk.NewStore(storeDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	return store.Assemble(m, destPath)
}
