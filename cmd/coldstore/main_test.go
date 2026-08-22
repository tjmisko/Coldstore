package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjmisko/coldstore/internal/catalog"
	"github.com/tjmisko/coldstore/internal/config"
)

func preflightCfg() *config.Config {
	return &config.Config{
		Storage: config.StorageConfig{ColdStoragePath: "/srv/coldstore"},
		Server:  config.ServerConfig{ListenAddr: "192.168.1.10:8090", Username: "coldstore", Password: "x"},
		Folders: []config.FolderConfig{
			{Label: "CameraRoll", Path: "/home/owner/CameraRoll", Budget: "20GB", BudgetBytes: 20_000_000_000},
			{Label: "Books", Path: "/home/owner/Books", Budget: "5GB", BudgetBytes: 5_000_000_000},
		},
	}
}

// --check runs outside the unit's sandbox, so it cannot see ProtectSystem=strict
// and will print "preflight OK" for a config that exits 1 the moment systemd
// starts it with a folder missing from ReadWritePaths=. The least it can do is
// hand the owner the line they need instead of staying silent about it.
func TestShouldPrintTheReadWritePathsLineWhenPreflightReports(t *testing.T) {
	var buf bytes.Buffer
	reportPreflight(&buf, preflightCfg(), "/etc/coldstore/coldstore.toml", nil)
	got := buf.String()

	want := "ReadWritePaths=/srv/coldstore /home/owner/CameraRoll /home/owner/Books"
	if !strings.Contains(got, want) {
		t.Errorf("preflight output does not contain %q:\n%s", want, got)
	}
}

func TestShouldSayPreflightCannotVerifyTheSandboxWhenPreflightReports(t *testing.T) {
	var buf bytes.Buffer
	reportPreflight(&buf, preflightCfg(), "/etc/coldstore/coldstore.toml", nil)
	got := buf.String()

	if !strings.Contains(got, "OUTSIDE the unit's sandbox") {
		t.Errorf("preflight claims coverage it does not have:\n%s", got)
	}
	// The old docs called a missing ReadWritePaths entry a runtime failure hours
	// later. It is a hard startup failure, and the report must not re-teach the
	// wrong model.
	if !strings.Contains(got, "exit 1 at startup") {
		t.Errorf("preflight does not say a missing entry is fatal at startup:\n%s", got)
	}
}

func TestShouldListEveryWarningWhenPreflightReportsWarnings(t *testing.T) {
	var buf bytes.Buffer
	reportPreflight(&buf, preflightCfg(), "/etc/coldstore/coldstore.toml",
		[]string{"folder \"Books\" path \"/home/owner/Books\" does not exist yet"})
	got := buf.String()

	if !strings.Contains(got, "preflight OK with 1 warning(s)") {
		t.Errorf("warning count missing:\n%s", got)
	}
	if !strings.Contains(got, "does not exist yet") {
		t.Errorf("the warning text itself is missing:\n%s", got)
	}
}

func TestShouldReportTailscaleAuthenticationWithoutPrintingLogins(t *testing.T) {
	cfg := preflightCfg()
	cfg.Server = config.ServerConfig{
		ListenAddr:            config.DefaultListenAddr,
		AuthMode:              config.AuthModeTailscale,
		TailscaleAllowedUsers: []string{"private-owner@example.com", "private-co-owner@example.com"},
	}
	var buf bytes.Buffer
	reportPreflight(&buf, cfg, "/etc/coldstore/coldstore.toml", nil)
	got := buf.String()

	if !strings.Contains(got, "auth             Tailscale Serve identity allowlist (2 users)") {
		t.Errorf("preflight did not report Tailscale auth mode:\n%s", got)
	}
	for _, private := range cfg.Server.TailscaleAllowedUsers {
		if strings.Contains(got, private) {
			t.Errorf("preflight leaked private login %q:\n%s", private, got)
		}
	}

	cfg.Server.TailscaleAllowedUsers = cfg.Server.TailscaleAllowedUsers[:1]
	buf.Reset()
	reportPreflight(&buf, cfg, "/etc/coldstore/coldstore.toml", nil)
	if got := buf.String(); !strings.Contains(got, "Tailscale Serve identity allowlist (1 user)") {
		t.Errorf("preflight did not use the singular user label:\n%s", got)
	}
}

func TestCheckShouldReportACataloguedPathUnderAnotherLabel(t *testing.T) {
	coldDir := t.TempDir()
	dbPath := filepath.Join(coldDir, "catalog.db")
	cat, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.ReconcileFolderFromTOML("Photos", "", "/srv/sync/Photos", 100, 30); err != nil {
		t.Fatal(err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Storage: config.StorageConfig{ColdStoragePath: coldDir},
		Folders: []config.FolderConfig{{
			Label: "Pictures", Path: "/srv/sync/Photos/", BudgetBytes: 100,
		}},
	}
	err = checkCatalogFolderIdentities(cfg, dbPath)
	if err == nil {
		t.Fatal("--check identity validation accepted a label rename")
	}
	if !strings.Contains(err.Error(), "already catalogued under label \"Photos\"") {
		t.Errorf("error = %q, want the catalog ownership conflict", err)
	}
}

func TestStatusFlagsShouldRemainAdditiveAndRejectAmbiguousModes(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		scan, check, status, json bool
		wantErr                   bool
	}{
		{"daemon", false, false, false, false, false},
		{"scan", true, false, false, false, false},
		{"check", false, true, false, false, false},
		{"status human", false, false, true, false, false},
		{"status json", false, false, true, true, false},
		{"json alone", false, false, false, true, true},
		{"scan and status", true, false, true, false, true},
		{"check and status", false, true, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateModeFlags(tc.scan, tc.check, tc.status, tc.json)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateModeFlags error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestWriteStatusShouldReturnFailureAndStructuredJSONForAMissingArchive(t *testing.T) {
	cold := filepath.Join(t.TempDir(), "missing")
	cfg := &config.Config{
		Storage: config.StorageConfig{ColdStoragePath: cold, RequireMountpoint: true},
		Folders: []config.FolderConfig{{Label: "Photos", Path: filepath.Join(t.TempDir(), "photos"), BudgetBytes: 100}},
	}
	var out bytes.Buffer
	code, err := writeStatus(context.Background(), &out, cfg, filepath.Join(cold, "catalog.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	if code != 2 {
		t.Errorf("exit code = %d, want 2 for a load-bearing failure", code)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("status JSON = %q: %v", out.String(), err)
	}
	if decoded["status"] != "failure" {
		t.Errorf("status = %v, want failure", decoded["status"])
	}
	if _, err := os.Stat(cold); !os.IsNotExist(err) {
		t.Errorf("status created missing archive path: %v", err)
	}
}
