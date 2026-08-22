package metadata

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ExifToolProbe keeps one exiftool -stay_open process alive and serializes
// candidate probes through it. ExifTool remains a separate executable; no GPL
// Go package is imported into the coldstore binary.
type ExifToolProbe struct {
	gate    chan struct{}
	binary  string
	timeout time.Duration
	nextID  uint64

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *limitedBuffer
}

// NewExifToolProbe constructs a lazy resident ExifTool client.
func NewExifToolProbe(binary string, timeout time.Duration) *ExifToolProbe {
	if binary == "" {
		binary = "exiftool"
	}
	if timeout <= 0 {
		timeout = defaultToolTimeout
	}
	return &ExifToolProbe{gate: make(chan struct{}, 1), binary: binary, timeout: timeout}
}

var exifToolArguments = []string{
	"-j",
	"-n",
	"-DateTimeOriginal",
	"-SubSecTimeOriginal",
	"-OffsetTimeOriginal",
	"-CreateDate",
	"-SubSecTimeDigitized",
	"-OffsetTimeDigitized",
	"-GPSLatitude",
	"-GPSLongitude",
	"-GPSAltitude",
	"-GPSAltitudeRef",
	"-GPSHPositioningError",
	"-GPSDateStamp",
	"-GPSTimeStamp",
	"-ContentIdentifier",
}

type exifToolResponse struct {
	SourceFile           string      `json:"SourceFile"`
	DateTimeOriginal     string      `json:"DateTimeOriginal"`
	SubSecTimeOriginal   any         `json:"SubSecTimeOriginal"`
	OffsetTimeOriginal   string      `json:"OffsetTimeOriginal"`
	CreateDate           string      `json:"CreateDate"`
	SubSecTimeDigitized  any         `json:"SubSecTimeDigitized"`
	OffsetTimeDigitized  string      `json:"OffsetTimeDigitized"`
	GPSLatitude          *float64    `json:"GPSLatitude"`
	GPSLongitude         *float64    `json:"GPSLongitude"`
	GPSAltitude          *float64    `json:"GPSAltitude"`
	GPSAltitudeRef       json.Number `json:"GPSAltitudeRef"`
	GPSHPositioningError *float64    `json:"GPSHPositioningError"`
	GPSDateStamp         string      `json:"GPSDateStamp"`
	GPSTimeStamp         string      `json:"GPSTimeStamp"`
	ContentIdentifier    string      `json:"ContentIdentifier"`
	Error                string      `json:"Error"`
}

type residentResult struct {
	data []byte
	err  error
}

// Probe extracts a still candidate's ContentIdentifier and normalized capture
// metadata. Calls are safe from multiple goroutines; one request is in flight
// in the resident process at a time.
func (p *ExifToolProbe) Probe(ctx context.Context, path string) (Extracted, error) {
	if err := ctx.Err(); err != nil {
		return Extracted{}, fmt.Errorf("metadata: exiftool cancelled: %w", err)
	}
	if strings.ContainsAny(path, "\r\n") {
		return Extracted{}, fmt.Errorf("metadata: exiftool argfile cannot represent a path containing a newline")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Extracted{}, fmt.Errorf("metadata: resolve ExifTool path %s: %w", path, err)
	}

	cctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	select {
	case p.gate <- struct{}{}:
		defer func() { <-p.gate }()
	case <-cctx.Done():
		return Extracted{}, exifToolContextError(ctx, cctx, p.timeout)
	}
	if err := cctx.Err(); err != nil {
		return Extracted{}, exifToolContextError(ctx, cctx, p.timeout)
	}
	if err := p.startLocked(); err != nil {
		return Extracted{}, err
	}

	p.nextID++
	id := strconv.FormatUint(p.nextID, 10)
	request := strings.Join(exifToolArguments, "\n") + "\n" + absolute + "\n-execute" + id + "\n"
	if _, err := io.WriteString(p.stdin, request); err != nil {
		_ = p.stopLocked(false)
		return Extracted{}, fmt.Errorf("metadata: write ExifTool request: %w", err)
	}

	result := make(chan residentResult, 1)
	reader := p.stdout
	go func() {
		data, err := readExifToolResponse(reader, id)
		result <- residentResult{data: data, err: err}
	}()

	select {
	case response := <-result:
		if response.err != nil {
			_ = p.stopLocked(false)
			return Extracted{}, response.err
		}
		return decodeExifToolResponse(response.data)
	case <-cctx.Done():
		_ = p.stopLocked(false)
		return Extracted{}, exifToolContextError(ctx, cctx, p.timeout)
	}
}

// Close gracefully shuts down the resident process.
func (p *ExifToolProbe) Close() error {
	p.gate <- struct{}{}
	defer func() { <-p.gate }()
	return p.stopLocked(true)
}

