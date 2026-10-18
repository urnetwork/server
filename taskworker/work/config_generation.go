// Recurring tasks whose arguments or enablement come from config stamp each
// row with the config version that wrote it and when. Taskworkers run
// different config versions during a rollout, and a block started before its
// host received a new config keeps the older one until its next deploy, so a
// taskworker on an older config leaves a newer config's live rows alone rather
// than judge them by its own settings. Versions are ordered as warp selects
// the latest config: release, then build metadata. A process without a config
// version orders nothing and keeps the previous behavior.
package work

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/coreos/go-semver/semver"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// A task handed back to a newer config is due again this soon, so an instance
// of that config can claim it.
const configHandbackDelay = 5 * time.Second

// The task families that hand claims back to a newer config.
var configHandbackFamilies = []string{
	"provider_egress_probe",
	"provider_egress_probe_disabled",
	"contract_degradation",
	"verify_sweep_trails",
	"verify_rollup_provider_stats",
	"verify_remove_old_provider_stats",
}

// Claims handed back unchanged because a newer config owns the row. A steady
// rate shows taskworkers on an older config beside a newer one.
var configHandbacks = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_task_config_handbacks_total",
	Help: "Claimed task rows handed back unchanged to the newer config that owns them, by task family",
}, []string{"family"})

func init() {
	prometheus.MustRegister(configHandbacks)
	for _, family := range configHandbackFamilies {
		configHandbacks.WithLabelValues(family)
	}
}

// The config version that wrote a task's arguments, and when. Neither is part
// of the work the arguments describe.
type ConfigStamp struct {
	ConfigGeneration string    `json:"config_generation,omitempty"`
	ConfigWrittenAt  time.Time `json:"config_written_at,omitzero"`
}

// The stamp of arguments this process writes at now.
func newConfigStamp(now time.Time) ConfigStamp {
	return ConfigStamp{
		ConfigGeneration: currentConfigGeneration(),
		ConfigWrittenAt:  now.UTC(),
	}
}

// Whether a config newer than this process's wrote the stamp within timeout
// of now.
func (self ConfigStamp) newerWithin(now time.Time, timeout time.Duration) bool {
	return configGenerationNewer(self.ConfigGeneration, currentConfigGeneration()) &&
		!self.ConfigWrittenAt.IsZero() && now.Sub(self.ConfigWrittenAt) < timeout
}

// The config version of this process. It is read directly rather than through
// server.ConfigVersion, which falls back to the service version: only config
// versions order configs.
var currentConfigGeneration = func() string {
	return os.Getenv("WARP_CONFIG_VERSION")
}

// Whether candidate is a strictly newer config version than reference. An
// empty or unparsable version on either side has no order. Within a release
// only build metadata orders versions, as in warp. A docker tag form such as
// 2026.10.13-host-1071053750 carries the build code in the prerelease
// instead, so a release without build metadata on either side has no order
// rather than losing to any version that has it.
func configGenerationNewer(candidate string, reference string) bool {
	if candidate == "" || reference == "" {
		return false
	}
	candidateVersion, err := semver.NewVersion(candidate)
	if err != nil {
		return false
	}
	referenceVersion, err := semver.NewVersion(reference)
	if err != nil {
		return false
	}
	if candidateVersion.Major == referenceVersion.Major && candidateVersion.Minor == referenceVersion.Minor &&
		candidateVersion.Patch == referenceVersion.Patch {
		if candidateVersion.Metadata == "" || referenceVersion.Metadata == "" {
			return false
		}
		return referenceVersion.Metadata < candidateVersion.Metadata
	}
	return referenceVersion.LessThan(*candidateVersion)
}

// The stamps of the pending rows under runOnceKeys, read as a plain snapshot
// that takes no lock. A row whose arguments do not decode has no stamp.
func readConfigStampsInTx(ctx context.Context, tx server.PgTx, runOnceKeys []string) []ConfigStamp {
	stamps := []ConfigStamp{}
	result, err := tx.Query(ctx, `SELECT args_json FROM pending_task WHERE run_once_key = ANY($1)`, runOnceKeys)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var argsJson string
			server.Raise(result.Scan(&argsJson))
			var stamp ConfigStamp
			if json.Unmarshal([]byte(argsJson), &stamp) == nil {
				stamps = append(stamps, stamp)
			}
		}
	})
	return stamps
}

// From a handed-back task's own post-step, schedules the task again under its
// run-once key at runAt with its arguments copied as stored, so the newer
// build's fields that this build does not decode survive. The post-step's
// execution identity names the row its finishing transaction moved to
// finished_task; a RunPost retry carries the retry's identity instead, finds
// no such row, and falls back to schedule, which encodes the decoded
// arguments again.
func rescheduleHandedBackTaskInTx(
	clientSession *session.ClientSession,
	tx server.PgTx,
	runOnce *task.RunOnceOption,
	runAt time.Time,
	schedule func(),
) {
	identity, ok := task.ExecutionIdentityFromContext(clientSession.Ctx)
	if !ok || !task.RescheduleFinishedTaskInTx(clientSession.Ctx, tx, identity.TaskId, runOnce, runAt) {
		schedule()
	}
}
