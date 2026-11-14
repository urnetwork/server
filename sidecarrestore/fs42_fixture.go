package sidecarrestore

// FS-42's frame (D-OPS-14, D-OPS-18, FI-12; OPS-M3-04): a Postgres loss on T3, through the real dumps and the real
// restore, on which every FS-42 step runs, OPS-M3-04's at M3 and each later owner's at its milestone (DNS-M5-24,
// SOCK-M5-17, TLS-M5-26, TLS-M6-07, MAIL's M7 tasks, SDK-M8-12, SMTP-M9-19).
//
// A test seeds the primary, its own private database, through real handlers, then takes the two dumps at the times
// it chooses, in either order and with writes between them: TakeMainDump runs pg_dump as bringyour-pg-backup.sh runs
// it, with the main dump's exclude list from sidecar-tables.conf, and TakeSidecarDump runs xops's own
// bringyour-pg-sidecar-dump.sh and unpacks its generation as the runbook does. LosePrimary restores the main dump into
// a new database, loads the sidecar dump into it with xops's bringyour-pg-sidecar-restore.sh under the incident, and
// points the env's pools at it, as a restore that replaced the primary does. RunFrameThrough then runs the restore
// frame's steps in order up to a named one.
//
// The scripts run under this machine's bash, as xops's live tests run them, with the host-side tools faked (runuser,
// zfs, findmnt, df, chown, flock, timeout) and psql, pg_dump and pg_restore running in a client container of the
// private server's own image, beside the server, through BRINGYOUR_PG_BINDIR's wrappers. The container sees the
// fixture's directory at the same path. It needs Docker, gpg and python3, the private server sidecar-test-pg, and
// xops's sibling checkout; it dials only the private server's published port, refusing the loopback ports that may be
// production tunnels, and makes its own roles, which it drops. Times are the database's, to the microsecond, and the
// fixture never sleeps: dumps and writes are ordered by the calls that make them.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server"
)

// What the fixture needs of a test: testing.TB has it.
type PostgresLossT interface {
	Helper()
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
	Context() context.Context
}

// The fixture's tunables.
type PostgresLossSettings struct {
	// xops's db-archive directory, the scripts and lists the fixture runs. Empty finds it from the working directory
	// upward, as the monoroot's sibling checkout xops/main/ansible/host_files/by-us-fmt-5-edge-6/db-archive.
	XopsDbArchiveDir string
	// The private PostgreSQL's container, whose local socket trusts its postgres role: the roles and databases the
	// fixture makes.
	ServerContainer string
	// The image the client container runs: the server's own, so the tools match its major version.
	ClientImage string
	// The name the client container reaches this machine by.
	DockerHost string
	// Bounds each script and tool run. A dump of a test's database takes seconds; two minutes, as xops's live tests
	// allow.
	CommandTimeout time.Duration
	// pg_dump's and pg_restore's jobs, and the restore script's --jobs.
	Jobs int
	// The directory the fixture's own directory goes in. Empty is the system's temporary directory, which Docker
	// shares; gpg's agent socket under it must fit a Unix socket path.
	TempRoot string
}

// The defaults.
func DefaultPostgresLossSettings() *PostgresLossSettings {
	return &PostgresLossSettings{
		ServerContainer: "sidecar-test-pg",
		ClientImage:     "postgres:18",
		DockerHost:      "host.docker.internal",
		CommandTimeout:  2 * time.Minute,
		Jobs:            2,
	}
}

// The db-archive directory's path under the monoroot.
const xopsDbArchiveRelPath = "xops/main/ansible/host_files/by-us-fmt-5-edge-6/db-archive"

// What each fixture's roles and container are named after, unique in the process.
var postgresLossFixtureCount atomic.Int64

