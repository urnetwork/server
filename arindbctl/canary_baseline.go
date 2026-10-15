package main

// The canary-baseline command (arindbctl/ARINDB.md, "Failure modes"): which
// database the next all-release update copies forward as arin-old.mmdb, from
// the canary decision the score export published. Copying the environment's
// selected latest forward after a fallback would make the rejected database
// the next release's baseline, so the copy follows the database the decision
// serves, or the old database of a fallback it latched, whether the latch is
// still served or was dropped. It prints one line:
//
//	copy            copy the database the environment selects (--previous-config)
//	pin <path>      copy this database instead: the one the decision protects
//	pause <reason>  publish no new ARIN database: the decision cannot be followed
//
// A decision that is unreadable, of another version or older than --max-age
// pauses, as does a protected database the config tree no longer holds, or a
// dropped fallback the decision does not name, since an unchecked release
// would follow either way.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/urnetwork/server"
)

// The version of model.ArindbCanaryDecision this command reads.
const canaryBaselineDecisionVersion = 1

// The oldest decision the copy follows. A score export decides every few
// minutes; a decision this old belongs to a stalled export.
const canaryBaselineMaxAge = 6 * time.Hour

// A latch of the published decision: the pair it held out.
type canaryBaselineLatch struct {
	LatestEpoch int64 `json:"latest_epoch"`
	OldEpoch    int64 `json:"old_epoch"`
}

// The fields of the published decision the baseline follows.
type canaryBaselineDecision struct {
	Version      int    `json:"version"`
	State        string `json:"state"`
	ServingEpoch int64  `json:"serving_epoch"`
	ServingBasis string `json:"serving_basis"`
	Latest       struct {
		BuildEpoch int64 `json:"build_epoch"`
	} `json:"latest"`
	Old struct {
		BuildEpoch int64 `json:"build_epoch"`
	} `json:"old"`
	DecidedAt time.Time `json:"decided_at"`
	// a fallback latched on the pair, held, or dropped by a single decision or
	// a pair whose old database is the held-out latest
	Latch           *canaryBaselineLatch `json:"latch"`
	DroppedLatch    *canaryBaselineLatch `json:"dropped_latch"`
	FallbackDropped bool                 `json:"fallback_dropped"`
	// warming's paired sample trips, so the latest is blocked even when no
	// latch holds and each row's latest slot is served
	WarmingTripped bool `json:"warming_tripped"`
}

func runCanaryBaseline(args []string, output io.Writer, now time.Time) error {
	flags := flag.NewFlagSet("arindbctl canary-baseline", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	decisionPath := flags.String("decision", "", "the published {arindb_canary_decision} document")
	configRoot := flags.String("previous-config", "", "config root whose selected arindb/arin.mmdb is the baseline unless the decision serves another")
	env := flags.String("previous-env", "", "environment whose resource selection --previous-config follows")
	maxAge := flags.Duration("max-age", canaryBaselineMaxAge, "the oldest decision the copy follows")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *decisionPath == "" || *configRoot == "" || *env == "" || *maxAge <= 0 {
		return errors.New("canary-baseline requires decision, previous-config, previous-env and a positive max-age")
	}
	_, err := fmt.Fprintln(output, canaryBaseline(*decisionPath, *configRoot, *env, *maxAge, now))
	return err
}

// The line for the decision at decisionPath, as runCanaryBaseline prints it.
func canaryBaseline(decisionPath string, configRoot string, env string, maxAge time.Duration, now time.Time) string {
	data, err := os.ReadFile(decisionPath)
	if err != nil {
		return "pause the canary decision is unreadable: " + err.Error()
	}
	var decision canaryBaselineDecision
	if err := json.Unmarshal(data, &decision); err != nil {
		return "pause the canary decision is unreadable: " + err.Error()
	}
	if decision.Version != canaryBaselineDecisionVersion {
		return fmt.Sprintf("pause the canary decision is version %d, not %d", decision.Version, canaryBaselineDecisionVersion)
	}
	if decision.DecidedAt.IsZero() || maxAge < now.Sub(decision.DecidedAt) {
		return fmt.Sprintf("pause the canary decision of %s is older than %s", decision.DecidedAt.Format(time.RFC3339), maxAge)
	}
	// A latched fallback protects its old database whatever the decision
	// serves now: warming can serve each row's latest slot while the latch is
	// held, and a dropped latch's pair compares against the held-out latest.
	// A dropped latch names the database before the rejected one, so it comes
	// first; a tripped warming sample blocks the latest without a latch.
	protected := decision.ServingEpoch
	switch {
	case decision.DroppedLatch != nil:
		protected = decision.DroppedLatch.OldEpoch
	case decision.Latch != nil:
		protected = decision.Latch.OldEpoch
	case decision.WarmingTripped:
		protected = decision.Old.BuildEpoch
	case decision.FallbackDropped:
		return "pause the canary dropped a fallback without naming the database it protected"
	}
	// each row's latest slot is served: the selected latest is the baseline
	if protected == 0 {
		return "copy"
	}
	paths, err := server.ResolveResourcePaths(configRoot, env, "arindb/arin.mmdb")
	if errors.Is(err, server.ErrResourceNotFound) {
		return fmt.Sprintf("pause the canary protects the database built at %d, and %s selects no arindb/arin.mmdb", protected, env)
	}
	if err != nil {
		return "pause the selected database does not resolve: " + err.Error()
	}
	selectedEpoch, err := server.ReadArinDatabaseBuildEpoch(paths[0])
	if err != nil {
		return "pause the selected database does not open: " + err.Error()
	}
	if protected == selectedEpoch {
		return "copy"
	}
	path := findArinDatabaseBuiltAt(configRoot, protected)
	if path == "" {
		return fmt.Sprintf("pause the canary protects the database built at %d (%s, %s), which no release directory beneath the config holds",
			protected, decision.State, decision.ServingBasis)
	}
	return "pin " + path
}

// A release directory's arin.mmdb beneath configRoot built at epoch, newest
// version first, or "" without one. Every environment's directories are
// candidates: the database a fallback serves was an earlier release's latest.
func findArinDatabaseBuiltAt(configRoot string, epoch int64) string {
	paths, err := filepath.Glob(filepath.Join(configRoot, "*", "arindb", "*", "arin.mmdb"))
	if err != nil {
		return ""
	}
	slices.Sort(paths)
	slices.Reverse(paths)
	for _, path := range paths {
		if buildEpoch, err := server.ReadArinDatabaseBuildEpoch(path); err == nil && buildEpoch == epoch {
			return path
		}
	}
	return ""
}
