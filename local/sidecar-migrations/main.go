// The sidecar migration groups' tools (§19.7, D-OPS-6), run from a server
// checkout:
//
//	go run ./local/sidecar-migrations promote MG1
//	go run ./local/sidecar-migrations check-tables
//
// promote is the one edit a Gn-out makes to register its group: it moves the
// group's entries, its marker last, from db_migrations_sidecar_pending.go into
// db_migrations.go's slice, ahead of the pending tail, and removes the group's
// functions from the pending file. It refuses a group while an earlier one is
// still pending, and prints the versions the group now holds, which never change
// again. It edits text and compiles nothing: build both legs and run the tests
// after it, and move the group's release-test names (db_migrations_sidecar_
// release_test.go) with it.
//
// check-tables compares the tables the groups create, pending or registered,
// with xops's sidecar-tables.conf, the one list behind D-OPS-18's dumps, and
// each line's dump or none with its §2.8 ttl: no dump holds a table whose ttl is
// at most 7 d. It reads the sibling checkouts, so it runs in the monoroot, as
// server/local/sidecar-gate runs it at each gate.
//
// Both read and write only the files named here, so neither needs a database,
// vault or the network.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Where check-tables reads the sibling checkouts, relative to the server
// directory.
const (
	defaultTablesConfPath   = "../xops/main/ansible/host_files/by-us-fmt-5-edge-6/db-archive/sidecar-tables.conf"
	defaultMetadataListPath = "../sidecar/metadata_list.yml"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// The command line, its output and its exit code: 0 done, 1 refused or failed,
// 64 usage.
func run(args []string, out io.Writer, errOut io.Writer) int {
	usage := func() int {
		fmt.Fprintf(errOut, "usage: sidecar-migrations promote [-server <dir>] MG<n>\n")
		fmt.Fprintf(errOut, "       sidecar-migrations check-tables [-server <dir>] [-tables <conf>] [-metadata <yml>]\n")
		return 64
	}
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "promote":
		flags := flag.NewFlagSet("promote", flag.ContinueOnError)
		flags.SetOutput(errOut)
		serverDirectory := flags.String("server", ".", "the server checkout")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 1 {
			return usage()
		}
		result, err := promote(*serverDirectory, flags.Arg(0))
		if err != nil {
			fmt.Fprintf(errOut, "promote refused: %s\n", err)
			return 1
		}
		fmt.Fprintf(out, "%s is registered at versions %d-%d. Build both legs, run the tests, and move its names out of db_migrations_sidecar_release_test.go.\n",
			result.groupName, result.firstVersion, result.lastVersion)
		return 0
	case "check-tables":
		flags := flag.NewFlagSet("check-tables", flag.ContinueOnError)
		flags.SetOutput(errOut)
		serverDirectory := flags.String("server", ".", "the server checkout")
		tablesConfPath := flags.String("tables", "", "sidecar-tables.conf; default the xops checkout beside the server's")
		metadataListPath := flags.String("metadata", "", "metadata_list.yml; default the sidecar checkout beside the server's")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return usage()
		}
		if *tablesConfPath == "" {
			*tablesConfPath = filepath.Join(*serverDirectory, defaultTablesConfPath)
		}
		if *metadataListPath == "" {
			*metadataListPath = filepath.Join(*serverDirectory, defaultMetadataListPath)
		}
		report, err := checkTables(*serverDirectory, *tablesConfPath, *metadataListPath)
		if err != nil {
			fmt.Fprintf(errOut, "check-tables could not read its inputs: %s\n", err)
			return 1
		}
		for _, finding := range report.findings {
			fmt.Fprintf(out, "FAIL %s\n", finding)
		}
		for _, note := range report.notes {
			fmt.Fprintf(out, "note %s\n", note)
		}
		if 0 < len(report.findings) {
			fmt.Fprintf(out, "check-tables: %d findings over %d tables the groups create\n", len(report.findings), report.createdTableCount)
			return 1
		}
		fmt.Fprintf(out, "check-tables: %d tables the groups create, each with its line and ttl\n", report.createdTableCount)
		return 0
	}
	return usage()
}
