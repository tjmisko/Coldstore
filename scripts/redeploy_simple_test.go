package scripts_test

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

const testRevision = "0123456789abcdef0123456789abcdef01234567"

type redeployFixture struct {
	script      string
	repo        string
	installed   string
	backupRoot  string
	stateFile   string
	commandLog  string
	environment []string
}

func TestSimpleRedeployCompletesWithVerifiedBackups(t *testing.T) {
	fixture := newRedeployFixture(t, "9")
	output, err := fixture.run()
	if err != nil {
		t.Fatalf("redeploy failed: %v\n%s", err, output)
	}
	for _, wanted := range []string{
		"Redeploy complete",
		"revision  " + testRevision,
		"schema    9",
		"Open the private Tailscale URL",
	} {
		if !strings.Contains(output, wanted) {
			t.Errorf("output missing %q:\n%s", wanted, output)
		}
	}
	installed, err := os.ReadFile(fixture.installed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(installed), "preflight OK, no warnings") {
		t.Fatalf("installed binary was not replaced with the build: %q", installed)
	}
	entries, err := os.ReadDir(fixture.backupRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("backup entries = %d, want 1", len(entries))
	}
	for _, name := range []string{"catalog.db", "coldstore.previous"} {
		if _, err := os.Stat(filepath.Join(fixture.backupRoot, entries[0].Name(), name)); err != nil {
			t.Errorf("missing backup %s: %v", name, err)
		}
	}
	log := fixture.readLog(t)
	for _, wanted := range []string{"git pull --ff-only origin main", "systemctl stop coldstore", "systemctl start coldstore"} {
		if !strings.Contains(log, wanted) {
			t.Errorf("command log missing %q:\n%s", wanted, log)
		}
	}
	statusAt := strings.Index(log, " --status")
	stopAt := strings.Index(log, "systemctl stop coldstore")
	checkAt := strings.Index(log, " --check")
	if statusAt < 0 || stopAt < 0 || checkAt < 0 || !(statusAt < stopAt && stopAt < checkAt) {
		t.Errorf("expected immutable status before stop and full preflight after stop:\n%s", log)
	}
}

func TestSimpleRedeployRefusesSchemaChangesBeforeDowntime(t *testing.T) {
	fixture := newRedeployFixture(t, "8")
	output, err := fixture.run()
	if err == nil {
		t.Fatalf("schema-changing redeploy unexpectedly succeeded:\n%s", output)
	}
	if !strings.Contains(output, "schema change detected (production 8, release 9)") ||
		!strings.Contains(output, "use the full deployment runbook") {
		t.Fatalf("schema refusal was not actionable:\n%s", output)
	}
	if strings.Contains(fixture.readLog(t), "systemctl stop coldstore") {
		t.Fatal("schema refusal stopped the production service")
	}
	if state := strings.TrimSpace(readFile(t, fixture.stateFile)); state != "active" {
		t.Fatalf("service state = %q, want active", state)
	}
}

func TestSimpleRedeployRollsBackTheBinaryWhenStartFails(t *testing.T) {
	fixture := newRedeployFixture(t, "9")
	fixture.environment = append(fixture.environment, "FAKE_FAIL_START_ONCE=1")
	output, err := fixture.run()
	if err == nil {
		t.Fatalf("failed start unexpectedly succeeded:\n%s", output)
	}
	if !strings.Contains(output, "systemctl could not start the new service") ||
		!strings.Contains(output, "Attempting automatic binary rollback") ||
		!strings.Contains(output, "previous service restarted successfully") {
		t.Fatalf("rollback diagnostics were incomplete:\n%s", output)
	}
	if got := string(mustRead(t, fixture.installed)); got != "#!/bin/sh\n# previous binary\n" {
		t.Fatalf("installed binary after rollback = %q", got)
	}
	if state := strings.TrimSpace(readFile(t, fixture.stateFile)); state != "active" {
		t.Fatalf("service state after rollback = %q, want active", state)
	}
}

func TestSimpleRedeployBuildFailureNamesTheStageAndLeavesServiceAlone(t *testing.T) {
	fixture := newRedeployFixture(t, "9")
	fixture.environment = append(fixture.environment, "FAKE_MAKE_FAIL=1")
	output, err := fixture.run()
	if err == nil {
		t.Fatalf("failed build unexpectedly succeeded:\n%s", output)
	}
	if !strings.Contains(output, "make build failed") || !strings.Contains(output, "Stage: building release") {
		t.Fatalf("build diagnostics were incomplete:\n%s", output)
	}
	if strings.Contains(fixture.readLog(t), "systemctl stop coldstore") {
		t.Fatal("build failure stopped the production service")
	}
}

