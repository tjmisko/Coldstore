package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Hash is the SHA-256 of a blob's bytes: its identity everywhere in the archive.
type Hash [sha256.Size]byte

const hashHexLen = sha256.Size * 2

// ParseHash accepts exactly 64 lowercase hex characters. Uppercase is refused
// rather than normalised so one blob has one spelling in the journal, the
// index and every URL.
func ParseHash(hexStr string) (Hash, error) {
	var h Hash
	if len(hexStr) != hashHexLen {
		return h, fmt.Errorf("parsing hash: want %d hex characters, got %d", hashHexLen, len(hexStr))
	}
	for i := 0; i < len(hexStr); i++ {
		c := hexStr[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return h, fmt.Errorf("parsing hash: character %q at offset %d is not lowercase hex", c, i)
		}
	}
	if _, err := hex.Decode(h[:], []byte(hexStr)); err != nil {
		return Hash{}, fmt.Errorf("parsing hash: %w", err)
	}
	return h, nil
}

// String is 64 lowercase hex characters.
func (h Hash) String() string { return hex.EncodeToString(h[:]) }

func (h Hash) MarshalText() ([]byte, error) { return []byte(h.String()), nil }

func (h *Hash) UnmarshalText(b []byte) error {
	parsed, err := ParseHash(string(b))
	if err != nil {
		return err
	}
	*h = parsed
	return nil
}

// RelPath is "sha256/ab/cd/abcd...ef": shared by blobs/ and thumbnails/.
func (h Hash) RelPath() string {
	s := h.String()
	return "sha256/" + s[0:2] + "/" + s[2:4] + "/" + s
}
