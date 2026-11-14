package journal

import (
	"errors"
	"hash/crc32"
	"strings"
	"testing"
	"time"
)

const goodHash = "abcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcdabcd"

func ptr[T any](v T) *T { return &v }

// everyRecordType returns one valid record per Type.
func everyRecordType() []Record {
	mtime := time.Date(2019, 6, 2, 10, 11, 12, 0, time.UTC)
	return []Record{
		{By: "import", Type: TypeFolderDeclared, Folder: "Photos", Kind: FolderKindCollection},
		{By: "import", Type: TypeBlobAdded, Hash: goodHash, Size: 4211034},
		{By: "import", Type: TypeItemCreated, Item: "01JCX", Hash: goodHash},
		{By: "import", Type: TypeItemVersioned, Item: "01JCX", Hash: goodHash},
		{By: "import", Type: TypePlacementSet, Folder: "Photos", Path: "2019/trip/IMG_1.HEIC", Item: "01JCX", Mtime: &mtime, Asked: "2019/trip/IMG_1.HEIC"},
		{By: "import", Type: TypePlacementCleared, Folder: "Photos", Path: "2019/trip/IMG_1.HEIC"},
		{By: "jen@x", Type: TypePlacementPinned, Folder: "Photos", Path: "a", Pinned: ptr(true)},
		{By: "jen@x", Type: TypeItemTagged, Item: "01JCX", Add: []string{"tokyo"}, Remove: []string{"osaka"}},
		{By: "jen@x", Type: TypeItemDescribed, Item: "01JCX", Text: ptr("a \"quoted\" <text> & more\n")},
		{By: "tjmisko@x", Type: TypeItemTrashed, Item: "01JCX"},
		{By: "tjmisko@x", Type: TypeItemUntrashed, Item: "01JCX"},
	}
}

func TestRecordValidate(t *testing.T) {
	t.Run("should reject a record when its type is unknown", func(t *testing.T) {
		err := Record{By: "x", Type: "item_exploded"}.Validate()
		if !errors.Is(err, ErrUnknownType) {
			t.Fatalf("got %v, want ErrUnknownType", err)
		}
	})
	t.Run("should reject blob_added when the hash is not 64 hex characters", func(t *testing.T) {
		for _, hash := range []string{"", "abcd", strings.ToUpper(goodHash), goodHash + "0", strings.Repeat("g", 64)} {
			if err := (Record{By: "x", Type: TypeBlobAdded, Hash: hash}).Validate(); err == nil {
				t.Errorf("hash %q accepted", hash)
			}
		}
	})
	t.Run("should reject a record when by is empty", func(t *testing.T) {
		rec := everyRecordType()[0]
		rec.By = ""
		if err := rec.Validate(); err == nil {
			t.Fatal("empty by accepted")
		}
	})
	t.Run("should reject each record type when a required field is missing", func(t *testing.T) {
		for _, rec := range everyRecordType() {
			if err := rec.Validate(); err != nil {
				t.Fatalf("%s: valid record rejected: %v", rec.Type, err)
			}
		}
		broken := []Record{
			{By: "x", Type: TypeFolderDeclared, Folder: "F", Kind: "other"},
			{By: "x", Type: TypeFolderDeclared, Kind: FolderKindSync},
			{By: "x", Type: TypeItemCreated, Hash: goodHash},
			{By: "x", Type: TypePlacementSet, Folder: "F", Path: "p"},
			{By: "x", Type: TypePlacementCleared, Folder: "F"},
			{By: "x", Type: TypePlacementPinned, Folder: "F", Path: "p"},
			{By: "x", Type: TypeItemTagged, Item: "i"},
			{By: "x", Type: TypeItemDescribed, Item: "i"},
			{By: "x", Type: TypeItemTrashed},
		}
		for _, rec := range broken {
			if err := rec.Validate(); err == nil {
				t.Errorf("%s with missing field accepted: %+v", rec.Type, rec)
			}
		}
	})
}