// One Postgres loss: its directory, client container, roles, dumps and restored database. See the file header.
type PostgresLossFixture struct {
	t        PostgresLossT
	settings *PostgresLossSettings

	root       string
	xopsDir    string
	pgbin      string
	fakeBin    string
	configDir  string
	gnupgDir   string
	archiveDir string
	client     string
	suffix     string

	// the primary as the env's pools reach it
	pgResource map[string]any
	pgHost     string
	pgPort     int
	sourceDb   string

	adminRole     string
	adminPassword string
	dumpRole      string
	dumpPassword  string
	guardRole     string

	mainDumpDir    string
	sidecarDumpDir string
	dumpCount      int

	targetDb  string
	popPools  func()
	incident  *Incident
	closeOnce sync.Once
	closes    []func()
}

// A fixture over the env's private database, the primary, with settings nil for the defaults. Close it before the
// test's env ends, as a defer in the test's body: it drops the restored database and the roles, stops the client
// container and puts the env's pools back.
func NewPostgresLossFixture(t PostgresLossT, settings *PostgresLossSettings) *PostgresLossFixture {
	t.Helper()
	if settings == nil {
		settings = DefaultPostgresLossSettings()
	}
	self := &PostgresLossFixture{t: t, settings: settings}
	ok := false
	defer func() {
		if !ok {
			self.Close()
		}
	}()
	self.suffix = fmt.Sprintf("%d_%d", os.Getpid(), postgresLossFixtureCount.Add(1))

	xopsDir, err := findXopsDbArchiveDir(settings.XopsDbArchiveDir)
	if err != nil {
		t.Fatalf("the fixture needs xops's sibling checkout: %v", err)
	}
	self.xopsDir = xopsDir

	resource := server.Vault.RequireSimpleResource(server.DefaultPgVaultResourceName)
	self.pgResource = map[string]any{}
	for key, value := range resource.Parse() {
		self.pgResource[key] = value
	}
	for _, key := range []string{"authority", "user", "password", "db"} {
		self.pgResource[key] = resource.RequireString(key)
	}
	host, portText, err := net.SplitHostPort(self.pgResource["authority"].(string))
	if err != nil {
		t.Fatalf("the env's Postgres authority: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("the env's Postgres port: %v", err)
	}
	// loopback's production tunnel ports are refused outright (server/test-env.sh)
	if port == 5432 || port == 6432 || (6379 <= port && port <= 6427) {
		t.Fatalf("port %d can be a production data tunnel; the fixture runs only on the private server", port)
	}
	self.pgHost, self.pgPort = host, port
	self.sourceDb = self.pgResource["db"].(string)

	root, err := os.MkdirTemp(settings.TempRoot, "scr-")
	if err != nil {
		t.Fatalf("%v", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("%v", err)
	}
	self.root = root
	self.closes = append(self.closes, func() { os.RemoveAll(root) })
	self.pgbin = filepath.Join(root, "pgbin")
	self.fakeBin = filepath.Join(root, "bin")
	self.configDir = filepath.Join(root, "config")
	self.gnupgDir = filepath.Join(root, "g")
	self.archiveDir = filepath.Join(root, "archive", "pg-sidecar")
	for _, dir := range []string{
		self.pgbin, self.fakeBin, self.configDir, self.gnupgDir,
		filepath.Join(self.archiveDir, "final"), filepath.Join(self.archiveDir, "verified"),
		filepath.Join(self.archiveDir, "retained", ".planetoid-verified"), filepath.Join(self.archiveDir, "incoming"),
		filepath.Join(self.archiveDir, "staging"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("%v", err)
		}
	}
	self.closes = append(self.closes, func() {
		// gpg starts an agent for the dump's encryption and the unpack's decryption: it ends with the fixture
		exec.Command("gpgconf", "--homedir", self.gnupgDir, "--kill", "all").Run()
	})

	self.adminRole = "sc_restore_admin_" + self.suffix
	self.dumpRole = "sc_restore_dump_" + self.suffix
	self.guardRole = "sc_restore_guard_" + self.suffix
	self.adminPassword = randomFixtureSecret()
	self.dumpPassword = randomFixtureSecret()
	self.serverSql(fmt.Sprintf(`
		CREATE ROLE %[1]s LOGIN SUPERUSER PASSWORD '%[2]s';
		CREATE ROLE %[3]s LOGIN NOSUPERUSER INHERIT NOCREATEROLE NOCREATEDB NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 10 PASSWORD '%[4]s';
		GRANT pg_read_all_data TO %[3]s;
		CREATE ROLE %[5]s NOLOGIN;
	`, self.adminRole, self.adminPassword, self.dumpRole, self.dumpPassword, self.guardRole))
	self.closes = append(self.closes, func() {
		self.serverSqlBestEffort(fmt.Sprintf("DROP ROLE IF EXISTS %s; DROP ROLE IF EXISTS %s; DROP ROLE IF EXISTS %s;", self.adminRole, self.dumpRole, self.guardRole))
	})

	self.client = "sidecar-test-restore-" + strings.ReplaceAll(self.suffix, "_", "-")
	if output, err := self.command(nil, "docker", "run", "-d", "--rm", "--name", self.client, "--add-host", settings.DockerHost+":host-gateway",
		"-v", root+":"+root, "--entrypoint", "sleep", settings.ClientImage, "infinity"); err != nil {
		t.Fatalf("the client container: %v: %s", err, output)
	}
	self.closes = append(self.closes, func() { exec.Command("docker", "rm", "-f", self.client).Run() })

	self.writeTools()
	ok = true
	return self
}

// Ends what the fixture started, in reverse, once.
func (self *PostgresLossFixture) Close() {
	self.closeOnce.Do(func() {
		for index := len(self.closes) - 1; 0 <= index; index -= 1 {
			self.closes[index]()
		}
	})
}

// A random value for the fixture's own roles, made here and dropped with them: never a real credential.
func randomFixtureSecret() string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		panic(err)
	}
	return hex.EncodeToString(bytes)
}

