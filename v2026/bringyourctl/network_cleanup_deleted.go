// bringyourctl network cleanup-deleted removes what accounts deleted before
// 2026-11-16 left behind, by the rules the account deletion now follows
// (model.RemoveDeletedNetworkData, whose tables say what goes and what
// stays): until then a deletion removed the network, its users and their
// sign-ins, and nothing else.
//
// The dry run (the default) runs in a read-only process. It finds the deleted
// networks, the network ids that rows still name with no network row
// (model.FindDeletedNetworkIds: one short statement per page of distinct ids),
// writes them sorted to --ids_file, counts what they left by table in short
// read-only transactions of --count_chunk networks, and prints the digest of
// the list.
//
// The commit (--commit --expect=<digest>) reads the list back from --ids_file
// (or --network_ids), checks the digest the dry run printed, and cleans the
// networks in id order, --count_chunk at a time with --network_pause between
// chunks. The networks of a chunk with no client or device left lose their
// rows together (model.RemoveDeletedNetworksRows); the others are cleaned one
// at a time (model.RemoveDeletedNetworkData). Every step is its own short
// transaction and re-checks that the network row is gone. A network whose
// clients the commit deactivates keeps them, inactive, for the client delay
// (24 hours); the commit queues the network's cleanup task for the time they
// are due, and the taskworker finishes them. Each chunk ends with the id to
// pass to --start_after to resume after it; --max_networks stops early.
//
// --count_statement_timeout bounds each count of the dry run: every count runs
// under its own savepoint and statement timeout, and a count past it is
// reported as timed out for its table and network set while the counting goes
// on. --run_tag names the run: every direct session of it, dry or commit (the
// maintenance pool, where all the counting and deleting runs, and every pool
// of the read-only dry run), carries the application_name "cleanup-deleted
// <tag>" (by default the tag is pid<pid>), so an operator who stops the run
// can cancel exactly its backends. Sessions through PgBouncer keep the default
// name: their server connections serve other clients too.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// The digest names the exact list of networks a dry run printed.
const networkCleanupDeletedDigestVersion = "urnetwork network cleanup-deleted v1"

type networkCleanupDeletedOptions struct {
	networkIds   []server.Id
	idsFile      string
	pageCount    int
	countChunk   int
	pause        time.Duration
	startAfter   *server.Id
	maxNetworks  int
	cleanup      *model.DeletedNetworkCleanupSettings
	commit       bool
	expectDigest string
	// names every session of the run (networkCleanupDeletedApplicationName)
	runTag string
}

// The application_name of every direct session of a run, dry or commit, so
// that an operator who stops the run can find and cancel exactly its backends:
// `SELECT pg_cancel_backend(pid) FROM pg_stat_activity WHERE
// application_name = '<this name>'`.
func networkCleanupDeletedApplicationName(runTag string) string {
	return "cleanup-deleted " + runTag
}

