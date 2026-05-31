// Package main is the bootstrap entry point for the GhostProxy application.
// It initializes configuration from a YAML file, spins up the storage
// backend, constructs the reverse proxy engine, and starts the HTTP server
// with graceful shutdown support via OS signal trapping.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/developer/GhostProxy/pkg/config"
	"github.com/developer/GhostProxy/pkg/proxy"
	"github.com/developer/GhostProxy/pkg/storage"
)

const (
	banner = `
   ██████╗ ██╗  ██╗ ██████╗ ███████╗████████╗
  ██╔════╝ ██║  ██║██╔═══██╗██╔════╝╚══██╔══╝
  ██║  ███╗███████║██║   ██║███████╗   ██║   
  ██║   ██║██╔══██║██║   ██║╚════██║   ██║   
  ╚██████╔╝██║  ██║╚██████╔╝███████║   ██║   
   ╚═════╝ ╚═╝  ╚═╝ ╚═════╝ ╚══════╝   ╚═╝   
  ─────────────────────────────────────────────
  Intelligent Traffic Proxy · v1.0.0
  ─────────────────────────────────────────────`
)

func main() {
	// ── CLI Flag Parsing ─────────────────────────────────────────────
	configPath := flag.String("config", "config.yaml", "Path to the GhostProxy YAML configuration file")
	modeOverride := flag.String("mode", "", "Override operational mode (record|replay|chaos)")
	flag.Parse()

	fmt.Println(banner)
	fmt.Println()

	logger := log.New(os.Stdout, "[GhostProxy] ", log.LstdFlags|log.Lmsgprefix)

	// ── Configuration Loading ────────────────────────────────────────
	logger.Printf("Loading configuration from: %s", *configPath)

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		logger.Fatalf("FATAL: Configuration error: %v", err)
	}

	// Apply CLI mode override if specified
	if *modeOverride != "" {
		overrideMode := config.OperationalMode(*modeOverride)
		switch overrideMode {
		case config.ModeRecord, config.ModeReplay, config.ModeChaos:
			cfg.Mode = overrideMode
			logger.Printf("Mode overridden via CLI flag: %s", cfg.Mode)
		default:
			logger.Fatalf("FATAL: Invalid mode override %q — must be record, replay, or chaos", *modeOverride)
		}
	}

	logger.Printf("Operational mode: %s", cfg.Mode)
	logger.Printf("Upstream target:  %s", cfg.Upstream.Target)
	logger.Printf("Storage directory: %s", cfg.Storage.Directory)
	logger.Printf("Registered routes: %d", len(cfg.Routes))

	// ── Storage Initialization ───────────────────────────────────────
	store, err := storage.NewDiskStore(cfg.Storage.Directory)
	if err != nil {
		logger.Fatalf("FATAL: Storage initialization failed: %v", err)
	}
	logger.Printf("Storage backend initialized at: %s", cfg.Storage.Directory)

	// ── Proxy Engine Construction ────────────────────────────────────
	handler, err := proxy.NewGhostProxy(cfg, store)
	if err != nil {
		logger.Fatalf("FATAL: Proxy engine construction failed: %v", err)
	}

	// ── HTTP Server Setup ────────────────────────────────────────────
	server := &http.Server{
		Addr:              cfg.ListenAddr(),
		Handler:           handler,
		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      cfg.Upstream.Timeout() + 10*time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MB
	}

	// ── Graceful Shutdown Signal Trap ────────────────────────────────
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	// Start the HTTP server in a separate goroutine
	serverErrors := make(chan error, 1)
	go func() {
		logger.Printf("GhostProxy listening on http://%s", cfg.ListenAddr())
		logger.Printf("   Status endpoint: http://%s/__ghostproxy/status", cfg.ListenAddr())
		fmt.Println()
		serverErrors <- server.ListenAndServe()
	}()

	// ── Block Until Shutdown Signal or Server Error ──────────────────
	select {
	case err := <-serverErrors:
		if err != nil && err != http.ErrServerClosed {
			logger.Fatalf("FATAL: HTTP server error: %v", err)
		}

	case sig := <-shutdown:
		logger.Printf("Shutdown signal received: %v", sig)
		logger.Println("Initiating graceful shutdown (10s deadline)...")

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := server.Shutdown(ctx); err != nil {
			logger.Printf("WARNING: Graceful shutdown failed: %v — forcing close", err)
			if closeErr := server.Close(); closeErr != nil {
				logger.Fatalf("FATAL: Forced close failed: %v", closeErr)
			}
		}

		logger.Println("GhostProxy shut down gracefully. Goodbye! ")
	}
}
