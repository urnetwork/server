package main

// st_ledger_v2_extenders.go — the operator's commands for the SN25 extender
// pool (sn WHITEPAPER.md §8.6): the offline replay of a recorded epoch, an
// epoch's recorded shadow report, and the platform endpoint registry.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

func stV2ExtenderEpoch(opts docopt.Opts) uint64 {
	epochStr, _ := opts.String("--epoch")
	epoch, err := strconv.ParseUint(epochStr, 10, 64)
	if err != nil {
		panic(fmt.Errorf("bad --epoch %q: %s", epochStr, err))
	}
	return epoch
}

// stV2ExtenderOut writes data to --out, which must not exist, or to stdout.
func stV2ExtenderOut(opts docopt.Opts, data []byte) {
	out, _ := opts.String("--out")
	if out == "" {
		os.Stdout.Write(append(data, '\n'))
		return
	}
	file, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		panic(fmt.Errorf("--out %q: %w", out, err))
	}
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		panic(err)
	}
	fmt.Fprintf(os.Stderr, "written to %s\n", out)
}

// stV2ExtenderReplay is the offline replay of a recorded epoch under the
// extender pool amendment (§8.6.7, epoch 4): read only, it prints the report as
// JSON and exits 1 unless the rows differ by exactly the pooled shares.
func stV2ExtenderReplay(opts docopt.Opts) {
	// PostgreSQL refuses every write this process attempts
	server.RequirePgReadOnly()
	epoch := stV2ExtenderEpoch(opts)
	if out, _ := opts.String("--out"); out != "" {
		// fail before the read, not after it
		if _, err := os.Lstat(out); !errors.Is(err, os.ErrNotExist) {
			panic(fmt.Errorf("--out %q already exists or cannot be checked: %v", out, err))
		}
	}
	report, err := controller.StLedgerV2ExtenderReplay(context.Background(), epoch)
	if err != nil {
		panic(err)
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		panic(err)
	}
	stV2ExtenderOut(opts, encoded)
	fmt.Fprintf(os.Stderr, "epoch %d: recorded %d bytes in %d rows; amended %d bytes in %d rows; pooled %d bytes (Z %d: %d intra-platform, %d intra-network; unfunded %d; measurement %d); exact %v\n",
		epoch, report.RecordedBytes, report.RecordedRows, report.AmendedBytes, report.AmendedRows, report.PooledBytes,
		report.ExtenderPoolBytes, report.IntraPlatformBytes, report.IntraNetworkBytes, report.UnfundedBytes, report.MeasurementBytes, report.Exact)
	if !report.Exact {
		os.Exit(1)
	}
}

// stV2ExtenderShadow prints an epoch's recorded shadow report (§8.6.7).
func stV2ExtenderShadow(opts docopt.Opts) {
	server.RequirePgReadOnly()
	epoch := stV2ExtenderEpoch(opts)
	shadow, err := controller.GetStLedgerV2ExtenderShadow(context.Background(), epoch)
	if err != nil {
		panic(err)
	}
	if shadow == nil {
		panic(fmt.Errorf("epoch %d has no recorded shadow report", epoch))
	}
	fmt.Fprintf(os.Stderr, "epoch %d shadow report %s, recorded %s\n", epoch, shadow.ReportRef, shadow.CreateTime.UTC().Format("2006-01-02T15:04:05Z"))
	stV2ExtenderOut(opts, shadow.Report)
}

// stV2PlatformEndpoints lists the platform endpoint registry, as JSON.
func stV2PlatformEndpoints(opts docopt.Opts) {
	server.RequirePgReadOnly()
	set := model.GetStPlatformEndpoints(context.Background())
	registrations := []*model.StPlatformEndpoint{}
	for _, clientRegistrations := range set {
		registrations = append(registrations, clientRegistrations...)
	}
	encoded, err := json.MarshalIndent(registrations, "", "  ")
	if err != nil {
		panic(err)
	}
	os.Stdout.Write(append(encoded, '\n'))
}

// stV2PlatformEndpointRegister registers a client as a platform endpoint from
// now on (§8.6.2): tlsedge, smtpedge or mxedge, the kinds the signed amendment
// entry covers, on one of the operator's own networks. A new kind needs a new
// signed entry.
func stV2PlatformEndpointRegister(opts docopt.Opts) {
	clientIdStr, _ := opts.String("--client_id")
	clientId, err := server.ParseId(clientIdStr)
	if err != nil {
		panic(fmt.Errorf("bad --client_id %q: %s", clientIdStr, err))
	}
	kind, _ := opts.String("--kind")
	by, _ := opts.String("--by")
	registered, err := controller.StRegisterPlatformEndpoint(context.Background(), clientId, kind, by)
	if err != nil {
		panic(err)
	}
	fmt.Printf("client %s (network %s) is a %s platform endpoint from %s, registered by %s\n", registered.ClientId, registered.NetworkId, registered.Kind, registered.RegisterTime.UTC().Format("2006-01-02T15:04:05Z"), registered.RegisteredBy)
}

// stV2PlatformEndpointRevoke ends a client's platform endpoint registration now.
func stV2PlatformEndpointRevoke(opts docopt.Opts) {
	clientIdStr, _ := opts.String("--client_id")
	clientId, err := server.ParseId(clientIdStr)
	if err != nil {
		panic(fmt.Errorf("bad --client_id %q: %s", clientIdStr, err))
	}
	by, _ := opts.String("--by")
	if err := controller.StRevokePlatformEndpoint(context.Background(), clientId, by); err != nil {
		panic(err)
	}
	fmt.Printf("client %s's platform endpoint registration revoked by %s\n", clientId, by)
}
