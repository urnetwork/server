// server/cli/filecluster-suite (EVSB-M2-17; §22.9's file-cluster suite, D-OPS-20, FS-114, FS-63,
// plan-EVSB-settle-gate): the one suite a new cluster and each build and upgrade run, over the cluster as a block
// reaches it, reporting each case pass, fail or not run. It runs server.RunFileClusterSuite's cluster cases and
// sidecartest/blobsuite's store cases, the settle cases among them through a path that never cancels a late request,
// so a store that takes one fails.
//
//	filecluster-suite -clusters <file-clusters yml> -hosts <hosts yml> [-cluster <id>]... [-json]
//
// -clusters is a vault file's text (file-clusters-<service>.yml), whose credentials are the operator's to hand it:
// the suite reads no vault and prints no secret. -hosts says what the vault file does not
// (server.FileClusterSuiteHostsFile): each cluster's drives per host and drive timeout, and on T3 its hosts' barrier
// control ports and containers (local/file-cluster-t3). With no -cluster every cluster the file names runs.
//
// It exits 0 only when every case of every cluster ran and passed, and only then prints each cluster's
// settle_passed_build line, the build every host ran, which the operator records in config/<env>/sidecar.yml's
// file_clusters (plan-EVSB-settle-gate). A production cluster carries no barrier hooks, so its FS-114 cases do not
// run there and its run does not pass alone: the same commit's settle_testhooks build passing on T3 is that evidence,
// and the operator records the build once both runs agree. Exit 1: a case failed or did not run; 2: usage.
//
// It writes only under fresh prefixes and deletes what it wrote, so it may run on a cluster that serves, but it
// stops and starts hosts only where -hosts names containers, which only T3 does. Never in the whole-suite scripts:
// nightly and at Gn-out (§22.15).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/urnetwork/sidecar/sidecartest/blobsuite"

	"github.com/urnetwork/server"
)

type clusterIdsFlag []string

func (self *clusterIdsFlag) String() string         { return strings.Join(*self, ",") }
func (self *clusterIdsFlag) Set(value string) error { *self = append(*self, value); return nil }

var (
	clustersPath = flag.String("clusters", "", "a vault file's text naming the clusters (file-clusters-<service>.yml)")
	hostsPath    = flag.String("hosts", "", "the hosts file: each cluster's drives per host, drive timeout, hooks and containers")
	jsonReport   = flag.Bool("json", false, "print the reports as JSON")
	clusterIds   clusterIdsFlag
)

func main() {
	flag.Var(&clusterIds, "cluster", "a cluster to run, repeatable; none runs every cluster")
	flag.CommandLine.Init(os.Args[0], flag.ContinueOnError)
	if err := flag.CommandLine.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(2)
	}
	if *clustersPath == "" || *hostsPath == "" {
		fmt.Fprintln(os.Stderr, "filecluster-suite needs -clusters and -hosts")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	reports, err := run(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "filecluster-suite: %v\n", err)
		os.Exit(2)
	}
	if *jsonReport {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		encoder.Encode(reports)
	} else {
		for _, report := range reports {
			fmt.Print(report.String())
		}
	}
	for _, report := range reports {
		if !report.Passed() {
			os.Exit(1)
		}
	}
}

func run(ctx context.Context) ([]*server.FileClusterSuiteReport, error) {
	clustersText, err := os.ReadFile(*clustersPath)
	if err != nil {
		return nil, err
	}
	configs, err := server.ParseFileClusterConfigs(clustersText)
	if err != nil {
		return nil, err
	}
	hostsText, err := os.ReadFile(*hostsPath)
	if err != nil {
		return nil, err
	}
	hostsFile, err := server.ParseFileClusterSuiteHostsFile(hostsText)
	if err != nil {
		return nil, err
	}
	reports := []*server.FileClusterSuiteReport{}
	for _, config := range configs {
		if 0 < len(clusterIds) && !slices.Contains(clusterIds, config.ClusterId) {
			continue
		}
		target, err := server.NewFileClusterSuiteTarget(config, hostsFile.Clusters[config.ClusterId], nil)
		if err != nil {
			return nil, err
		}
		target.StoreCases = StoreCases
		report, err := server.RunFileClusterSuite(ctx, target)
		if err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}
	if len(reports) == 0 {
		return nil, errors.New("no cluster ran")
	}
	return reports, nil
}

// blobsuite's cases as the suite's: each by name under store/, a case that waits not run.
func StoreCases(ctx context.Context, target *server.FileClusterSuiteStoreTarget) []*server.FileClusterSuiteCase {
	results := blobsuite.RunCases(ctx, &blobsuite.Target{
		Cluster: target.Cluster,
		Now:     target.Now,
		// a real store rounds LastModified to the second
		ClockTolerance: 2 * time.Second,
		Settle:         &blobsuite.Settle{Now: target.Now, Delayed: target.Delayed},
	})
	cases := []*server.FileClusterSuiteCase{}
	for _, result := range results {
		c := &server.FileClusterSuiteCase{Name: "store/" + result.Name, Outcome: server.FileClusterSuitePass, Elapsed: result.Elapsed}
		switch {
		case result.Waits:
			c.Outcome = server.FileClusterSuiteNotRun
			c.Detail = "waits for D-OPS-20's build"
		case result.Err != nil:
			c.Outcome = server.FileClusterSuiteFail
			c.Detail = result.Err.Error()
		}
		cases = append(cases, c)
	}
	return cases
}
