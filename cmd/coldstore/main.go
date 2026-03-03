package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/tjmisko/coldstore/internal/api"
	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
	"github.com/tjmisko/coldstore/internal/daemon"
)

func main() {
	configPath := flag.String("config", "coldstore.toml", "path to config file")
	scanOnce := flag.Bool("scan", false, "run a single catalog scan and exit")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	dbPath := cfg.Storage.ColdStoragePath + "/catalog.db"
	cat, err := catalog.Open(dbPath)
	if err != nil {
		logger.Error("failed to open catalog", "error", err)
		os.Exit(1)
	}
	defer cat.Close()

	if *scanOnce {
		scanner := daemon.NewScanner(cfg, cat, logger)
		if err := scanner.ScanAll(); err != nil {
			logger.Error("scan failed", "error", err)
			os.Exit(1)
		}
		logger.Info("scan complete")
		return
	}

	restorer := daemon.NewRestorer(cfg, cat, logger)
	scanner := daemon.NewScanner(cfg, cat, logger)

	// Start API server in background
	srv := api.NewServer(cfg, cat, restorer, scanner, logger)
	go func() {
		if err := srv.ListenAndServe(cfg.Server.ListenAddr); err != nil {
			logger.Error("API server error", "error", err)
		}
	}()

	logger.Info("coldstore daemon starting", "config", *configPath)
	d := daemon.New(cfg, cat, logger)
	if err := d.Run(); err != nil {
		logger.Error("daemon exited with error", "error", err)
		os.Exit(1)
	}
}