// xops's db-archive directory: dir when given, else the first <ancestor>/xops/... above the working directory.
func findXopsDbArchiveDir(dir string) (string, error) {
	if dir != "" {
		return filepath.Abs(dir)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for at := cwd; ; at = filepath.Dir(at) {
		candidate := filepath.Join(at, xopsDbArchiveRelPath)
		if info, err := os.Stat(filepath.Join(candidate, "bringyour-pg-sidecar-restore.sh")); err == nil && !info.IsDir() {
			return filepath.EvalSymlinks(candidate)
		}
		if filepath.Dir(at) == at {
			return "", fmt.Errorf("no %s above %s", xopsDbArchiveRelPath, cwd)
		}
	}
}

// Runs name with args and env added to this process's, bounded by the settings, and answers its output.
func (self *PostgresLossFixture) command(env []string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(self.t.Context(), self.settings.CommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, name, args...)
	command.Env = append(os.Environ(), env...)
	output, err := command.CombinedOutput()
	return string(output), err
}

// SQL as the private server's postgres role, on its local socket: the fixture's roles and databases.
func (self *PostgresLossFixture) serverSql(sql string) string {
	self.t.Helper()
	output, err := self.serverSqlOutput(sql)
	if err != nil {
		self.t.Fatalf("server sql: %v: %s", err, output)
	}
	return output
}

// serverSql, failing quietly: what a cleanup runs.
func (self *PostgresLossFixture) serverSqlBestEffort(sql string) {
	self.serverSqlOutput(sql)
}

// The output of sql as the postgres role.
func (self *PostgresLossFixture) serverSqlOutput(sql string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), self.settings.CommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "docker", "exec", "-i", self.settings.ServerContainer,
		"psql", "-U", "postgres", "-X", "-A", "-t", "-q", "-v", "ON_ERROR_STOP=1", "-f", "-")
	command.Stdin = strings.NewReader(sql)
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

// A conninfo for database as the fixture's admin, from the client container.
func (self *PostgresLossFixture) conninfo(database string) string {
	return fmt.Sprintf("host=%s port=%d dbname=%s user=%s sslmode=disable connect_timeout=30",
		self.settings.DockerHost, self.pgPort, database, self.adminRole)
}

// Writes an executable file.
func writeFixtureExecutable(t PostgresLossT, path string, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("%v", err)
	}
}

