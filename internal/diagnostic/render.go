package diagnostic

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tjmisko/coldstore/internal/catalog"
)

func WriteJSON(w io.Writer, report Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

func WriteHuman(w io.Writer, r Report) error {
	var writeErr error
	line := func(format string, args ...any) {
		if writeErr == nil {
			_, writeErr = fmt.Fprintf(w, format+"\n", args...)
		}
	}
	line("coldstore status: %s", strings.ToUpper(r.Status))
	line("build             %s", r.BuildVersion)
	line("daemon            %s (offline report; HTTP is not required)", r.DaemonStatus)
	if r.Catalog.Exists {
		line("catalog schema    on disk %d / supported %d", r.Catalog.OnDiskSchema, r.Catalog.SupportedSchema)
	} else {
		line("catalog schema    catalog missing / supported %d", r.Catalog.SupportedSchema)
	}
	line("cold storage      %s", r.Storage.Path)
	if r.Storage.Exists {
		mount := "plain directory"
		if r.Storage.IsMountPoint {
			mount = "mount point"
		}
		if r.Storage.FSType != "" {
			mount += ", " + r.Storage.FSType
		}
		line("storage state     %s; %s free (%.1f%%)", mount, catalog.HumanSI(int64(r.Storage.FreeBytes)), r.Storage.FreePercent)
		line("btrfs membership  %s%s", r.Storage.Btrfs.Status, detailSuffix(r.Storage.Btrfs.Detail))
	} else {
		line("storage state     unavailable")
	}
	line("files             %d total: %d synced, %d archiving, %d archived, %d restoring, %d missing",
		r.Files.Total, r.Files.Synced, r.Files.Archiving, r.Files.Archived, r.Files.Restoring, r.Files.Missing)

	line("folders           %d", len(r.Folders))
	for _, folder := range r.Folders {
		state := "NOT READY: " + folder.Readiness
		if folder.Ready {
			state = "ready"
		}
		line("  %-18s %-24s %s / %s on device; %s pinned%s",
			folder.Label, state, catalog.HumanSI(folder.OnDeviceBytes), catalog.HumanSI(folder.BudgetBytes),
			catalog.HumanSI(folder.PinnedBytes), wedgeSuffix(folder))
	}
	line("jobs              %d active, %d failed (retained recent jobs)", len(r.ActiveJobs), len(r.FailedJobs))
	line("derived data      %d thumbnail failures; %d type-detection backlog; metadata unavailable",
		r.Derived.ThumbnailFailures, r.Derived.DetectionBacklog)
	line("last scan         %s", formatTime(r.LastScan))
	line("last offload      %s", formatTime(r.LastOffload))
	line("recent warnings   %d redacted event summaries", len(r.RecentEvents))

	if len(r.Findings) > 0 {
		line("findings")
		for _, finding := range r.Findings {
			line("  %-7s %-24s %s", strings.ToUpper(string(finding.Severity)), finding.Code, finding.Message)
		}
	}
	line("limitations")
	for _, limitation := range r.Limitations {
		line("  - %s", limitation)
	}
	return writeErr
}

func formatTime(at *time.Time) string {
	if at == nil {
		return "never recorded"
	}
	return at.UTC().Format(time.RFC3339)
}

func detailSuffix(detail string) string {
	if detail == "" {
		return ""
	}
	return " -- " + detail
}

func wedgeSuffix(folder FolderReport) string {
	if folder.Wedged {
		return "; WEDGED"
	}
	if folder.OverBudget {
		return "; over budget"
	}
	return ""
}
