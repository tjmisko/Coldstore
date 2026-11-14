package store

import (
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
)

const sampleHashHex = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" // sha256("abc")

func TestHash(t *testing.T) {
	t.Run("should round-trip through String and ParseHash when the input is valid", func(t *testing.T) {
		h, err := ParseHash(sampleHashHex)
		if err != nil {
			t.Fatal(err)
		}
		if h != Hash(sha256.Sum256([]byte("abc"))) {
			t.Fatal("parsed bytes differ from sha256(abc)")
		}
		if h.String() != sampleHashHex {
			t.Fatalf("String = %q", h.String())
		}
	})

	t.Run("should round-trip through JSON when used as a field and as a map key", func(t *testing.T) {
		h, _ := ParseHash(sampleHashHex)
		raw, err := json.Marshal(map[Hash]Hash{h: h})
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"` + sampleHashHex + `":"` + sampleHashHex + `"}`; string(raw) != want {
			t.Fatalf("json = %s", raw)
		}
		var back map[Hash]Hash
		if err := json.Unmarshal(raw, &back); err != nil || back[h] != h {
			t.Fatalf("Unmarshal = %v, %v", back, err)
		}
	})

	t.Run("should reject a hash when it is 63 characters", func(t *testing.T) {
		if _, err := ParseHash(sampleHashHex[:63]); err == nil {
			t.Fatal("accepted 63 characters")
		}
	})

	t.Run("should reject a hash when it is 65 characters", func(t *testing.T) {
		if _, err := ParseHash(sampleHashHex + "0"); err == nil {
			t.Fatal("accepted 65 characters")
		}
	})

	t.Run("should reject a hash when it is empty", func(t *testing.T) {
		if _, err := ParseHash(""); err == nil {
			t.Fatal("accepted the empty string")
		}
	})

	t.Run("should reject a hash when it contains uppercase hex", func(t *testing.T) {
		if _, err := ParseHash(strings.ToUpper(sampleHashHex)); err == nil {
			t.Fatal("accepted uppercase")
		}
	})

	t.Run("should reject a hash when it contains a non-hex character", func(t *testing.T) {
		for _, bad := range []string{"g", "-", " ", "\x00", "é"} {
			in := bad + sampleHashHex[len(bad):]
			if len(in) != 64 {
				in = bad + sampleHashHex[:64-len(bad)]
			}
			if _, err := ParseHash(in); err == nil {
				t.Errorf("accepted %q", in)
			}
		}
	})

	t.Run("should leave the receiver unchanged when UnmarshalText fails", func(t *testing.T) {
		h, _ := ParseHash(sampleHashHex)
		before := h
		if err := h.UnmarshalText([]byte("nope")); err == nil {
			t.Fatal("accepted garbage")
		}
		if h != before {
			t.Fatal("receiver mutated on error")
		}
	})

	t.Run("should produce sha256/ab/cd/<hash> when RelPath is called", func(t *testing.T) {
		h, _ := ParseHash(sampleHashHex)
		if want := "sha256/ba/78/" + sampleHashHex; h.RelPath() != want {
			t.Fatalf("RelPath = %q, want %q", h.RelPath(), want)
		}
	})
}