// The tools the scripts call: the PostgreSQL clients in the client container, and the host's fakes, as xops's live
// tests have them; and the scripts' config files.
func (self *PostgresLossFixture) writeTools() {
	t := self.t
	t.Helper()
	for _, tool := range []string{"psql", "pg_dump", "pg_restore"} {
		writeFixtureExecutable(t, filepath.Join(self.pgbin, tool), fmt.Sprintf(
			"#!/bin/sh\nexec docker exec -i -e PGPASSFILE=\"$PGPASSFILE\" -e PGPASSWORD=\"$PGPASSWORD\" -e PGOPTIONS=\"${PGOPTIONS:-}\" %s /usr/lib/postgresql/18/bin/%s \"$@\"\n",
			self.client, tool,
		))
	}
	for name, body := range map[string]string{
		"runuser": "#!/bin/sh\nshift 3\nexec \"$@\"\n",
		"zfs":     "#!/bin/sh\ncase $1 in\n    get) case \"$*\" in\n        *keystatus*) echo available ;;\n        *mounted*) echo yes ;;\n        *encryption*) echo aes-256-gcm ;;\n    esac ;;\nesac\n",
		"findmnt": "#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do\n    case $1 in\n        --mountpoint) mountpoint=$2; shift 2 ;;\n        *) shift ;;\n    esac\ndone\n[ \"$mountpoint\" = \"$STAGING_MOUNT\" ] && { echo \"zfs archive/bringyour/pg-sidecar-staging\"; exit 0; }\n[ \"$mountpoint\" = \"$ARCHIVE_MOUNT\" ] && { echo \"zfs archive/bringyour\"; exit 0; }\nexit 1\n",
		"df":      "#!/bin/sh\nprintf 'Filesystem 1024-blocks Used Available Capacity Mounted on\\n'\nprintf 'archive/bringyour 31104000000 1 30000000000 1%% /mnt/archive/bringyour\\n'\n",
		"chown":   "#!/bin/sh\nexit 0\n",
		"flock":   "#!/bin/sh\n[ \"$1\" = -n ]\n",
		"timeout": "#!/bin/sh\nshift\nexec \"$@\"\n",
	} {
		writeFixtureExecutable(t, filepath.Join(self.fakeBin, name), body)
	}
	if err := os.Chmod(self.gnupgDir, 0o700); err != nil {
		t.Fatalf("%v", err)
	}
	if err := os.WriteFile(filepath.Join(self.configDir, "encryption-passphrase"), []byte("synthetic test key\n"), 0o400); err != nil {
		t.Fatalf("%v", err)
	}
	pgpass := fmt.Sprintf("%s:%d:%s:%s:%s\n", self.settings.DockerHost, self.pgPort, self.sourceDb, self.dumpRole, self.dumpPassword)
	if err := os.WriteFile(filepath.Join(self.configDir, "pgpass"), []byte(pgpass), 0o600); err != nil {
		t.Fatalf("%v", err)
	}
	// the client container reads psql's scripts at the same path, so they sit under the shared root
	for _, name := range []string{"sidecar-named-rows.sql", "sidecar-restore-reconcile.sql"} {
		data, err := os.ReadFile(filepath.Join(self.xopsDir, name))
		if err != nil {
			t.Fatalf("%v", err)
		}
		if err := os.WriteFile(filepath.Join(self.root, name), data, 0o600); err != nil {
			t.Fatalf("%v", err)
		}
	}
}

// The env every tool run takes: the admin's password for the clients, and no other.
func (self *PostgresLossFixture) toolEnv() []string {
	return []string{"PGPASSFILE=", "PGPASSWORD=" + self.adminPassword}
}

// The database's time now, to the microsecond, in the form the dumps record.
func (self *PostgresLossFixture) databaseTime() string {
	self.t.Helper()
	output, err := self.command(self.toolEnv(), filepath.Join(self.pgbin, "psql"), "-X", "-A", "-t", "-q", "-d", self.conninfo(self.sourceDb),
		"-c", `SELECT to_char(clock_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')`)
	if err != nil {
		self.t.Fatalf("the database's time: %v: %s", err, output)
	}
	return strings.TrimSpace(output)
}

