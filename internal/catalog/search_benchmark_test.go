package catalog

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

const searchBenchmarkRows = 50_000

var (
	benchmarkSearchFiles []FileRecord
	benchmarkSearchIDs   []int64
	benchmarkSearchTotal int64
)

// BenchmarkSearchFilters50K deliberately uses the ordinary schema and write
// representation rather than a synthetic SELECT-only table. Paths vary by
// year/month/album, most rows have descriptions, and file_tags has realistic
// one-to-many cardinality. Run with a fixed iteration count so fixture setup
// is excluded without dominating adaptive benchmark calibration:
//
//	go test ./internal/catalog -run '^$' -bench '^BenchmarkSearchFilters50K$' -benchtime=5x -count=3
func BenchmarkSearchFilters50K(b *testing.B) {
	cat, err := Open(filepath.Join(b.TempDir(), "search-50k.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = cat.Close() })
	seedSearchBenchmark(b, cat)

	b.ReportMetric(searchBenchmarkRows, "catalog_rows")
	b.Run("q_hit_page", func(b *testing.B) {
		q := FileQuery{Search: "coastal travel journal", Limit: 60, SortBy: "name"}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			benchmarkSearchFiles, err = cat.ListFiles(q)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("q_miss_page", func(b *testing.B) {
		q := FileQuery{Search: "no-such-phrase-%_", Limit: 60}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			benchmarkSearchFiles, err = cat.ListFiles(q)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("two_tags_described_page", func(b *testing.B) {
		described := true
		q := FileQuery{Tags: []string{"Trips", "People"}, Described: &described, Limit: 60}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			benchmarkSearchFiles, err = cat.ListFiles(q)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("q_all_matching_ids", func(b *testing.B) {
		q := FileQuery{Search: "coastal travel journal"}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			benchmarkSearchIDs, benchmarkSearchTotal, err = cat.ListFileIDs(q, 20_000)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

func seedSearchBenchmark(b *testing.B, cat *Catalog) {
	b.Helper()
	tagNames := []string{
		"Trips/Iceland/Reykjavik", "Trips/Japan/Tokyo", "Trips/California/Coast",
		"People/Alice", "People/Bob", "People/Family",
		"Events/Birthday", "Events/Wedding", "Nature/Birds", "Nature/Garden",
		"Projects/Renovation", "Receipts/Taxes/2024", "Favorites", "To Print",
	}
	tags, _, err := cat.ResolveOrCreateTags(tagNames, 1)
	if err != nil {
		b.Fatal(err)
	}

	tx, err := cat.db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback()
	insertFile, err := tx.Prepare(`INSERT INTO files
		(folder_label, relative_path, filename, extension, size_bytes, file_mtime, status, description)
		VALUES (?, ?, ?, '.jpg', ?, ?, 'synced', ?)`)
	if err != nil {
		b.Fatal(err)
	}
	defer insertFile.Close()
	insertTag, err := tx.Prepare(`INSERT OR IGNORE INTO file_tags (file_id, tag_id) VALUES (?, ?)`)
	if err != nil {
		b.Fatal(err)
	}
	defer insertTag.Close()

	folders := []string{"CameraRoll", "Scans", "Documents", "Shared"}
	descriptions := []string{
		"coastal travel journal with family",
		"birthday dinner and candles",
		"garden bird observation",
		"receipt for the renovation project",
		"portrait selected for printing",
		"museum notes from the afternoon",
		"",
		"",
	}
	mtime := time.Unix(1_700_000_000, 0).UTC()
	for i := 0; i < searchBenchmarkRows; i++ {
		filename := fmt.Sprintf("IMG_%06d.jpg", i)
		relative := fmt.Sprintf("%d/%02d/Album-%03d/%s", 2018+i%8, 1+i%12, i%240, filename)
		result, err := insertFile.Exec(
			folders[i%len(folders)], relative, filename, 100_000+i%8_000_000,
			mtime.Add(time.Duration(i)*time.Second), descriptions[i%len(descriptions)],
		)
		if err != nil {
			b.Fatalf("insert file %d: %v", i, err)
		}
		fileID, err := result.LastInsertId()
		if err != nil {
			b.Fatal(err)
		}
		// About 80% of files carry one to three leaf tags. Ancestors exist in
		// tags but are intentionally not assigned, matching the production model.
		if i%5 == 0 {
			continue
		}
		count := 1 + i%3
		for j := 0; j < count; j++ {
			tag := tags[(i*7+j*5)%len(tags)]
			if _, err := insertTag.Exec(fileID, tag.ID); err != nil {
				b.Fatalf("tag file %d: %v", i, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
}
