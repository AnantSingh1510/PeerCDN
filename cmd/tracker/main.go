//	tracker [--addr :8080]
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

	"github.com/peercdn/peercdn/internal/signaling"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	srv := signaling.New(log)

	log.Info("tracker starting", "addr", *addr)
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Error("tracker exited", "err", err)
		os.Exit(1)
	}
}