func exifToolContextError(ctx, invocation context.Context, timeout time.Duration) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return fmt.Errorf("metadata: exiftool caller deadline exceeded: %w", context.DeadlineExceeded)
		}
		return fmt.Errorf("metadata: exiftool cancelled: %w", context.Canceled)
	}
	if errors.Is(invocation.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("metadata: exiftool timed out after %s: %w", timeout, context.DeadlineExceeded)
	}
	return fmt.Errorf("metadata: exiftool cancelled: %w", context.Canceled)
}

func (p *ExifToolProbe) startLocked() error {
	if p.cmd != nil {
		return nil
	}
	cmd := exec.Command(p.binary, "-stay_open", "True", "-@", "-")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = processWaitDelay
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("metadata: create ExifTool stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("metadata: create ExifTool stdout: %w", err)
	}
	stderr := &limitedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return fmt.Errorf("metadata: start %s: %w", p.binary, err)
	}
	p.cmd = cmd
	p.stdin = stdin
	p.stdout = bufio.NewReader(stdout)
	p.stderr = stderr
	return nil
}

func (p *ExifToolProbe) stopLocked(graceful bool) error {
	if p.cmd == nil {
		return nil
	}
	cmd := p.cmd
	stdin := p.stdin
	stderr := p.stderr
	p.cmd = nil
	p.stdin = nil
	p.stdout = nil
	p.stderr = nil

	if graceful {
		_, _ = io.WriteString(stdin, "-stay_open\nFalse\n")
		_ = stdin.Close()
	} else {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = stdin.Close()
	}

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		if err != nil && graceful {
			if diagnostic := stderr.diagnostic(); diagnostic != "" {
				return fmt.Errorf("metadata: stop ExifTool: %w: %s", err, diagnostic)
			}
			return fmt.Errorf("metadata: stop ExifTool: %w", err)
		}
		return nil
	case <-time.After(processWaitDelay):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-waited
		if graceful {
			return fmt.Errorf("metadata: ExifTool did not stop within %s", processWaitDelay)
		}
		return nil
	}
}

func readExifToolResponse(reader *bufio.Reader, id string) ([]byte, error) {
	marker := "{ready" + id + "}"
	var response strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if strings.TrimSpace(line) == marker {
			return []byte(response.String()), nil
		}
		response.WriteString(line)
		if err != nil {
			return nil, fmt.Errorf("metadata: read ExifTool response: %w", err)
		}
	}
}

func decodeExifToolResponse(data []byte) (Extracted, error) {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	var records []exifToolResponse
	if err := decoder.Decode(&records); err != nil {
		return Extracted{}, fmt.Errorf("metadata: decode ExifTool JSON: %w", err)
	}
	if len(records) != 1 {
		return Extracted{}, fmt.Errorf("metadata: ExifTool returned %d records, want 1", len(records))
	}
	record := records[0]
	if record.Error != "" {
		return Extracted{}, fmt.Errorf("metadata: ExifTool: %s", record.Error)
	}
	originalSubsecond, err := subsecondFromAny(record.SubSecTimeOriginal)
	if err != nil {
		return Extracted{}, err
	}
	digitizedSubsecond, err := subsecondFromAny(record.SubSecTimeDigitized)
	if err != nil {
		return Extracted{}, err
	}
	gpsUTC := parseExifToolGPSTime(record.GPSDateStamp, record.GPSTimeStamp)
	original, err := parseEXIFCapture(record.DateTimeOriginal, originalSubsecond, record.OffsetTimeOriginal, "exif", gpsUTC)
	if err != nil {
		return Extracted{}, err
	}
	digitized, err := parseEXIFCapture(record.CreateDate, digitizedSubsecond, record.OffsetTimeDigitized, "exif_digitized", gpsUTC)
	if err != nil {
		return Extracted{}, err
	}
	capture := chooseCapture(original, captureCandidate{}, digitized, captureCandidate{})
	result := Extracted{
		CapturedLocal:  capture.local,
		CapturedOffset: capture.offset,
		CapturedSource: capture.source,
		Lat:            record.GPSLatitude,
		Lon:            record.GPSLongitude,
		AltitudeM:      record.GPSAltitude,
		AccuracyM:      record.GPSHPositioningError,
		ContentID:      strings.TrimSpace(record.ContentIdentifier),
	}
	if result.Lat != nil && result.Lon != nil {
		result.GPSSource = "exif"
	} else {
		result.Lat = nil
		result.Lon = nil
	}
	if result.AltitudeM != nil && record.GPSAltitudeRef != "" {
		ref, err := record.GPSAltitudeRef.Int64()
		if err == nil && ref == 1 {
			altitude := -math.Abs(*result.AltitudeM)
			result.AltitudeM = &altitude
		}
	}
	return result, nil
}

func parseExifToolGPSTime(date, clock string) time.Time {
	if date == "" || clock == "" {
		return time.Time{}
	}
	parsed, err := time.ParseInLocation("2006:01:02 15:04:05", date+" "+clock, time.UTC)
	if err != nil {
		return time.Time{}
	}
	return parsed
}