// The main dump of the primary now: pg_dump as bringyour-pg-backup.sh runs it, with sidecar-tables.conf's exclude
// list, and a backup-point.txt whose copy_ended is the database's time as the dump ended.
func (self *PostgresLossFixture) TakeMainDump() {
	t := self.t
	t.Helper()
	exclude, err := self.command(nil, "python3", "-I", filepath.Join(self.xopsDir, "db-archive.py"), "sidecar-tables",
		"--file", filepath.Join(self.xopsDir, "sidecar-tables.conf"), "--list", "exclude")
	if err != nil {
		t.Fatalf("the exclude list: %v: %s", err, exclude)
	}
	self.dumpCount += 1
	dir := filepath.Join(self.root, fmt.Sprintf("main-%d", self.dumpCount))
	args := append([]string{"--format=directory", fmt.Sprintf("--jobs=%d", self.settings.Jobs), "--compress=zstd:3", "--file=" + dir},
		strings.Fields(exclude)...)
	args = append(args, "--dbname="+self.conninfo(self.sourceDb))
	if output, err := self.command(self.toolEnv(), filepath.Join(self.pgbin, "pg_dump"), args...); err != nil {
		t.Fatalf("the main dump: %v: %s", err, output)
	}
	copyEnded := self.databaseTime()
	if err := os.WriteFile(filepath.Join(dir, "backup-point.txt"), []byte("copy_ended "+copyEnded+"\n"), 0o600); err != nil {
		t.Fatalf("%v", err)
	}
	self.mainDumpDir = dir
	t.Logf("main dump at %s", copyEnded)
}

// The sidecar dump of the primary now: xops's bringyour-pg-sidecar-dump.sh publishes a generation, and the fixture
// unpacks it as the runbook does, its sha256, gpg and tar.
func (self *PostgresLossFixture) TakeSidecarDump() {
	t := self.t
	t.Helper()
	// the guard's conditions are clear now, as pgbackup-slot-guard leaves them
	self.serverSql(fmt.Sprintf(
		`DO $$ BEGIN EXECUTE format('COMMENT ON ROLE %s IS %%L', 'pgbackup-slot-guard time=' || extract(epoch FROM now())::bigint || ' disk=clear cpu=clear lag=clear'); END $$`,
		self.guardRole,
	))
	self.dumpCount += 1
	name := fmt.Sprintf("2026-11-06-02-30-%02d", self.dumpCount)
	groupOutput, err := self.command(nil, "id", "-gn")
	if err != nil {
		t.Fatalf("%v", err)
	}
	env := []string{
		"BRINGYOUR_PG_ARCHIVE_PATH=" + self.archiveDir,
		"BRINGYOUR_PG_ARCHIVE_MOUNT=" + filepath.Dir(self.archiveDir),
		"BRINGYOUR_PG_ARCHIVE_DATASET=archive/bringyour",
		"BRINGYOUR_PG_STAGING_DATASET=archive/bringyour/pg-sidecar-staging",
		"BRINGYOUR_PG_CONFIG_PATH=" + self.configDir,
		"BRINGYOUR_DB_ARCHIVE_HELPER=" + filepath.Join(self.xopsDir, "db-archive.py"),
		"BRINGYOUR_PG_SIDECAR_TABLES=" + filepath.Join(self.xopsDir, "sidecar-tables.conf"),
		"BRINGYOUR_PG_SIDECAR_NAMED_ROWS_SQL=" + filepath.Join(self.root, "sidecar-named-rows.sql"),
		"BRINGYOUR_PG_BINDIR=" + self.pgbin,
		"BRINGYOUR_PG_SOURCE_HOST=" + self.settings.DockerHost,
		"BRINGYOUR_PG_SOURCE_PORT=" + strconv.Itoa(self.pgPort),
		"BRINGYOUR_PG_SOURCE_DB=" + self.sourceDb,
		"BRINGYOUR_PG_SOURCE_USER=" + self.dumpRole,
		"BRINGYOUR_PG_SOURCE_SSLMODE=disable",
		"BRINGYOUR_PG_REPLICATION_USER=" + self.guardRole,
		"BRINGYOUR_PG_SIDECAR_MIN_FREE_BYTES=1000000",
		"BRINGYOUR_PG_SIDECAR_JOBS=" + strconv.Itoa(self.settings.Jobs),
		"BRINGYOUR_PG_BACKUP_GROUP=" + strings.TrimSpace(groupOutput),
		"BRINGYOUR_PG_BACKUP_NAME=" + name,
		"GNUPGHOME=" + self.gnupgDir,
		"STAGING_MOUNT=" + filepath.Join(self.archiveDir, "staging"),
		"ARCHIVE_MOUNT=" + filepath.Dir(self.archiveDir),
		"COPYFILE_DISABLE=1",
		"PGPASSFILE=" + filepath.Join(self.configDir, "pgpass"),
		"PGPASSWORD=",
		"PATH=" + self.fakeBin + ":" + os.Getenv("PATH"),
	}
	if output, err := self.command(env, "bash", filepath.Join(self.xopsDir, "bringyour-pg-sidecar-dump.sh")); err != nil {
		t.Fatalf("the sidecar dump: %v: %s", err, output)
	}

	generation := "main-pg-sidecar-" + name + ".dir.tar"
	artifact := filepath.Join(self.archiveDir, "final", generation+".gpg")
	sums, err := os.ReadFile(artifact + ".sha256")
	if err != nil {
		t.Fatalf("the generation's sha256: %v", err)
	}
	fields := strings.Fields(string(sums))
	artifactBytes, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatalf("the generation: %v", err)
	}
	sum := sha256.Sum256(artifactBytes)
	if len(fields) != 2 || fields[0] != hex.EncodeToString(sum[:]) || fields[1] != filepath.Base(artifact) {
		t.Fatalf("the generation's sha256 does not check")
	}
	into := filepath.Join(self.root, fmt.Sprintf("sidecar-%d", self.dumpCount))
	if err := os.Mkdir(into, 0o700); err != nil {
		t.Fatalf("%v", err)
	}
	unpack := fmt.Sprintf("gpg --batch --quiet --decrypt --pinentry-mode loopback --passphrase-file %q %q | tar -xf - -C %q",
		filepath.Join(self.configDir, "encryption-passphrase"), artifact, into)
	if output, err := self.command([]string{"GNUPGHOME=" + self.gnupgDir}, "bash", "-o", "pipefail", "-c", unpack); err != nil {
		t.Fatalf("the generation does not unpack: %v: %s", err, output)
	}
	self.sidecarDumpDir = filepath.Join(into, "main-pg-sidecar-"+name+".dir")
	t.Logf("sidecar dump %s", name)
}

