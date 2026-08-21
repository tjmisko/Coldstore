package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// stfolderMarker is the file Syncthing plants in every folder it manages. The
// daemon package has its own copy of this constant; config cannot import daemon
// (daemon imports config) and one shared string is not worth a third package.
const stfolderMarker = ".stfolder"

// CheckEnvironment inspects the machine for the things Validate cannot know from
// the file alone, and splits what it finds into two piles.
//
// The split is the whole point. A folder path that is missing, or present but
// without Syncthing's .stfolder marker, is NOT a configuration error: on a fresh
// boot the daemon can easily win the race against the mount unit or against
// Syncthing's own first scan, and refusing to start would turn a transient
// ordering problem into a service that stays down until someone logs in. Those
// come back as warnings, the daemon starts, and the existing .stfolder guards
// keep it from touching the folder until it is really there.
//
// Cold storage is the opposite case. Nothing about coldstore works without it,
// the daemon would create the catalog and the archive under whatever empty
// directory happens to be at that path, and doing that on an unmounted pool
// writes the archive to the root filesystem. That is fatal.
func (c *Config) CheckEnvironment() (warnings []string, err error) {
	if err := c.checkColdStorage(); err != nil {
		return nil, err
	}

	for i := range c.Folders {
		w, err := checkFolder(c.Folders[i].Label, c.Folders[i].Path)
		if err != nil {
			return nil, err
		}
		if w != "" {
			warnings = append(warnings, w)
		}
	}

	return warnings, nil
}

func (c *Config) checkColdStorage() error {
	path := c.Storage.ColdStoragePath

	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("[storage] cold_storage_path %q does not exist. "+
			"coldstore will not create it: on an unmounted pool that would silently write "+
			"the archive and the catalog to the root filesystem. Create it first, e.g.\n"+
			"    zfs create -o mountpoint=%s tank/coldstore   # or: mkdir -p %s\n"+
			"    chown coldstore-user %s && chmod 700 %s", path, path, path, path, path)
	}
	if err != nil {
		return fmt.Errorf("[storage] cold_storage_path %q is not readable: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("[storage] cold_storage_path %q is not a directory", path)
	}

	if err := probeWritable(path); err != nil {
		return fmt.Errorf("[storage] cold_storage_path %q is not writable by this user (%s): %w. "+
			"Every archived file, the catalog and every thumbnail are written here. %s",
			path, currentUserHint(), err,
			writeFailureHint(err, "Chown it to the account in the unit's User= and check its mode."))
	}
	return nil
}

// checkFolder returns a warning string for a folder that is not ready yet, or an
// error for one that is misconfigured in a way no amount of waiting will fix.
func checkFolder(label, path string) (string, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Sprintf("folder %q path %q does not exist yet; coldstore will skip it "+
			"until it appears (an unmounted volume looks exactly like this)", label, path), nil
	}
	if errors.Is(err, os.ErrPermission) {
		return fmt.Sprintf("folder %q path %q cannot be stat'ed by this user (%s); "+
			"coldstore will skip it", label, path, currentUserHint()), nil
	}
	if err != nil {
		return "", fmt.Errorf("folder %q: cannot stat path %q: %w", label, path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("folder %q: path %q is not a directory", label, path)
	}

	// No .stfolder means "not a mounted Syncthing folder", which is exactly the
	// boot-ordering case. Do not judge its permissions -- an unmounted mountpoint
	// is often a root-owned stub whose mode says nothing about the real volume.
	if _, err := os.Stat(filepath.Join(path, stfolderMarker)); err != nil {
		return fmt.Sprintf("folder %q path %q has no %s marker; coldstore will skip it until "+
			"Syncthing creates one (the volume may not be mounted yet)",
			label, path, stfolderMarker), nil
	}

	// From here the folder is demonstrably real and mounted, so a permission
	// problem is a deployment mistake, not a race.
	if err := probeWritable(path); err != nil {
		return "", fmt.Errorf("folder %q path %q is mounted (%s present) but not writable by "+
			"this user (%s): %w. coldstore must delete files here to free space and write them "+
			"back on restore. %s",
			label, path, stfolderMarker, currentUserHint(), err,
			writeFailureHint(err, "Run it as the same account Syncthing runs as."))
	}
	return "", nil
}

// writeFailureHint turns the errno behind a failed write probe into the remedy
// that actually applies to it.
//
// EROFS and EACCES read almost identically in a log line and have opposite
// fixes. Collapsing them into one "run it as the account Syncthing runs as"
// message sent an owner whose only mistake was leaving a [[folders]].path out
// of the unit's ReadWritePaths= off to chown a directory that was already
// correctly owned -- and chown cannot make a read-only mount writable, so they
// would have kept going.
//
// Under the shipped unit ProtectSystem=strict is the overwhelmingly likely
// cause of EROFS here: everything outside ReadWritePaths= is read-only to this
// process and to nothing else on the box, which is exactly why `ls -ld` and a
// hand-run `--check` outside the sandbox both look fine.
func writeFailureHint(err error, ownershipFix string) string {
	if errors.Is(err, syscall.EROFS) {
		return "This path is read-only TO THIS PROCESS, which is not an ownership problem: " +
			"chown will not fix it. Under the shipped systemd unit it means ProtectSystem=strict " +
			"is in force and this path is missing from ReadWritePaths= -- add it, then " +
			"`systemctl daemon-reload && systemctl restart coldstore`. Outside systemd it means " +
			"the filesystem itself is mounted read-only."
	}
	return ownershipFix
}

// probeWritable creates and removes a file, because os.Stat's mode bits lie under
// ACLs, read-only mounts and systemd's ProtectSystem= sandbox -- which is the most
// likely reason a production deployment cannot write a path it appears to own.
func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".coldstore-writecheck-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

func currentUserHint() string {
	return fmt.Sprintf("uid %d", os.Getuid())
}
