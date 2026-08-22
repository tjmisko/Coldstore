package diagnostic

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

func inspectBtrfs(ctx context.Context, path string) BtrfsReport {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "btrfs", "filesystem", "show", "--raw", path).CombinedOutput()
	return classifyBtrfsOutput(output, err)
}

func classifyBtrfsOutput(output []byte, commandErr error) BtrfsReport {
	lower := strings.ToLower(string(output))
	if strings.Contains(lower, "some devices missing") ||
		strings.Contains(lower, "path missing") || strings.Contains(lower, "devid missing") {
		return BtrfsReport{Status: "degraded", Detail: "one or more filesystem devices are missing"}
	}
	if commandErr != nil {
		return BtrfsReport{Status: "unknown", Detail: "btrfs filesystem show was unavailable or not permitted"}
	}
	return BtrfsReport{Status: "healthy", Detail: "btrfs filesystem show reports no missing devices"}
}