// Loses the primary and restores it under incidentId: the newest main dump into a new database, then the newest
// sidecar dump through xops's restore script, which reconciles the rows by the dumps' times, marks the database and
// starts the incident's list; then points the env's pools at the restored database. Answers the incident, whose
// kind is postgres.
func (self *PostgresLossFixture) LosePrimary(incidentId server.Id) *Incident {
	t := self.t
	t.Helper()
	if self.mainDumpDir == "" || self.sidecarDumpDir == "" {
		t.Fatalf("take both dumps before the primary is lost")
	}
	if self.targetDb != "" {
		t.Fatalf("the primary is already lost")
	}
	self.targetDb = "scr_" + self.suffix
	self.serverSql(fmt.Sprintf(`CREATE DATABASE %s OWNER %s`, self.targetDb, quoteFixtureIdentifier(self.pgResource["user"].(string))))
	self.closes = append(self.closes, func() {
		self.serverSqlBestEffort(fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", self.targetDb))
	})
	if output, err := self.command(self.toolEnv(), filepath.Join(self.pgbin, "pg_restore"), "--exit-on-error",
		fmt.Sprintf("--jobs=%d", self.settings.Jobs), "--dbname="+self.conninfo(self.targetDb), self.mainDumpDir); err != nil {
		t.Fatalf("the main dump does not restore: %v: %s", err, output)
	}
	listPath := filepath.Join(self.root, "incident-"+incidentId.String()+".tsv")
	env := append(self.toolEnv(),
		"BRINGYOUR_PG_BINDIR="+self.pgbin,
		"BRINGYOUR_DB_ARCHIVE_HELPER="+filepath.Join(self.xopsDir, "db-archive.py"),
		"BRINGYOUR_PG_SIDECAR_RECONCILE_SQL="+filepath.Join(self.root, "sidecar-restore-reconcile.sql"),
	)
	output, err := self.command(env, "bash", filepath.Join(self.xopsDir, "bringyour-pg-sidecar-restore.sh"),
		"--dump", self.sidecarDumpDir, "--main-point", filepath.Join(self.mainDumpDir, "backup-point.txt"),
		"--incident", incidentId.String(), "--dbname", self.conninfo(self.targetDb), "--jobs", strconv.Itoa(self.settings.Jobs),
		"--report", listPath)
	if err != nil {
		t.Fatalf("the sidecar dump does not restore: %v: %s", err, output)
	}
	t.Logf("%s", output)
	self.pointPools(self.targetDb)
	incident, err := OpenIncident(incidentId, listPath, nil)
	if err != nil {
		t.Fatalf("the incident's list: %v", err)
	}
	if err := incident.SetKind(IncidentKindPostgres); err != nil {
		t.Fatalf("%v", err)
	}
	self.incident = incident
	return incident
}

// A Postgres identifier, quoted.
func quoteFixtureIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

// Points the env's application and maintenance pools at database until Close.
func (self *PostgresLossFixture) pointPools(database string) {
	t := self.t
	t.Helper()
	resource := map[string]any{}
	for key, value := range self.pgResource {
		resource[key] = value
	}
	resource["db"] = database
	data, err := yaml.Marshal(resource)
	if err != nil {
		t.Fatalf("%v", err)
	}
	popPg := server.Vault.PushSimpleResource(server.DefaultPgVaultResourceName, data)
	popMaintenance := server.Vault.PushSimpleResource(server.MaintenancePgVaultResourceName, data)
	server.PgReset()
	self.closes = append(self.closes, func() {
		popMaintenance()
		popPg()
		server.PgReset()
	})
}

// The restored database's name, empty before LosePrimary.
func (self *PostgresLossFixture) RestoredDatabase() string {
	return self.targetDb
}

// What sql answers on database as the fixture's admin, one row a line, its columns split by |.
func (self *PostgresLossFixture) Query(database string, sql string) []string {
	self.t.Helper()
	output, err := self.command(self.toolEnv(), filepath.Join(self.pgbin, "psql"), "-X", "-A", "-t", "-q", "-v", "ON_ERROR_STOP=1",
		"-d", self.conninfo(database), "-c", sql)
	if err != nil {
		self.t.Fatalf("query: %v: %s", err, output)
	}
	lines := []string{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// The primary's database, which the env's pools reached before the loss.
func (self *PostgresLossFixture) PrimaryDatabase() string {
	return self.sourceDb
}

// Runs the frame's steps of the restored incident in order through last, each twice, so a step that double-applies
// shows in its own second run: a step with a registered function runs, and one without is ended by hand with the
// statement byHand gives it, or the fixture's. after runs after each step's second run, for FI-12's checks.
func (self *PostgresLossFixture) RunFrameThrough(ctx context.Context, frame *Frame, last string, byHand map[string]string, out io.Writer, after func(step string)) error {
	if self.incident == nil {
		return errors.New("the primary is not lost yet")
	}
	steps, err := frame.incidentSteps(self.incident)
	if err != nil {
		return err
	}
	for _, step := range steps {
		if step.Function == nil {
			statement := byHand[step.Name]
			if statement == "" {
				statement = "not built at this milestone; the drill ran nothing for it"
			}
			if err := frame.EndStepByHand(self.incident, step.Name, statement); err != nil {
				return err
			}
		} else {
			for range 2 {
				if err := frame.RunStep(ctx, self.incident, step.Name, out, byHand[step.Name]); err != nil {
					return err
				}
			}
		}
		if after != nil {
			after(step.Name)
		}
		if step.Name == last {
			return nil
		}
	}
	return fmt.Errorf("%w: %q is not a step of the incident", ErrStepUnknown, last)
}
