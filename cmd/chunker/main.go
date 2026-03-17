package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/peercdn/peercdn/internal/chunk"
	"github.com/peercdn/peercdn/internal/manifest"
)

func main() {
	filePath  := flag.String("file", "", "path to file to chunk (required)")
	chunkSize := flag.Int("chunk-size", manifest.DefaultChunkSize, "chunk size in bytes")
	outDir    := flag.String("out", "./chunks", "directory to write chunks and manifest")
	mimeType  := flag.String("mime", "", "MIME type hint (optional)")
	flag.Parse()

	if *filePath == "" {
		fmt.Fprintln(os.Stderr, "error: --file is required")
		flag.Usage()
		os.Exit(1)
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	log.Info("building manifest", "file", *filePath, "chunkSize", *chunkSize)
	m, err := manifest.NewManifest(*filePath, *chunkSize, *mimeType)
	if err != nil {
		log.Error("manifest build failed", "err", err)
		os.Exit(1)
	}
	log.Info("manifest built", "id", m.ID[:12], "chunks", m.ChunkCount(), "size", m.Size)

	store, err := chunk.NewStore(*outDir)
	if err != nil {
		log.Error("store init failed", "err", err)
		os.Exit(1)
	}
	log.Info("seeding chunks into store", "dir", *outDir)
	if err := store.SeedFromFile(m, *filePath); err != nil {
		log.Error("seed failed", "err", err)
		os.Exit(1)
	}

	manifestPath := fmt.Sprintf("%s/%s.json", *outDir, m.ID)
	if err := m.Save(manifestPath); err != nil {
		log.Error("save manifest failed", "err", err)
		os.Exit(1)
	}

	log.Info("done",
		"manifest", manifestPath,
		"chunks", m.ChunkCount(),
		"totalBytes", m.Size,
	)

	fmt.Printf("\nManifest ID : %s\n", m.ID)
	fmt.Printf("File        : %s (%d bytes)\n", m.Name, m.Size)
	fmt.Printf("Chunks      : %d × %d bytes\n", m.ChunkCount(), m.ChunkSize)
	fmt.Printf("Manifest    : %s\n", manifestPath)
}
