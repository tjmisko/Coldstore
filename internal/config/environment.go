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

// writeProbePattern names the file probeWritable creates. It ends in
// ".coldstore-tmp" on purpose: the probe runs inside every [[folders]].path,
// which is a live Syncthing folder, and Syncthing's watcher sees the create
// before the unlink. Every deployment is already told to put `*.coldstore-tmp`
// in that folder's .stignore for the mover's temp siblings, and daemon's
// skipScanEntry already refuses to catalogue the suffix, so reusing it means
// the probe is covered by rules that exist rather than needing a third one
// nobody was told about. (The daemon package owns the ".coldstore-tmp"
// constant; config cannot import daemon, and one shared string is not worth a
// third package -- same reason as stfolderMarker above.)
const writeProbePattern = ".coldstore-writecheck-*.coldstore-tmp"

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
			"coldstore will not create it: on an unmounted volume that would silently write "+
			"the archive and the catalog to the root filesystem. Create it first, e.g.\n"+
			"    mkdir -p %s   # then mount your archive volume there, see docs/deployment.md\n"+
			"    chown SERVICEUSER %s && chmod 700 %s\n"+
			"If it IS a mountpoint, the volume is not mounted: findmnt %s",
			path, path, path, path, path)
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

	// Last, and only when asked for: everything above passes on an UNMOUNTED
	// mountpoint, which is the one failure here that costs data outright.
	if c.Storage.RequireMountpoint {
		if err := requireMountpoint(path); err != nil {
			return err
		}
	}
	return nil
}

// requireMountpoint is the [storage] require_mountpoint check. See the field's
// documentation in config.go for why the absence of the path is not the guard
// it looks like.
func requireMountpoint(path string) error {
	state, err := DescribeMount(path)
	if err != nil {
		return fmt.Errorf("[storage] cannot determine whether cold_storage_path %q is a "+
			"mount point, and require_mountpoint is set: %w", path, err)
	}
	if state.IsMountPoint {
		return nil
	}
	return fmt.Errorf(`[storage] cold_storage_path %q is not a mount point, and require_mountpoint is set.

The directory exists and is writable, which is exactly what an unmounted volume
looks like: mount(8) needs the mountpoint directory to exist, so a volume that
failed to mount leaves an empty writable stub behind on the root filesystem.

Starting here would build a fresh catalog on the root filesystem -- losing the
budgets and pins the real one holds -- and archive into it, while Syncthing
propagated every one of those deletions to every device you own.

    findmnt %s
    systemctl status $(systemd-escape -p --suffix=mount %s)

If cold storage really is meant to be a plain directory on an existing
filesystem, remove require_mountpoint from [storage].`, path, path, path)
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
	f, err := os.CreateTemp(dir, writeProbePattern)
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

// MountState describes whether a path is the root of its own mounted
// filesystem. It exists because that is the one thing --check most needs to
// tell an owner about cold storage and could not.
//
// coldstore refuses to CREATE cold_storage_path, and the stated protection is
// that an unmounted volume therefore leaves the daemon with nothing to write
// to. That holds for a ZFS dataset with a native mountpoint=, which `zfs mount`
// materialises itself. It does NOT hold for anything mounted from /etc/fstab:
// mount(8) requires the mountpoint directory to exist, so an unmounted volume
// leaves behind a real, empty, writable directory on the root filesystem that
// passes every check in checkColdStorage. The daemon then builds a fresh
// catalog and archives into the root filesystem while Syncthing propagates each
// deletion to every device the owner has.
//
// Reporting is all this does. A plain directory on an existing filesystem is a
// supported and documented layout -- it is what the tests and the demo use --
// so "not a mount point" cannot be an error. It can be printed, which is enough
// for an owner to notice it on the line above `preflight OK`.
type MountState struct {
	IsMountPoint bool
	FSType       string
}

// fsTypeNames maps the statfs magic numbers worth naming. An unlisted
// filesystem reports an empty FSType, which the caller simply omits.
var fsTypeNames = map[int64]string{
	0x9123683e: "btrfs",
	0x2fc12fc1: "zfs",
	0xef53:     "ext2/3/4",
	0x58465342: "xfs",
	0x01021994: "tmpfs",
	0x6969:     "nfs",
	0x2011bab0: "exfat",
	0x4d44:     "vfat",
	0x794c7630: "overlayfs",
}

// DescribeMount reports whether path is a mount point by comparing its device
// id with its parent's -- the same test findmnt makes, and one that needs no
// /proc/mounts parsing and no privileges.
func DescribeMount(path string) (MountState, error) {
	var state MountState

	var self syscall.Stat_t
	if err := syscall.Stat(path, &self); err != nil {
		return state, err
	}

	parent := filepath.Dir(path)
	if parent == path {
		state.IsMountPoint = true // "/" is its own parent
	} else {
		var up syscall.Stat_t
		if err := syscall.Stat(parent, &up); err != nil {
			return state, err
		}
		state.IsMountPoint = self.Dev != up.Dev
	}

	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err == nil {
		state.FSType = fsTypeNames[int64(fs.Type)]
	}
	return state, nil
}
