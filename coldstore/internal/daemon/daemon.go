package daemon

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
)

type Daemon struct {
	cfg     *config.Config
	cat     *catalog.Catalog
	logger  *slog.Logger
	scanner *Scanner
}

func New(cfg *config.Config, cat *catalog.Catalog, logger *slog.Logger) *Daemon {
	return &Daemon{
		cfg:     cfg,
		cat:     cat,
		logger:  logger,
		scanner: NewScanner(cfg, cat, logger),
	}
}

func (d *Daemon) Run() error {
	d.logger.Info("running initial catalog scan")
	if err := d.scanner.ScanAll(); err != nil {
		d.logger.Error("initial scan failed", "error", err)
	}

	scanTicker := time.NewTicker(time.Duration(d.cfg.Daemon.ScanIntervalMinutes) * time.Minute)
	defer scanTicker.Stop()

	catalogTicker := time.NewTicker(time.Duration(d.cfg.Daemon.CatalogSyncIntervalMinutes) * time.Minute)
	defer catalogTicker.Stop()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case <-scanTicker.C:
			d.logger.Info("quota enforcement tick")
			// Phase 2: quota enforcement runs here
		case <-catalogTicker.C:
			d.logger.Info("catalog sync tick")
			if err := d.scanner.ScanAll(); err != nil {
				d.logger.Error("catalog sync scan failed", "error", err)
			}
		case sig := <-sigCh:
			d.logger.Info("received signal, shutting down", "signal", sig)
			return nil
		}
	}
}
