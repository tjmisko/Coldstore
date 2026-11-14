package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const collectionsHeader = `
[storage]
cold_storage_path = "/tmp/coldtest"
`

const folderBlock = `
[[folders]]
label = "Photos"
path = "/tmp/photos"
budget = "10GB"
`

func writeCollectionsConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "coldstore.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustFailLoad(t *testing.T, body, wantSubstring string) error {
	t.Helper()
	_, err := Load(writeCollectionsConfig(t, body))
	if err == nil {
		t.Fatalf("Load accepted:\n%s", body)
	}
	if !strings.Contains(err.Error(), wantSubstring) {
		t.Fatalf("error %q does not contain %q", err, wantSubstring)
	}
	return err
}

func TestCollections(t *testing.T) {
	t.Run("should load when only [[collections]] are declared", func(t *testing.T) {
		cfg, err := Load(writeCollectionsConfig(t, collectionsHeader+"\n[[collections]]\nname = \"Photos\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Collections) != 1 || cfg.Collections[0].Name != "Photos" || len(cfg.Folders) != 0 {
			t.Fatalf("Collections = %+v, Folders = %+v", cfg.Collections, cfg.Folders)
		}
	})

	t.Run("should refuse and name both sections when neither folders nor collections are declared", func(t *testing.T) {
		mustFailLoad(t, collectionsHeader, "no [[folders]] and no [[collections]]")
	})

	t.Run("should load collections beside folders when names differ", func(t *testing.T) {
		cfg, err := Load(writeCollectionsConfig(t, collectionsHeader+folderBlock+"\n[[collections]]\nname = \"Archive 2024\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Folders) != 1 || len(cfg.Collections) != 1 {
			t.Fatalf("got %d folders, %d collections", len(cfg.Folders), len(cfg.Collections))
		}
	})

	t.Run("should reject a collection when its name equals a folder label", func(t *testing.T) {
		mustFailLoad(t, collectionsHeader+folderBlock+"\n[[collections]]\nname = \"Photos\"\n", "same name as a folder")
	})

	t.Run("should reject a duplicate collection name", func(t *testing.T) {
		mustFailLoad(t, collectionsHeader+"\n[[collections]]\nname = \"A\"\n[[collections]]\nname = \"A\"\n", "more than once")
	})

	t.Run("should reject a collection when its name is missing", func(t *testing.T) {
		mustFailLoad(t, collectionsHeader+"\n[[collections]]\n", "no name")
	})

	t.Run("should reject a collection named sha256", func(t *testing.T) {
		mustFailLoad(t, collectionsHeader+"\n[[collections]]\nname = \"sha256\"\n", "reserved")
	})

	t.Run("should reject a folder labelled sha256", func(t *testing.T) {
		mustFailLoad(t, collectionsHeader+"\n[[folders]]\nlabel = \"sha256\"\npath = \"/tmp/x\"\nbudget = \"1GB\"\n", "reserved")
	})

	t.Run("should reject a collection name containing a slash", func(t *testing.T) {
		mustFailLoad(t, collectionsHeader+"\n[[collections]]\nname = \"a/b\"\n", "invalid")
	})

	t.Run("should reject a collection name when it breaks the pattern", func(t *testing.T) {
		long := strings.Repeat("a", 65)
		for _, name := range []string{" lead", ".dot", "-dash", "tab\\t", "new\\nline", "..", "caf\\u00e9", long} {
			mustFailLoad(t, collectionsHeader+"\n[[collections]]\nname = \""+name+"\"\n", "invalid")
		}
	})

	t.Run("should accept a collection name when it is at the pattern limits", func(t *testing.T) {
		for _, name := range []string{"a", "0", "A b.c_d-e", strings.Repeat("a", 64)} {
			if _, err := Load(writeCollectionsConfig(t, collectionsHeader+"\n[[collections]]\nname = \""+name+"\"\n")); err != nil {
				t.Errorf("name %q rejected: %v", name, err)
			}
		}
	})
}

func TestArchiveDefaults(t *testing.T) {
	load := func(t *testing.T, archive string) (*Config, error) {
		t.Helper()
		return Load(writeCollectionsConfig(t, collectionsHeader+folderBlock+archive))
	}

	t.Run("should default upload_concurrency to 2 when [archive] is absent", func(t *testing.T) {
		cfg, err := load(t, "")
		if err != nil {
			t.Fatal(err)
		}
		want := ArchiveConfig{UploadConcurrency: 2, ScrubIntervalHours: 720, ScrubRateMiBPerSec: 20}
		if cfg.Archive != want {
			t.Fatalf("Archive = %+v, want %+v", cfg.Archive, want)
		}
	})

	t.Run("should keep an explicit 0 when scrub_interval_hours and scrub_rate are set to 0", func(t *testing.T) {
		cfg, err := load(t, "\n[archive]\nscrub_interval_hours = 0\nscrub_rate_mib_per_sec = 0\n")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Archive.ScrubIntervalHours != 0 || cfg.Archive.ScrubRateMiBPerSec != 0 {
			t.Fatalf("Archive = %+v; explicit zeros were replaced by defaults", cfg.Archive)
		}
		if cfg.Archive.UploadConcurrency != 2 {
			t.Fatalf("UploadConcurrency = %d, want the default 2", cfg.Archive.UploadConcurrency)
		}
	})

	t.Run("should default the other keys when only one is set", func(t *testing.T) {
		cfg, err := load(t, "\n[archive]\nupload_concurrency = 5\n")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Archive.UploadConcurrency != 5 || cfg.Archive.ScrubIntervalHours != 720 || cfg.Archive.ScrubRateMiBPerSec != 20 {
			t.Fatalf("Archive = %+v", cfg.Archive)
		}
	})

	t.Run("should reject upload_concurrency when it is outside 1..8", func(t *testing.T) {
		for _, v := range []string{"-1", "9", "100"} {
			if _, err := load(t, "\n[archive]\nupload_concurrency = "+v+"\n"); err == nil {
				t.Errorf("upload_concurrency = %s accepted", v)
			}
		}
	})

	t.Run("should accept upload_concurrency when it is 1 or 8", func(t *testing.T) {
		for _, v := range []string{"1", "8"} {
			if _, err := load(t, "\n[archive]\nupload_concurrency = "+v+"\n"); err != nil {
				t.Errorf("upload_concurrency = %s rejected: %v", v, err)
			}
		}
	})

	t.Run("should reject the scrub settings when they are negative", func(t *testing.T) {
		for _, key := range []string{"scrub_interval_hours", "scrub_rate_mib_per_sec"} {
			if _, err := load(t, "\n[archive]\n"+key+" = -1\n"); err == nil {
				t.Errorf("%s = -1 accepted", key)
			}
		}
	})
}

func TestStrictDecoding(t *testing.T) {
	t.Run("should reject the file when a [storage] key is misspelt", func(t *testing.T) {
		err := mustFailLoad(t, "[storage]\ncold_storage_path = \"/tmp/c\"\nrequire_mountpont = true\n"+folderBlock, "storage.require_mountpont")
		var unknown *UnknownKeysError
		if !errors.As(err, &unknown) {
			t.Fatalf("error is %T, want *UnknownKeysError", err)
		}
	})

	t.Run("should name every unknown key when several are present", func(t *testing.T) {
		body := "[storage]\ncold_storage_path = \"/tmp/c\"\nrequire_mountpont = true\n" +
			"[daemon]\nscan_interval = 5\n" +
			"[[folders]]\nlabel = \"P\"\npath = \"/tmp/p\"\nbudget = \"1GB\"\npersistant = [\"x\"]\n"
		_, err := Load(writeCollectionsConfig(t, body))
		var unknown *UnknownKeysError
		if !errors.As(err, &unknown) {
			t.Fatalf("error is %v, want *UnknownKeysError", err)
		}
		want := []string{"daemon.scan_interval", "folders.persistant", "storage.require_mountpont"}
		if strings.Join(unknown.Keys, ",") != strings.Join(want, ",") {
			t.Fatalf("Keys = %v, want %v", unknown.Keys, want)
		}
	})

	t.Run("should reject an unknown key inside a [[folders]] block", func(t *testing.T) {
		mustFailLoad(t, collectionsHeader+"[[folders]]\nlabel = \"P\"\npath = \"/tmp/p\"\nbudget = \"1GB\"\nbogus = 1\n", "folders.bogus")
	})

	t.Run("should reject an unknown key inside a [[collections]] block", func(t *testing.T) {
		mustFailLoad(t, collectionsHeader+"[[collections]]\nname = \"A\"\npath = \"/x\"\n", "collections.path")
	})

	t.Run("should reject an unknown top-level table when it is not a known section", func(t *testing.T) {
		mustFailLoad(t, collectionsHeader+folderBlock+"\n[archiv]\nupload_concurrency = 2\n", "archiv")
	})

	t.Run("should still load for inspection when a key is unknown", func(t *testing.T) {
		path := writeCollectionsConfig(t, "[storage]\ncold_storage_path = \"/tmp/c\"\nrequire_mountpont = true\n"+folderBlock)
		if _, err := LoadForInspection(path); err != nil {
			t.Fatalf("LoadForInspection refused a typo: %v", err)
		}
	})

	t.Run("should load coldstore.toml.example without unknown keys", func(t *testing.T) {
		if _, err := Load(filepath.Join("..", "..", "coldstore.toml.example")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("should load docker/coldstore.test.toml without unknown keys", func(t *testing.T) {
		if _, err := Load(filepath.Join("..", "..", "docker", "coldstore.test.toml")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("should still reject a type mismatch when a known key has the wrong type", func(t *testing.T) {
		mustFailLoad(t, "[storage]\ncold_storage_path = 5\n"+folderBlock, "parsing config")
	})
}