func newRedeployFixture(t *testing.T, onDiskSchema string) *redeployFixture {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	fakeBin := filepath.Join(root, "bin")
	coldStorage := filepath.Join(root, "cold-storage")
	backupRoot := filepath.Join(root, "backups")
	installed := filepath.Join(root, "usr", "local", "bin", "coldstore")
	config := filepath.Join(root, "etc", "coldstore.toml")
	stateFile := filepath.Join(root, "service-state")
	commandLog := filepath.Join(root, "commands.log")
	failStartMarker := filepath.Join(root, "failed-start-once")

	for _, dir := range []string{
		filepath.Join(repo, ".git"),
		filepath.Join(repo, "internal", "catalog"),
		fakeBin,
		coldStorage,
		filepath.Dir(installed),
		filepath.Dir(config),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(repo, "internal", "catalog", "migrations.go"), "package catalog\nconst schemaVersion = 9\n", 0o644)
	writeFile(t, filepath.Join(coldStorage, "catalog.db"), "fixture catalog\n", 0o644)
	writeFile(t, config, "fixture config\n", 0o644)
	writeFile(t, installed, "#!/bin/sh\n# previous binary\n", 0o755)
	writeFile(t, stateFile, "active\n", 0o644)
	writeFile(t, commandLog, "", 0o644)

	newBinary := `#!/bin/sh
printf 'coldstore %s\n' "$*" >>"$FAKE_COMMAND_LOG"
if [ "$1" = "--config" ] && [ "$3" = "--status" ]; then
  printf 'coldstore status: FAILED\n'
  printf 'cold storage      %s\n' "$FAKE_COLD_STORAGE"
  printf '  FAILURE catalog_unreadable catalog has an uncheckpointed WAL\n'
  exit 2
fi
if [ "$1" = "--config" ] && [ "$3" = "--check" ]; then
  printf 'config           %s\n' "$2"
  printf 'cold storage     %s (exists, writable; mounted, btrfs)\n' "$FAKE_COLD_STORAGE"
  printf 'systemd          ReadWritePaths=%s /home/test/Camera Roll (Tristan)\n' "$FAKE_COLD_STORAGE"
  printf 'preflight OK, no warnings\n'
  exit 0
fi
exit 2
`
	writeFile(t, filepath.Join(repo, "coldstore"), newBinary, 0o755)

	writeExecutable(t, fakeBin, "git", `
printf 'git %s\n' "$*" >>"$FAKE_COMMAND_LOG"
case "$1" in
  branch) printf 'main\n' ;;
  status) ;;
  pull) ;;
  rev-parse) printf '%s\n' "$FAKE_REVISION" ;;
  *) exit 2 ;;
esac
`)
	writeExecutable(t, fakeBin, "make", `
printf 'make %s\n' "$*" >>"$FAKE_COMMAND_LOG"
[ "${FAKE_MAKE_FAIL:-0}" = 0 ] || exit 23
exit 0
`)
	writeExecutable(t, fakeBin, "go", `
[ "$1" = version ] && [ "$2" = -m ] || exit 2
printf '  build vcs.revision=%s\n' "$FAKE_REVISION"
printf '  build vcs.modified=false\n'
`)
	writeExecutable(t, fakeBin, "sudo", `
printf 'sudo %s\n' "$*" >>"$FAKE_COMMAND_LOG"
if [ "$1" = -v ]; then exit 0; fi
if [ "$1" = -u ]; then shift 2; fi
exec "$@"
`)
	writeExecutable(t, fakeBin, "systemd-run", `
while [ "$#" -gt 0 ]; do
  case "$1" in
    --pipe|--wait|--collect|--quiet|--uid=*|--property=*) shift ;;
    *) break ;;
  esac
done
exec "$@"
`)
	writeExecutable(t, fakeBin, "systemctl", `
printf 'systemctl %s\n' "$*" >>"$FAKE_COMMAND_LOG"
case "$1" in
  show)
    if [ "$4" = User ]; then printf '%s\n' "$FAKE_SERVICE_USER"; else printf '%s\n' "$FAKE_SERVICE_GROUP"; fi
    ;;
  is-active)
    state=$(sed -n '1p' "$FAKE_STATE_FILE")
    if [ "$2" != --quiet ]; then printf '%s\n' "$state"; fi
    [ "$state" = active ]
    ;;
  stop)
    printf 'inactive\n' >"$FAKE_STATE_FILE"
    ;;
  start)
    if [ "${FAKE_FAIL_START_ONCE:-0}" = 1 ] && [ ! -e "$FAKE_FAIL_START_MARKER" ]; then
      : >"$FAKE_FAIL_START_MARKER"
      exit 1
    fi
    printf 'active\n' >"$FAKE_STATE_FILE"
    ;;
  status) printf 'fixture service status\n' ;;
  *) exit 2 ;;
esac
`)
	writeExecutable(t, fakeBin, "sqlite3", `
database=$1
sql=$2
case "$sql" in
  'PRAGMA user_version;') printf '%s\n' "$FAKE_ON_DISK_SCHEMA" ;;
  'PRAGMA quick_check;') printf 'ok\n' ;;
  SELECT\ COUNT*) printf '0\n' ;;
  .backup*)
    destination=$(printf '%s\n' "$sql" | sed -n "s/^\.backup '\(.*\)'$/\1/p")
    [ -n "$destination" ] || exit 3
    printf 'verified backup of %s\n' "$database" >"$destination"
    ;;
  *) printf 'unexpected SQL: %s\n' "$sql" >&2; exit 4 ;;
esac
`)
	writeExecutable(t, fakeBin, "findmnt", `
case "$*" in
  *SOURCE,FSTYPE,TARGET*) printf '/dev/fixture btrfs %s\n' "$FAKE_COLD_STORAGE" ;;
  *) printf 'btrfs\n' ;;
esac
`)
	writeExecutable(t, fakeBin, "journalctl", `
printf 'fixture journal: service start failed\n'
`)

	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	currentUser, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	currentGroup, err := user.LookupGroupId(currentUser.Gid)
	if err != nil {
		t.Fatal(err)
	}
	environment := append(filteredEnvironment(
		"PATH",
		"COLDSTORE_REPO_ROOT",
		"COLDSTORE_CONFIG",
		"COLDSTORE_INSTALLED_BINARY",
		"COLDSTORE_BACKUP_DIR",
		"COLDSTORE_DEPLOY_HEALTH_SECONDS",
		"COLDSTORE_DEPLOY_WAIT_SECONDS",
		"FAKE_COMMAND_LOG",
		"FAKE_STATE_FILE",
		"FAKE_FAIL_START_MARKER",
		"FAKE_COLD_STORAGE",
		"FAKE_ON_DISK_SCHEMA",
		"FAKE_REVISION",
		"FAKE_SERVICE_USER",
		"FAKE_SERVICE_GROUP",
	),
		"PATH="+fakeBin+":"+os.Getenv("PATH"),
		"COLDSTORE_REPO_ROOT="+repo,
		"COLDSTORE_CONFIG="+config,
		"COLDSTORE_INSTALLED_BINARY="+installed,
		"COLDSTORE_BACKUP_DIR="+backupRoot,
		"COLDSTORE_DEPLOY_HEALTH_SECONDS=0",
		"COLDSTORE_DEPLOY_WAIT_SECONDS=0",
		"FAKE_COMMAND_LOG="+commandLog,
		"FAKE_STATE_FILE="+stateFile,
		"FAKE_FAIL_START_MARKER="+failStartMarker,
		"FAKE_COLD_STORAGE="+coldStorage,
		"FAKE_ON_DISK_SCHEMA="+onDiskSchema,
		"FAKE_REVISION="+testRevision,
		"FAKE_SERVICE_USER="+currentUser.Username,
		"FAKE_SERVICE_GROUP="+currentGroup.Name,
	)

	return &redeployFixture{
		script:      filepath.Join(workingDirectory, "redeploy-simple"),
		repo:        repo,
		installed:   installed,
		backupRoot:  backupRoot,
		stateFile:   stateFile,
		commandLog:  commandLog,
		environment: environment,
	}
}

func (f *redeployFixture) run() (string, error) {
	command := exec.Command(f.script)
	command.Env = f.environment
	output, err := command.CombinedOutput()
	return string(output), err
}

func (f *redeployFixture) readLog(t *testing.T) string {
	t.Helper()
	return readFile(t, f.commandLog)
}

func writeExecutable(t *testing.T, directory, name, body string) {
	t.Helper()
	writeFile(t, filepath.Join(directory, name), "#!/bin/sh\nset -eu\n"+body, 0o755)
}

func writeFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	return string(mustRead(t, path))
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func filteredEnvironment(remove ...string) []string {
	dropped := make(map[string]bool, len(remove))
	for _, key := range remove {
		dropped[key] = true
	}
	result := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !dropped[key] {
			result = append(result, entry)
		}
	}
	return result
}
