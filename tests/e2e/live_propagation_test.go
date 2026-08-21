//go:build e2e_live

package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// This file tests the claim the whole product rests on.
//
// Coldstore frees space on the owner's phone by DELETING a file from the
// server's Syncthing folder and letting Syncthing propagate that deletion to
// every paired device; a restore is the same trick in reverse. Nothing in the
// unit or in-process e2e tiers can observe that -- they have no second device
// and no Syncthing at all -- so until now it existed only as a paragraph in
// docs/control-plane-design.md.
//
// Device B here is the phone. Coldstore cannot see it, cannot reach it, and does
// not know it exists. Every assertion below about device B is an assertion about
// something Syncthing did on its own.

func syncthingBPath(rel string) string { return syncthingSyncPath + "/" + rel }

func existsOnDeviceB(rel string) func() bool {
	return func() bool { return live.existsIn(serviceSyncthingB, syncthingBPath(rel)) }
}

func goneFromDeviceB(rel string) func() bool {
	return func() bool { return !live.existsIn(serviceSyncthingB, syncthingBPath(rel)) }
}

// propagationTimeout is generous on purpose. Syncthing's index exchange,
// scanning and pulling are all asynchronous; in practice each hop here settles
// in 3-6 seconds, and a limit ten times that keeps a slow CI runner from
// producing a red build that says nothing about the code.
const propagationTimeout = 90 * time.Second

func TestLiveSyncthingPropagation(t *testing.T) {
	const rel = "prop-photo.bin"
	content := randomBytes(512_000, 41)
	want := sha256Hex(content)

	var fileID int64

	ok := t.Run("should reach the managed folder and be catalogued when a file is created on the paired device", func(t *testing.T) {
		// Written on device B only. Coldstore has no way to know about it
		// except by Syncthing delivering it.
		live.writeFileIn(t, serviceSyncthingB, syncthingBPath(rel), content)

		await(t, propagationTimeout, "the file to arrive in the coldstore-managed folder", func() bool {
			return live.existsIn(serviceColdstore, managedPath(rel))
		})
		if got := live.sha256In(serviceColdstore, managedPath(rel)); got != want {
			t.Fatalf("the file that arrived has sha256 %q, want %q", got, want)
		}

		live.scanAndWait(t)
		file, found := live.fileByRelPath(t, rel)
		if !found {
			t.Fatalf("no catalog record for %q after it synced in and a scan completed", rel)
		}
		if file.Status != "synced" {
			t.Fatalf("status = %q, want synced", file.Status)
		}
		if file.SizeBytes != int64(len(content)) {
			t.Fatalf("size_bytes = %d, want %d", file.SizeBytes, len(content))
		}
		fileID = file.ID
	})
	if !ok {
		t.Fatal("the file never reached coldstore; the rest of the propagation chain cannot be tested")
	}

	ok = t.Run("should disappear from the paired device when coldstore archives the file", func(t *testing.T) {
		// Same rule writeManaged follows: do not delete a file the cluster is
		// still settling on, or the deletion races the pull and comes back as a
		// conflict instead of propagating.
		awaitFixtureConverged(t, rel)

		var archived liveFile
		live.mustJSON(t, http.MethodPost,
			fmt.Sprintf("/api/v1/files/%d/archive", fileID), nil, &archived, http.StatusOK)
		if archived.Status != "archived" {
			t.Fatalf("status = %q after archive, want archived", archived.Status)
		}
		if live.existsIn(serviceColdstore, managedPath(rel)) {
			t.Fatalf("%s is still in the managed folder after archiving", rel)
		}

		// THE CLAIM. Nobody told device B to delete anything.
		await(t, propagationTimeout,
			"Syncthing to propagate coldstore's deletion to device B (this is the product's entire premise)",
			goneFromDeviceB(rel))

		// And device B agrees it is deleted rather than merely out of sync,
		// which is the failure mode a Receive Only folder would produce.
		entry, err := live.syncthingB.indexEntry(liveSyncthingFolderID, rel)
		if err != nil {
			t.Fatalf("querying device B's index for %q: %v", rel, err)
		}
		if !entry.Present {
			t.Fatalf("device B has no index entry for %q at all", rel)
		}
		if !entry.Deleted {
			t.Errorf("device B's index still lists %q as present; the deletion was not accepted", rel)
		}
	})
	if !ok {
		t.Fatal("the deletion did not propagate; the restore half cannot be tested meaningfully")
	}

	t.Run("should come back to the paired device when the file is restored through the API", func(t *testing.T) {
		var restored liveFile
		live.mustJSON(t, http.MethodPost,
			fmt.Sprintf("/api/v1/files/%d/restore", fileID), nil, &restored, http.StatusOK)
		if restored.Status != "synced" {
			t.Fatalf("status = %q after restore, want synced", restored.Status)
		}
		if got := live.sha256In(serviceColdstore, managedPath(rel)); got != want {
			t.Fatalf("restored file in the managed folder has sha256 %q, want %q", got, want)
		}

		// THE OTHER HALF OF THE CLAIM. A restore that does not reach the phone
		// is a restore the owner cannot use.
		await(t, propagationTimeout,
			"Syncthing to propagate the restored file back to device B", existsOnDeviceB(rel))

		if got := live.sha256In(serviceSyncthingB, syncthingBPath(rel)); got != want {
			t.Errorf("device B received %q with sha256 %q, want %q", rel, got, want)
		}
	})
}

