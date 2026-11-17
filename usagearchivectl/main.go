// usagearchivectl exports weeks of st_provider_usage_archive from the Main
// primary to the backup host's storage, verifies each copy where it is
// stored, records it in the export ledger, and restores a week from its copy
// (package usagearchive). It runs on the backup host, outside warp, and
// connects directly to PostgreSQL with a pg_maintenance.yml-shaped file
// (user, password, authority, db).
//
// It never deletes a row: weeks leave PostgreSQL only through the server's
// drop fence, under the export control (server/db_provider_usage_archive_export.go).
//
//	usagearchivectl export --pg=<yml> --dir=<dir> [--floor=<date>] [--max-weeks=<n>] [--week=<date>] [--register] [--location=<where>] [--threads=<n>] [--plan]
//	usagearchivectl verify --dir=<dir> [--manifest=<path>]
//	usagearchivectl register --pg=<yml> --location=<where> <manifest>...
//	usagearchivectl restore --pg=<yml> --manifest=<path> --table=<name>
//	usagearchivectl status --pg=<yml> [--dir=<dir>]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server/usagearchive"
)

const usage = `usagearchivectl: export, verify, register and restore weeks of st_provider_usage_archive.

  export    export the weeks that may leave PostgreSQL and are not exported as they are now
  verify    read every stored copy back and match it against its manifest
  register  record verified exports in the ledger (after its migration is applied)
  restore   load one export into a new table and check it against its manifest
  status    the week leaves, their rows now, and their newest exports

Run "usagearchivectl <command> -h" for a command's flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "export":
		err = runExport(ctx, os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "register":
		err = runRegister(ctx, os.Args[2:])
	case "restore":
		err = runRestore(ctx, os.Args[2:])
	case "status":
		err = runStatus(ctx, os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "usagearchivectl %s: %s\n", os.Args[1], err)
		os.Exit(1)
	}
}

// A pg_maintenance.yml-shaped file: user, password, authority (host:port), db.
type pgResource struct {
	User      string `yaml:"user"`
	Password  string `yaml:"password"`
	Authority string `yaml:"authority"`
	Db        string `yaml:"db"`
}

// Connects with the resource at path. The password goes into the config, never
// into a string that is printed.
func connect(ctx context.Context, path string) (*pgx.Conn, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	resource := pgResource{}
	if err := yaml.Unmarshal(data, &resource); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if resource.User == "" || resource.Authority == "" || resource.Db == "" {
		return nil, fmt.Errorf("%s needs user, authority and db", path)
	}
	host, port, found := strings.Cut(resource.Authority, ":")
	if !found {
		port = "5432"
	}
	config, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=%s dbname=%s sslmode=prefer connect_timeout=10 application_name=usagearchivectl", host, port, resource.Db))
	if err != nil {
		return nil, err
	}
	config.User = resource.User
	config.Password = resource.Password
	return pgx.ConnectConfig(ctx, config)
}

// One run at a time per directory.
func lockDir(dir string) (func(), error) {
	file, err := os.OpenFile(filepath.Join(dir, ".usagearchivectl.lock"), os.O_RDWR|os.O_CREATE, 0o640)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("another usagearchivectl holds %s", dir)
	}
	return func() {
		unix.Flock(int(file.Fd()), unix.LOCK_UN)
		file.Close()
	}, nil
}

type weekList []string

func (self *weekList) String() string     { return strings.Join(*self, ",") }
func (self *weekList) Set(v string) error { *self = append(*self, v); return nil }

func runExport(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("export", flag.ContinueOnError)
	pgPath := flags.String("pg", "/srv/warp/main/vault/pg_maintenance.yml", "the PostgreSQL resource")
	dir := flags.String("dir", "", "where the copies go")
	floor := flags.String("floor", usagearchive.DefaultCloseTimeFloor.Format(time.DateOnly), "the close time floor: only weeks that end by it and whose rows all closed before it are exported (never later than 70 days ago)")
	maxWeeks := flags.Int("max-weeks", 4, "weeks exported at most in this run")
	var weeks weekList
	flags.Var(&weeks, "week", "export only this week (its Monday, YYYY-MM-DD); repeatable")
	register := flags.Bool("register", true, "record each verified export in the ledger, when the ledger exists")
	location := flags.String("location", "", "where the copies are kept, for the ledger (default <host>:<dir>)")
	threads := flags.Int("threads", 2, "zstd threads")
	minFree := flags.Uint64("min-free-bytes", 1000000000000, "stop before a week while --free-path's filesystem has less than this free")
	freePath := flags.String("free-path", "", "the filesystem whose free space --min-free-bytes guards (default --dir)")
	requireMountpoint := flags.Bool("require-mountpoint", false, "refuse to run unless --dir is a mount point of its own, such as a dataset that must be mounted")
	plan := flags.Bool("plan", false, "print what would be exported and export nothing")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("--dir is required")
	}
	floorTime, err := time.Parse(time.DateOnly, *floor)
	if err != nil {
		return fmt.Errorf("--floor: %w", err)
	}
	host, _ := os.Hostname()
	if *location == "" {
		*location = host + ":" + *dir
	}
	if *freePath == "" {
		*freePath = *dir
	}
	if *requireMountpoint {
		if err := requireOwnMount(*dir); err != nil {
			return err
		}
	}
	unlock, err := lockDir(*dir)
	if err != nil {
		return err
	}
	defer unlock()
	conn, err := connect(ctx, *pgPath)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))

	effectiveFloor := usagearchive.EffectiveCloseTimeFloor(floorTime, time.Now())
	leaves, err := usagearchive.ListWeekLeaves(ctx, conn)
	if err != nil {
		return err
	}
	manifests, err := usagearchive.ListManifests(*dir)
	if err != nil {
		return err
	}
	ledgerExists, err := usagearchive.LedgerExists(ctx, conn)
	if err != nil {
		return err
	}
	fmt.Printf("floor %s (effective %s), %d week leaves, ledger present: %t\n", floorTime.Format(time.DateOnly), effectiveFloor.Format(time.RFC3339), len(leaves), ledgerExists)
	if *register && ledgerExists {
		// copies verified before the ledger existed, or whose registration
		// failed, are recorded now; each is read back once more first
		written, err := usagearchive.RegisterStoredCopies(ctx, conn, *dir, *location)
		for _, file := range written {
			fmt.Printf("earlier copy %s: ledger row written\n", file)
		}
		if err != nil {
			return err
		}
	}
	exported := 0
	for _, leaf := range leaves {
		if 0 < len(weeks) && !containsString(weeks, leaf.WeekStart.Format(time.DateOnly)) {
			continue
		}
		weekEnd := leaf.WeekStart.AddDate(0, 0, 7)
		if effectiveFloor.Before(weekEnd) {
			continue
		}
		candidate, reason, err := usagearchive.ExportCandidate(ctx, conn, leaf, effectiveFloor, manifests[leaf.Leaf])
		if err != nil {
			return fmt.Errorf("%s: %w", leaf.Leaf, err)
		}
		if !candidate {
			fmt.Printf("%s: skip, %s\n", leaf.Leaf, reason)
			continue
		}
		if exported == *maxWeeks {
			fmt.Printf("%s: left for the next run (--max-weeks %d)\n", leaf.Leaf, *maxWeeks)
			continue
		}
		if free, err := freeBytes(*freePath); err != nil {
			return err
		} else if free < *minFree {
			return fmt.Errorf("%s has %d bytes free, under --min-free-bytes %d; %s waits", *freePath, free, *minFree, leaf.Leaf)
		}
		if *plan {
			fmt.Printf("%s: would export (%s)\n", leaf.Leaf, reason)
			exported++
			continue
		}
		start := time.Now()
		manifest, err := usagearchive.ExportWeek(ctx, conn, leaf, &usagearchive.ExportSettings{Dir: *dir, Host: host, Concurrency: *threads})
		if err != nil {
			return err
		}
		exported++
		fmt.Printf("%s: exported and verified %d rows, %d bytes as %s (%d bytes, sha256 %s) in %s\n",
			leaf.Leaf, manifest.Rows, manifest.CopyBytes, manifest.File, manifest.FileBytes, manifest.FileSha256, time.Since(start).Round(time.Second))
		if *register && ledgerExists {
			manifestPath := filepath.Join(*dir, strings.TrimSuffix(manifest.File, ".copy.zst")+".manifest.json")
			if _, registered, err := usagearchive.Register(ctx, conn, manifestPath, *location); err != nil {
				return fmt.Errorf("register %s: %w", manifest.File, err)
			} else {
				fmt.Printf("%s: ledger row written: %t\n", leaf.Leaf, registered)
			}
		}
	}
	return nil
}

func containsString(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func runVerify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir := flags.String("dir", "", "the copies' directory")
	requireMountpoint := flags.Bool("require-mountpoint", false, "refuse to run unless --dir is a mount point of its own")
	only := flags.String("manifest", "", "verify only this manifest")
	if err := flags.Parse(args); err != nil {
		return err
	}
	paths := []string{}
	if *only != "" {
		paths = append(paths, *only)
	} else {
		if *dir == "" {
			return errors.New("--dir or --manifest is required")
		}
		if *requireMountpoint {
			if err := requireOwnMount(*dir); err != nil {
				return err
			}
		}
		matches, err := filepath.Glob(filepath.Join(*dir, "st_provider_usage_archive_w*.manifest.json"))
		if err != nil {
			return err
		}
		paths = matches
	}
	sort.Strings(paths)
	failed := 0
	for _, path := range paths {
		manifest, err := usagearchive.VerifyManifest(path)
		if err != nil {
			failed++
			fmt.Printf("FAIL %s: %s\n", path, err)
			continue
		}
		fmt.Printf("ok %s: %d rows, sha256 %s\n", manifest.File, manifest.Rows, manifest.FileSha256)
	}
	if 0 < failed {
		return fmt.Errorf("%d of %d copies do not verify", failed, len(paths))
	}
	return nil
}

func runRegister(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("register", flag.ContinueOnError)
	pgPath := flags.String("pg", "/srv/warp/main/vault/pg_maintenance.yml", "the PostgreSQL resource")
	location := flags.String("location", "", "where the copies are kept")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *location == "" || flags.NArg() == 0 {
		return errors.New("--location and at least one manifest are required")
	}
	conn, err := connect(ctx, *pgPath)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	for _, path := range flags.Args() {
		manifest, registered, err := usagearchive.Register(ctx, conn, path, *location)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		fmt.Printf("%s: %d rows, ledger row written: %t\n", manifest.File, manifest.Rows, registered)
	}
	return nil
}

func runRestore(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	pgPath := flags.String("pg", "/srv/warp/main/vault/pg_maintenance.yml", "the PostgreSQL resource")
	manifestPath := flags.String("manifest", "", "the export to restore")
	table := flags.String("table", "", "the new table to restore into")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *manifestPath == "" || *table == "" {
		return errors.New("--manifest and --table are required")
	}
	conn, err := connect(ctx, *pgPath)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	report, err := usagearchive.Restore(ctx, conn, *manifestPath, *table)
	if err != nil {
		return err
	}
	manifest, err := usagearchive.ReadManifest(*manifestPath)
	if err != nil {
		return err
	}
	fmt.Printf("restored %d rows into %s; digest %s matches the export\n", report.Rows, report.Table, report.RowDigest)
	if 0 < report.DefaultRowsInWeek {
		fmt.Printf("the DEFAULT partition holds %d rows inside the week: ATTACH refuses the table until they move\n", report.DefaultRowsInWeek)
	}
	fmt.Println("to make it the week's partition again, the owner runs:")
	for _, statement := range usagearchive.AttachSql(manifest, *table) {
		fmt.Printf("  %s;\n", statement)
	}
	return nil
}

func runStatus(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	pgPath := flags.String("pg", "/srv/warp/main/vault/pg_maintenance.yml", "the PostgreSQL resource")
	dir := flags.String("dir", "", "the copies' directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	conn, err := connect(ctx, *pgPath)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	leaves, err := usagearchive.ListWeekLeaves(ctx, conn)
	if err != nil {
		return err
	}
	exports := map[string]*usagearchive.LedgerExport{}
	if exists, err := usagearchive.LedgerExists(ctx, conn); err != nil {
		return err
	} else if exists {
		if exports, err = usagearchive.LatestExports(ctx, conn); err != nil {
			return err
		}
	}
	manifests := map[string][]*usagearchive.Manifest{}
	if *dir != "" {
		if manifests, err = usagearchive.ListManifests(*dir); err != nil {
			return err
		}
	}
	for _, leaf := range leaves {
		line := leaf.Leaf
		if export := exports[leaf.Leaf]; export != nil {
			line += fmt.Sprintf(" ledger: %d rows in %s", export.RowCount, export.FileName)
		}
		if local := manifests[leaf.Leaf]; 0 < len(local) {
			line += fmt.Sprintf(" stored: %d copies, newest %s", len(local), local[len(local)-1].File)
		}
		fmt.Println(line)
	}
	for leaf, export := range exports {
		if export.DroppedTime != nil {
			fmt.Printf("%s dropped from PostgreSQL at %s; its copy is %s (sha256 %s)\n", leaf, export.DroppedTime.Format(time.RFC3339), export.FileName, export.FileSha256)
		}
	}
	return nil
}

// Bytes an unprivileged writer can still use on dir's filesystem.
func freeBytes(dir string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(dir, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}

// dir must be a mount point: its filesystem differs from its parent's. An
// encrypted dataset that is not mounted leaves an empty directory of the parent
// filesystem in its place, which must never receive a copy.
func requireOwnMount(dir string) error {
	var self, parent unix.Stat_t
	if err := unix.Stat(dir, &self); err != nil {
		return err
	}
	if err := unix.Stat(filepath.Dir(filepath.Clean(dir)), &parent); err != nil {
		return err
	}
	if self.Dev == parent.Dev {
		return fmt.Errorf("%s is not mounted: it is a directory of its parent's filesystem", dir)
	}
	return nil
}
