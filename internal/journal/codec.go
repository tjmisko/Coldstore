package journal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strconv"
)

const (
	// maxLineBytes bounds one journal line (without its newline). A longer line
	// is damage, never an allocation.
	maxLineBytes = 1 << 20

	crcKey = `,"crc":"`
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// appendLine appends the encoded line for rec, newline included, to dst.
func appendLine(dst []byte, rec Record) ([]byte, error) {
	rec.At = rec.At.UTC()
	body, err := json.Marshal(rec)
	if err != nil {
		return dst, fmt.Errorf("journal: encode record: %w", err)
	}
	if len(body) < 2 || body[len(body)-1] != '}' {
		return dst, errors.New("journal: encoded record is not a JSON object")
	}
	body = body[:len(body)-1]
	if len(body)+len(crcKey)+10 > maxLineBytes {
		return dst, fmt.Errorf("journal: record of %d bytes exceeds the %d byte line limit", len(body), maxLineBytes)
	}
	dst = append(dst, body...)
	dst = append(dst, crcKey...)
	dst = appendCRCHex(dst, crc32.Checksum(body, castagnoli))
	return append(dst, '"', '}', '\n'), nil
}

func appendCRCHex(dst []byte, sum uint32) []byte {
	hex := strconv.FormatUint(uint64(sum), 16)
	for i := len(hex); i < 8; i++ {
		dst = append(dst, '0')
	}
	return append(dst, hex...)
}

// decodeError is why a line did not decode; Reason is a DamagedError reason.
type decodeError struct {
	Reason string
	Err    error
}

func (e *decodeError) Error() string { return e.Reason + ": " + e.Err.Error() }
func (e *decodeError) Unwrap() error { return e.Err }

// decodeLine verifies the checksum of line (no trailing newline) and decodes it.
func decodeLine(line []byte) (Record, error) {
	idx := bytes.LastIndex(line, []byte(crcKey))
	if idx < 0 || idx+len(crcKey)+8+2 != len(line) || line[len(line)-2] != '"' || line[len(line)-1] != '}' {
		return Record{}, &decodeError{"checksum", errors.New("no crc trailer")}
	}
	want, err := strconv.ParseUint(string(line[idx+len(crcKey):idx+len(crcKey)+8]), 16, 32)
	if err != nil {
		return Record{}, &decodeError{"checksum", err}
	}
	body := line[:idx]
	if got := crc32.Checksum(body, castagnoli); uint64(got) != want {
		return Record{}, &decodeError{"checksum", fmt.Errorf("crc %08x, line says %08x", got, want)}
	}
	var rec Record
	if err := json.Unmarshal(append(body[:len(body):len(body)], '}'), &rec); err != nil {
		return Record{}, &decodeError{"json", err}
	}
	if err := rec.Validate(); err != nil {
		if errors.Is(err, ErrUnknownType) {
			return Record{}, &decodeError{"unknown_type", err}
		}
		return Record{}, &decodeError{"json", err}
	}
	return rec, nil
}

// lineReader reads newline-terminated lines without ever holding more than
// maxLineBytes of one.
type lineReader struct {
	br     *bufio.Reader
	buf    []byte
	offset int64 // bytes consumed so far
}

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{br: bufio.NewReaderSize(r, 64<<10)}
}

// next returns the next line without its newline. complete is false when the
// file ended before a newline. tooLong is true when the line exceeded
// maxLineBytes; line is then nil and the rest of it was discarded. size is the
// number of bytes the line occupies on disk. At end of file it returns io.EOF.
func (lr *lineReader) next() (line []byte, complete, tooLong bool, size int64, err error) {
	lr.buf = lr.buf[:0]
	for {
		chunk, rerr := lr.br.ReadSlice('\n')
		size += int64(len(chunk))
		if !tooLong {
			lr.buf = append(lr.buf, chunk...)
			if len(lr.buf) > maxLineBytes+1 {
				tooLong = true
				lr.buf = lr.buf[:0]
			}
		}
		if errors.Is(rerr, bufio.ErrBufferFull) {
			continue
		}
		lr.offset += size
		if rerr == nil {
			if tooLong {
				return nil, true, true, size, nil
			}
			return lr.buf[:len(lr.buf)-1], true, false, size, nil
		}
		if rerr != io.EOF {
			return nil, false, false, size, rerr
		}
		if size == 0 {
			return nil, false, false, 0, io.EOF
		}
		if tooLong {
			return nil, false, true, size, nil
		}
		return lr.buf, false, false, size, nil
	}
}