func TestCodec(t *testing.T) {
	t.Run("should decode the example lines of the design when each is given a checksum", func(t *testing.T) {
		examples := []string{
			`{"seq":9119,"at":"2026-11-13T22:01:07Z","by":"import","t":"folder_declared","folder":"Photos","kind":"collection"`,
			`{"seq":9120,"at":"2026-11-13T22:01:08Z","by":"import","t":"blob_added","hash":"` + goodHash + `","size":4211034`,
			`{"seq":9121,"at":"2026-11-13T22:01:08Z","by":"import","t":"item_created","item":"01JCX","hash":"` + goodHash + `"`,
			`{"seq":9122,"at":"2026-11-13T22:01:09Z","by":"import","t":"placement_set","folder":"Photos","path":"2019/trip/IMG_1.HEIC","item":"01JCX","mtime":"2019-06-02T10:11:12Z"`,
			`{"seq":9140,"at":"2026-11-13T22:01:10Z","by":"jen@x","t":"item_tagged","item":"01JCX","add":["tokyo"]`,
			`{"seq":9200,"at":"2026-11-13T22:01:11Z","by":"tjmisko@x","t":"item_trashed","item":"01JCX"`,
		}
		for _, body := range examples {
			line := append([]byte(body), crcKey...)
			line = appendCRCHex(line, crcOf([]byte(body)))
			line = append(line, '"', '}')
			rec, err := decodeLine(line)
			if err != nil {
				t.Fatalf("%s: %v", body, err)
			}
			if rec.Seq == 0 || rec.Type == "" {
				t.Fatalf("decoded to %+v", rec)
			}
		}
	})
	t.Run("should round-trip every record type through encode and decode", func(t *testing.T) {
		for i, rec := range everyRecordType() {
			rec.Seq, rec.Batch = uint64(i+1), uint64(i+1)
			rec.At = time.Date(2026, 11, 13, 22, 1, 7, 123456789, time.UTC)
			line, err := appendLine(nil, rec)
			if err != nil {
				t.Fatalf("%s: encode: %v", rec.Type, err)
			}
			if line[len(line)-1] != '\n' || strings.Count(string(line), "\n") != 1 {
				t.Fatalf("%s: line is not exactly one newline-terminated line: %q", rec.Type, line)
			}
			got, err := decodeLine(line[:len(line)-1])
			if err != nil {
				t.Fatalf("%s: decode: %v", rec.Type, err)
			}
			again, err := appendLine(nil, got)
			if err != nil || string(again) != string(line) {
				t.Fatalf("%s: re-encoding differs:\n%s\n%s (%v)", rec.Type, line, again, err)
			}
		}
	})
	t.Run("should detect a flipped byte in a line when the checksum is verified", func(t *testing.T) {
		rec := everyRecordType()[4]
		rec.Seq, rec.Batch, rec.At = 7, 7, time.Unix(1700000000, 0)
		line, err := appendLine(nil, rec)
		if err != nil {
			t.Fatal(err)
		}
		line = line[:len(line)-1]
		for i := range line {
			damaged := append([]byte(nil), line...)
			damaged[i] ^= 0x01
			if _, err := decodeLine(damaged); err == nil {
				t.Fatalf("flip at byte %d went undetected", i)
			}
		}
	})
	t.Run("should report unknown_type when a checksummed line has a type from the future", func(t *testing.T) {
		body := `{"seq":1,"at":"2026-11-13T22:01:07Z","by":"x","t":"item_purged","batch":1`
		line := append([]byte(body), crcKey...)
		line = appendCRCHex(line, crcOf([]byte(body)))
		line = append(line, '"', '}')
		_, err := decodeLine(line)
		var de *decodeError
		if !errors.As(err, &de) || de.Reason != "unknown_type" {
			t.Fatalf("got %v, want unknown_type", err)
		}
	})
	t.Run("should refuse to encode a record when its line would exceed the limit", func(t *testing.T) {
		rec := Record{By: "x", Type: TypeItemDescribed, Item: "i", Text: ptr(strings.Repeat("a", maxLineBytes))}
		if _, err := appendLine(nil, rec); err == nil {
			t.Fatal("oversized record encoded")
		}
	})
}

func crcOf(b []byte) uint32 { return crc32.Checksum(b, castagnoli) }