// A run tag fits in application_name (63 bytes) with its prefix and holds
// nothing an operator must quote.
func validNetworkCleanupDeletedRunTag(runTag string) bool {
	if runTag == "" || 40 < len(runTag) {
		return false
	}
	for _, c := range runTag {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func networkCleanupDeletedCommand(opts docopt.Opts) int {
	options, err := parseNetworkCleanupDeletedOptions(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "network cleanup-deleted: %s\n", err)
		return 1
	}
	applicationName := networkCleanupDeletedApplicationName(options.runTag)
	server.SetPgDirectApplicationName(applicationName)
	fmt.Fprintf(os.Stdout, "application_name: %s\n", applicationName)
	// the dry run writes nothing: its process is read only, so PostgreSQL
	// refuses any write on its path
	if !options.commit {
		server.RequirePgReadOnly()
	}
	return runNetworkCleanupDeleted(context.Background(), os.Stdout, os.Stderr, options)
}

func parseNetworkCleanupDeletedOptions(opts docopt.Opts) (*networkCleanupDeletedOptions, error) {
	options := &networkCleanupDeletedOptions{
		pageCount:  10000,
		countChunk: 200,
		pause:      200 * time.Millisecond,
		cleanup:    model.DefaultDeletedNetworkCleanupSettings(),
	}
	networkIdsStr, _ := opts.String("--network_ids")
	for _, part := range strings.Split(networkIdsStr, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		networkId, err := server.ParseId(part)
		if err != nil {
			return nil, fmt.Errorf("--network_ids: %q: %s", part, err)
		}
		options.networkIds = append(options.networkIds, networkId)
	}
	options.idsFile, _ = opts.String("--ids_file")
	positive := func(name string, value *int) error {
		str, _ := opts.String(name)
		if str == "" {
			return nil
		}
		n, err := strconv.Atoi(str)
		if err != nil || n <= 0 {
			return fmt.Errorf("%s: %q is not a positive count", name, str)
		}
		*value = n
		return nil
	}
	for name, value := range map[string]*int{
		"--page":         &options.pageCount,
		"--count_chunk":  &options.countChunk,
		"--client_batch": &options.cleanup.ClientBatchCount,
		"--row_batch":    &options.cleanup.RowBatchCount,
		"--max_networks": &options.maxNetworks,
	} {
		if err := positive(name, value); err != nil {
			return nil, err
		}
	}
	if pauseStr, _ := opts.String("--network_pause"); pauseStr != "" {
		pause, err := time.ParseDuration(pauseStr)
		if err != nil || pause < 0 {
			return nil, fmt.Errorf("--network_pause: %q is not a duration", pauseStr)
		}
		options.pause = pause
	}
	if startAfterStr, _ := opts.String("--start_after"); startAfterStr != "" {
		startAfter, err := server.ParseId(startAfterStr)
		if err != nil {
			return nil, fmt.Errorf("--start_after: %q: %s", startAfterStr, err)
		}
		options.startAfter = &startAfter
	}
	options.commit, _ = opts.Bool("--commit")
	options.expectDigest, _ = opts.String("--expect")
	if options.commit && len(options.networkIds) == 0 && options.idsFile == "" {
		return nil, errors.New("--commit needs the list the dry run wrote (--ids_file) or --network_ids")
	}
	if timeoutStr, _ := opts.String("--count_statement_timeout"); timeoutStr != "" {
		if options.commit {
			return nil, errors.New("--count_statement_timeout bounds the dry run's counts; the commit does not count")
		}
		timeout, err := time.ParseDuration(timeoutStr)
		if err != nil || timeout <= 0 {
			return nil, fmt.Errorf("--count_statement_timeout: %q is not a positive duration", timeoutStr)
		}
		options.cleanup.CountStatementTimeout = timeout
	}
	options.runTag = fmt.Sprintf("pid%d", os.Getpid())
	if runTag, _ := opts.String("--run_tag"); runTag != "" {
		if !validNetworkCleanupDeletedRunTag(runTag) {
			return nil, fmt.Errorf("--run_tag: %q is not 1-40 letters, digits, '-', '_' or '.'", runTag)
		}
		options.runTag = runTag
	}
	return options, nil
}

// The digest of a sorted network id list.
func networkCleanupDeletedDigest(networkIds []server.Id) string {
	hash := sha256.New()
	hash.Write([]byte(networkCleanupDeletedDigestVersion))
	for _, networkId := range networkIds {
		hash.Write([]byte{'\n'})
		hash.Write([]byte(networkId.String()))
	}
	return hex.EncodeToString(hash.Sum(nil))[:16]
}

func sortedUniqueNetworkIds(networkIds []server.Id) []server.Id {
	set := map[server.Id]bool{}
	for _, networkId := range networkIds {
		set[networkId] = true
	}
	return slices.SortedFunc(maps.Keys(set), func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})
}

func readNetworkIdsFile(path string) ([]server.Id, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	networkIds := []server.Id{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		networkId, err := server.ParseId(line)
		if err != nil {
			return nil, fmt.Errorf("%s: %q: %s", path, line, err)
		}
		networkIds = append(networkIds, networkId)
	}
	return networkIds, scanner.Err()
}

func writeNetworkIdsFile(path string, networkIds []server.Id) error {
	var builder strings.Builder
	for _, networkId := range networkIds {
		builder.WriteString(networkId.String())
		builder.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(builder.String()), 0o600)
}