// TestLiveShouldNotLeaveAColdstoreTempSiblingOnThePairedDeviceWhenARestoreCompletes
// is deliberately written to be able to fail.
//
// A restore is a cross-device copy: coldstore writes `NAME.coldstore-tmp` next
// to the destination and renames it into place (internal/daemon/move.go). For
// the duration of that copy the temp sibling is a real file inside a folder
// Syncthing is watching, and design section 3.8 records that keeping it out of
// Syncthing's way needs a .stignore entry -- which is explicitly out of scope on
// this branch. So there are three possible outcomes and the test reports which
// one actually happened rather than asserting the convenient one:
//
//   - never indexed        -- the rename beat Syncthing's watcher. Good, but
//     timing-dependent, not a guarantee.
//   - indexed then deleted -- the sibling was synced to the phone and then
//     removed. Wasted transfer, no lasting damage.
//   - indexed and present  -- a permanent junk file on the owner's phone. This
//     is the only outcome that fails the test.
func TestLiveShouldNotLeaveAColdstoreTempSiblingOnThePairedDeviceWhenARestoreCompletes(t *testing.T) {
	const rel = "prop-tmp.bin"
	tmpRel := rel + tmpSuffixLive

	// Large enough that the copy is not instantaneous, which is what gives
	// Syncthing's watcher a chance to see the sibling at all. A tiny file would
	// make "never indexed" a foregone conclusion and the test vacuous.
	content := randomBytes(8<<20, 43)
	want := sha256Hex(content)

	writeManaged(t, rel, content)
	await(t, propagationTimeout, "the file to reach device B before the archive", existsOnDeviceB(rel))

	live.scanAndWait(t)
	file, found := live.fileByRelPath(t, rel)
	if !found {
		t.Fatalf("no catalog record for %q", rel)
	}

	var archived liveFile
	live.mustJSON(t, http.MethodPost,
		fmt.Sprintf("/api/v1/files/%d/archive", file.ID), nil, &archived, http.StatusOK)
	await(t, propagationTimeout, "the archive to propagate to device B", goneFromDeviceB(rel))

	var restored liveFile
	live.mustJSON(t, http.MethodPost,
		fmt.Sprintf("/api/v1/files/%d/restore", file.ID), nil, &restored, http.StatusOK)
	await(t, propagationTimeout, "the restore to propagate to device B", existsOnDeviceB(rel))
	if got := live.sha256In(serviceSyncthingB, syncthingBPath(rel)); got != want {
		t.Errorf("device B received %q with sha256 %q, want %q", rel, got, want)
	}

	// Give Syncthing a moment to deliver anything else it had queued from that
	// window before declaring the folder clean. The index probe below is the
	// real detector; this only stops the directory listing being a snapshot
	// taken mid-transfer.
	if awaitNoFatal(5*time.Second, func() bool {
		return live.existsIn(serviceSyncthingB, syncthingBPath(tmpRel))
	}) {
		t.Logf("device B currently holds %s", tmpRel)
	}

	// The assertion that matters: nothing named *.coldstore-tmp is left on the
	// phone, in either folder.
	for _, name := range live.listDirIn(t, serviceSyncthingB, syncthingSyncPath) {
		if len(name) > len(tmpSuffixLive) && name[len(name)-len(tmpSuffixLive):] == tmpSuffixLive {
			t.Errorf("device B permanently holds coldstore's temp sibling %q; "+
				"design 3.8's .stignore work is required before deployment", name)
		}
	}
	for _, name := range live.listDirIn(t, serviceColdstore, coldstoreSyncPath) {
		if len(name) > len(tmpSuffixLive) && name[len(name)-len(tmpSuffixLive):] == tmpSuffixLive {
			t.Errorf("the managed folder permanently holds the temp sibling %q", name)
		}
	}

	// The observation, reported either way. This is not an assertion: whether
	// the sibling is ever seen depends on how fast the copy is relative to
	// Syncthing's 1s watcher delay, and a green "never seen" here is evidence,
	// not a guarantee.
	report := func(c *syncthingClient) {
		entry, err := c.indexEntry(liveSyncthingFolderID, tmpRel)
		if err != nil {
			t.Logf("OBSERVATION: could not query %s's index for %s: %v", c.name, tmpRel, err)
			return
		}
		switch {
		case !entry.Present:
			t.Logf("OBSERVATION: %s never indexed %s -- the rename beat Syncthing's watcher on this run. "+
				"Timing-dependent, not a guarantee; a slower pool or a larger file could change it.",
				c.name, tmpRel)
		case entry.Deleted:
			t.Logf("OBSERVATION: %s DID index %s and then recorded it deleted. The sibling was visible to "+
				"Syncthing and, on a real deployment, would have been transferred to the phone and then "+
				"deleted again -- wasted transfer, no lasting artifact. This is exactly what design 3.8's "+
				".stignore entry is for, and that work is out of scope on this branch.", c.name, tmpRel)
		default:
			t.Errorf("%s indexed %s and still lists it as present: a permanent artifact", c.name, tmpRel)
		}
	}
	report(live.syncthingA)
	report(live.syncthingB)
}
