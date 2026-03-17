package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/peercdn/peercdn/internal/manifest"
	"github.com/peercdn/peercdn/internal/peer"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "seed":
		runSeed(os.Args[2:])
	case "get":
		runGet(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

// Will add more functions later - Anant
func printUsage() {
	fmt.Println("Usage:")
	fmt.Println("  peer seed  --tracker <url> --manifest <path> --store <dir> [--listen :9001]")
	fmt.Println("  peer get   --tracker <url> --manifest <path> --store <dir> --origin <url> --out <path>")
}

func runSeed(args []string) {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	trackerURL  := fs.String("tracker", "ws://localhost:8080/ws", "tracker WebSocket URL")
	manifestPath := fs.String("manifest", "", "path to manifest JSON (required)")
	storeDir    := fs.String("store", "./chunks", "chunk store directory")
	listenAddr  := fs.String("listen", "", "address to accept inbound peer connections")
	_ = fs.Parse(args)

	if *manifestPath == "" {
		fmt.Fprintln(os.Stderr, "error: --manifest is required")
		os.Exit(1)
	}

	log := newLogger()
	m, err := manifest.Load(*manifestPath)
	if err != nil {
		log.Error("load manifest", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	p, err := peer.New(peer.Config{
		TrackerURL: *trackerURL,
		StoreDir:   *storeDir,
		ListenAddr: *listenAddr,
	}, log)
	if err != nil {
		log.Error("create peer", "err", err)
		os.Exit(1)
	}
	if err := p.Start(ctx); err != nil {
		log.Error("start peer", "err", err)
		os.Exit(1)
	}
	if err := p.Announce(m); err != nil {
		log.Error("announce", "err", err)
		os.Exit(1)
	}
	log.Info("seeding", "manifest", m.ID[:12], "chunks", m.ChunkCount())
	<-ctx.Done()
	log.Info("shutting down")
}

func runGet(args []string) {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	trackerURL   := fs.String("tracker", "ws://localhost:8080/ws", "tracker WebSocket URL")
	manifestPath := fs.String("manifest", "", "path to manifest JSON (required)")
	storeDir     := fs.String("store", "./chunks", "chunk store directory")
	originURL    := fs.String("origin", "", "origin HTTP URL for fallback (optional)")
	outPath      := fs.String("out", "", "output file path (required)")
	_ = fs.Parse(args)

	if *manifestPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "error: --manifest and --out are required")
		os.Exit(1)
	}

	log := newLogger()
	m, err := manifest.Load(*manifestPath)
	if err != nil {
		log.Error("load manifest", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	p, err := peer.New(peer.Config{
		TrackerURL: *trackerURL,
		OriginURL:  *originURL,
		StoreDir:   *storeDir,
	}, log)
	if err != nil {
		log.Error("create peer", "err", err)
		os.Exit(1)
	}
	if err := p.Start(ctx); err != nil {
		log.Error("start peer", "err", err)
		os.Exit(1)
	}

	log.Info("starting download", "manifest", m.ID[:12], "chunks", m.ChunkCount())
	if err := p.Download(ctx, m); err != nil {
		log.Error("download failed", "err", err)
		os.Exit(1)
	}

	store := mustStore(*storeDir, log)
	if err := store.Assemble(m, *outPath); err != nil {
		log.Error("assemble failed", "err", err)
		os.Exit(1)
	}
	log.Info("download complete", "out", *outPath, "size", m.Size)
}

func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func mustStore(dir string, log *slog.Logger) interface {
	Assemble(m *manifest.Manifest, destPath string) error
} {
	import_chunk_store := chunkStoreWrapper{dir: dir, log: log}
	return import_chunk_store
}

type chunkStoreWrapper struct {
	dir string
	log *slog.Logger
}

func (w chunkStoreWrapper) Assemble(m *manifest.Manifest, destPath string) error {
	return assembleChunks(w.dir, m, destPath)
}
