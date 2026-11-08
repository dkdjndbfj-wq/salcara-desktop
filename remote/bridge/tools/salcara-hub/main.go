// Command salcara-hub is a standalone self-hosted Salcara Hub server.
//
// Usage:
//
//	salcara-hub [flags]
//
// Flags:
//
//	-addr      :addr to listen on (default ":8765")
//	-mode      station mode: open | restricted (default "open")
//	-revision  station policy revision string (default "self-hosted-v1")
//	-bases     comma-separated list of allowed API bases (for restricted mode)
//	-tls-cert  path to TLS certificate (PEM); enables HTTPS when set
//	-tls-key   path to TLS private key (PEM)
//	-v         verbose logging
//
// Example — run a local Hub for home use:
//
//	salcara-hub -addr :8765
//
// Then in Bridge, set the Hub URL to http://192.168.1.x:8765 or the machine's
// hostname.  Bridge's phone-pairing flow works identically to the cloud Hub.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"salcara/bridge/internal/hubserver"
)

func main() {
	addr := flag.String("addr", ":8765", "listen address")
	mode := flag.String("mode", "open", "station mode: open | restricted")
	revision := flag.String("revision", "self-hosted-v1", "station policy revision string")
	bases := flag.String("bases", "", "comma-separated allowed API bases (restricted mode)")
	tlsCert := flag.String("tls-cert", "", "TLS certificate PEM file (enables HTTPS)")
	tlsKey := flag.String("tls-key", "", "TLS key PEM file")
	verbose := flag.Bool("v", false, "verbose logging")
	flag.Parse()

	logger := log.New(os.Stderr, "[salcara-hub] ", log.LstdFlags)
	if !*verbose {
		logger.SetFlags(log.LstdFlags)
	}

	var apiBases []string
	if *bases != "" {
		for _, b := range strings.Split(*bases, ",") {
			if t := strings.TrimSpace(b); t != "" {
				apiBases = append(apiBases, t)
			}
		}
	}

	hub := hubserver.New(hubserver.Options{
		Logger:          logger,
		StationMode:     *mode,
		StationRevision: *revision,
		APIBases:        apiBases,
	})

	mux := http.NewServeMux()
	mux.Handle("/salcara-hub/", hub.Handler())
	mux.HandleFunc("POST /salcara-hub/v1/qr", hub.HandleQrToken)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"service":"salcara-hub","devices":%d,"phones":%d}`, hub.DeviceCount(), hub.PhoneCount())
	})

	srv := &http.Server{
		Addr:         *addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // SSE streams need no write timeout
		IdleTimeout:  120 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go hub.RunGC(ctx)

	go func() {
		var err error
		if *tlsCert != "" && *tlsKey != "" {
			logger.Printf("listening on %s (HTTPS)", *addr)
			err = srv.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			logger.Printf("listening on %s (HTTP — use a TLS reverse proxy for production)", *addr)
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			logger.Fatalf("serve: %v", err)
		}
	}()

	<-ctx.Done()
	logger.Printf("shutting down…")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
	logger.Printf("done")
}