// Runs the dry run or the commit and returns the exit code.
func runNetworkCleanupDeleted(
	ctx context.Context,
	stdout io.Writer,
	stderr io.Writer,
	options *networkCleanupDeletedOptions,
) int {
	networkIds := options.networkIds
	if !options.commit && len(networkIds) == 0 {
		// discovery: one short read-only statement per page of each anchor
		cursor := model.DeletedNetworkDiscoveryCursor{}
		anchors := model.DeletedNetworkDiscoveryAnchors()
		anchorFound := 0
		for {
			page, next, done := model.FindDeletedNetworkIds(ctx, cursor, options.pageCount)
			networkIds = append(networkIds, page...)
			anchorFound += len(page)
			if next.AnchorIndex != cursor.AnchorIndex {
				fmt.Fprintf(stdout, "discovery %s: %d deleted network ids\n", anchors[cursor.AnchorIndex], anchorFound)
				anchorFound = 0
			}
			cursor = next
			if done {
				break
			}
		}
	} else if options.idsFile != "" && len(networkIds) == 0 {
		var err error
		networkIds, err = readNetworkIdsFile(options.idsFile)
		if err != nil {
			fmt.Fprintf(stderr, "network cleanup-deleted: %s\n", err)
			return 1
		}
	}
	networkIds = sortedUniqueNetworkIds(networkIds)
	digest := networkCleanupDeletedDigest(networkIds)

	if !options.commit {
		if options.idsFile != "" {
			if err := writeNetworkIdsFile(options.idsFile, networkIds); err != nil {
				fmt.Fprintf(stderr, "network cleanup-deleted: %s\n", err)
				return 1
			}
		}
		now := server.NowUtc()
		total := &model.DeletedNetworkData{RowCounts: map[string]int64{}, TimedOutCounts: map[string]int{}}
		for chunk := range slices.Chunk(networkIds, options.countChunk) {
			data := model.CountDeletedNetworkData(ctx, chunk, nil, now, options.cleanup)
			for table, count := range data.RowCounts {
				total.RowCounts[table] += count
			}
			for table, count := range data.TimedOutCounts {
				total.TimedOutCounts[table] += count
			}
			total.ActiveClientCount += data.ActiveClientCount
			total.DueClientCount += data.DueClientCount
			total.WaitingClientCount += data.WaitingClientCount
			total.DeviceCount += data.DeviceCount
		}
		fmt.Fprintf(stdout, "deleted networks: %d\n", len(networkIds))
		fmt.Fprintf(stdout, "clients: %d active (deactivated first), %d inactive and due, %d inactive and waiting for the client delay\n", total.ActiveClientCount, total.DueClientCount, total.WaitingClientCount)
		fmt.Fprintf(stdout, "devices: %d\n", total.DeviceCount)
		for _, table := range slices.Sorted(maps.Keys(total.RowCounts)) {
			fmt.Fprintf(stdout, "rows %s: %d\n", table, total.RowCounts[table])
		}
		for _, table := range slices.Sorted(maps.Keys(total.TimedOutCounts)) {
			fmt.Fprintf(stdout, "timed out %s: %d of %d network sets not counted (--count_statement_timeout=%s)\n", table, total.TimedOutCounts[table], (len(networkIds)+options.countChunk-1)/options.countChunk, options.cleanup.CountStatementTimeout)
		}
		fmt.Fprintf(stdout, "digest: %s\n", digest)
		fmt.Fprintf(stdout, "nothing was changed (dry run); commit with --ids_file and --commit --expect=%s\n", digest)
		return 0
	}

	if options.expectDigest != digest {
		fmt.Fprintf(stderr, "network cleanup-deleted: --expect %q does not match the digest %s of the %d networks listed; nothing was changed\n", options.expectDigest, digest, len(networkIds))
		return 1
	}
	pendingNetworkIds := []server.Id{}
	for _, networkId := range networkIds {
		if options.startAfter == nil || 0 < networkId.Cmp(*options.startAfter) {
			pendingNetworkIds = append(pendingNetworkIds, networkId)
		}
	}
	if 0 < options.maxNetworks && options.maxNetworks < len(pendingNetworkIds) {
		pendingNetworkIds = pendingNetworkIds[:options.maxNetworks]
	}
	// Chunks of --count_chunk networks, in id order. The networks of a chunk
	// with no client or device left lose their rows together, a pass per
	// chunk; the others are cleaned one at a time. A chunk ends with the id
	// to resume after.
	resumeAfter := "(none: run without --start_after)"
	if options.startAfter != nil {
		resumeAfter = options.startAfter.String()
	}
	// a network's runs until it is complete or its clients wait for the
	// client delay; then its cleanup task is queued for when they are due,
	// and the taskworker finishes them
	cleanupToWait := func(networkId server.Id) (status string, rowCounts map[string]int64, returnErr error) {
		deletedNetwork := &model.DeletedNetwork{NetworkId: networkId}
		rowCounts = map[string]int64{}
		for {
			var cleanup *model.DeletedNetworkCleanupResult
			if r := server.HandleError(func() {
				cleanup = model.RemoveDeletedNetworkData(ctx, deletedNetwork, server.NowUtc(), options.cleanup)
			}); r != nil {
				return "", rowCounts, fmt.Errorf("%v", r)
			}
			for table, count := range cleanup.RowCounts {
				rowCounts[table] += count
			}
			if cleanup.More && ctx.Err() == nil {
				continue
			}
			switch {
			case cleanup.NetworkExists:
				return "skipped: the network exists", rowCounts, nil
			case cleanup.Complete:
				return "complete", rowCounts, nil
			}
			runAt := server.NowUtc().Add(options.cleanup.ContinueInterval)
			if cleanup.NextClientDueTime != nil && runAt.Before(*cleanup.NextClientDueTime) {
				runAt = *cleanup.NextClientDueTime
			}
			if r := server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					controller.ScheduleRemoveDeletedNetworkDataInTx(ctx, tx, deletedNetwork, runAt)
				})
			}); r != nil {
				return "", rowCounts, fmt.Errorf("queue the cleanup task: %v", r)
			}
			return fmt.Sprintf("waiting: its clients are due at %s; queued for the taskworker", runAt.Format(time.RFC3339)), rowCounts, nil
		}
	}
	cleanedCount := 0
	for chunk := range slices.Chunk(pendingNetworkIds, options.countChunk) {
		if 0 < cleanedCount && 0 < options.pause {
			select {
			case <-ctx.Done():
				return 1
			case <-time.After(options.pause):
			}
		}
		var withClients map[server.Id]bool
		if r := server.HandleError(func() {
			withClients = model.DeletedNetworksWithClientsOrDevices(ctx, chunk)
		}); r != nil {
			fmt.Fprintf(stderr, "networks %s..%s failed: %v; resume with --start_after=%s\n", chunk[0], chunk[len(chunk)-1], r, resumeAfter)
			return 1
		}
		rowsOnly := slices.DeleteFunc(slices.Clone(chunk), func(networkId server.Id) bool {
			return withClients[networkId]
		})
		if 0 < len(rowsOnly) {
			var rowCounts map[string]int64
			if r := server.HandleError(func() {
				rowCounts = model.RemoveDeletedNetworksRows(ctx, rowsOnly, options.cleanup)
			}); r != nil {
				fmt.Fprintf(stderr, "networks %s..%s failed: %v; resume with --start_after=%s\n", chunk[0], chunk[len(chunk)-1], r, resumeAfter)
				return 1
			}
			fmt.Fprintf(stdout, "%d networks without clients or devices: %s\n", len(rowsOnly), formatNetworkCleanupCounts(rowCounts))
		}
		for _, networkId := range chunk {
			if !withClients[networkId] {
				continue
			}
			status, rowCounts, err := cleanupToWait(networkId)
			if err != nil {
				fmt.Fprintf(stderr, "network %s failed: %v; resume with --start_after=%s after checking it\n", networkId, err, resumeAfter)
				return 1
			}
			fmt.Fprintf(stdout, "network %s %s %s\n", networkId, status, formatNetworkCleanupCounts(rowCounts))
		}
		cleanedCount += len(chunk)
		resumeAfter = chunk[len(chunk)-1].String()
		fmt.Fprintf(stdout, "%d/%d networks; resume with --start_after=%s\n", cleanedCount, len(pendingNetworkIds), resumeAfter)
	}
	fmt.Fprintf(stdout, "done: %d networks\n", cleanedCount)
	return 0
}

func formatNetworkCleanupCounts(rowCounts map[string]int64) string {
	parts := []string{}
	for _, table := range slices.Sorted(maps.Keys(rowCounts)) {
		parts = append(parts, fmt.Sprintf("%s=%d", table, rowCounts[table]))
	}
	return strings.Join(parts, " ")
}
