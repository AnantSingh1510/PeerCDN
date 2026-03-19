//	tracker [--addr :8443] [--cert cert.pem --key key.pem] [--no-tls]
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

	"github.com/peercdn/peercdn/internal/signaling"
	"github.com/peercdn/peercdn/internal/tlsutil"
)

func main() {
	addr     := flag.String("addr", ":8443", "listen address")
	certFile := flag.String("cert", "", "TLS cert file (optional, auto-generated if omitted)")
	keyFile  := flag.String("key", "", "TLS key file (optional, auto-generated if omitted)")
	noTLS   := flag.Bool("no-tls", false, "disable TLS entirely")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	srv := signaling.New(log)

	if *noTLS {
		log.Info("tracker starting without TLS", "addr", *addr)
		if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
			log.Error("tracker exited", "err", err)
			os.Exit(1)
		}
		return
	}

	tlsCfg, err := tlsutil.ServerConfig(*certFile, *keyFile)
	if err != nil {
		log.Error("TLS setup failed", "err", err)
		os.Exit(1)
	}

	log.Info("tracker starting with TLS",
		"addr", *addr,
		"auto-cert", *certFile == "",
	)

	server := &http.Server{
		Addr:      *addr,
		Handler:   srv.Handler(),
		TLSConfig: tlsCfg,
	}

	if err := server.ListenAndServeTLS("", ""); err != nil {
		log.Error("tracker exited", "err", err)
		os.Exit(1)
	}
}