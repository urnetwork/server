package perf

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type perfReport struct {
	Environment struct {
		Label        string `json:"label"`
		Commit       string `json:"commit"`
		Profile      string `json:"profile"`
		Machine      string `json:"machine"`
		RedisVersion string `json:"redis_version"`
		RedisMasters int    `json:"redis_masters"`
		Pools        string `json:"pools"`
		// subtracted from every measured interval
		IdleRedisCores float64   `json:"idle_redis_cores"`
		Started        time.Time `json:"started"`
		Seconds        float64   `json:"seconds"`
	} `json:"environment"`
	PhaseSeconds  map[string]float64 `json:"phase_seconds"`
	Ops           []*opSummary       `json:"ops"`
	Units         []*unitCost        `json:"units"`
	Memory        []*memoryFigure    `json:"memory"`
	MaxStateBytes int64              `json:"max_state_bytes"`
	ReceiptDays   float64            `json:"receipt_days"`
	Outcomes      []*outcome         `json:"outcomes"`
	Storms        []*stormReport     `json:"storms"`
	Steady        *steadyReport      `json:"steady,omitempty"`
	Outbox        []*outboxReport    `json:"outbox"`
	Slow          []*slowSummary     `json:"slow_scripts"`
	Gates         []*gateResult      `json:"gates"`
	Projection    []*projectedCost   `json:"projection"`
}

func newPerfReport(config *perfConfig) *perfReport {
	report := &perfReport{PhaseSeconds: map[string]float64{}}
	report.Environment.Label = config.label
	report.Environment.Profile = config.profile.name
	report.Environment.Machine = machineDescription()
	if output, err := exec.Command("git", "rev-parse", "--short=12", "HEAD").Output(); err == nil {
		report.Environment.Commit = strings.TrimSpace(string(output))
	}
	return report
}

type unitCost struct {
	Name               string       `json:"name"`
	Ops                int          `json:"ops"`
	WallSeconds        float64      `json:"wall_seconds"`
	Cost               *clusterCost `json:"cost"`
	CpuMicrosPerOp     float64      `json:"redis_cpu_us_per_op"`
	CpuMainMicrosPerOp float64      `json:"redis_main_thread_cpu_us_per_op"`
	CommandsPerOp      float64      `json:"commands_per_op"`
	EvalMicrosPerOp    float64      `json:"eval_us_per_op"`
	EvalsPerOp         float64      `json:"evals_per_op"`
	MemoryBytesPerOp   float64      `json:"memory_bytes_per_op"`
}

func (self *unitCost) derive() {
	ops := float64(max(1, self.Ops))
	self.CpuMicrosPerOp = self.Cost.CpuSeconds * 1e6 / ops
	self.CpuMainMicrosPerOp = self.Cost.CpuMainSeconds * 1e6 / ops
	self.CommandsPerOp = float64(self.Cost.TotalCommands) / ops
	if eval := self.Cost.Commands["eval"]; eval != nil {
		self.EvalMicrosPerOp = float64(eval.Usec) / ops
		self.EvalsPerOp = float64(eval.Calls) / ops
	}
	self.MemoryBytesPerOp = float64(self.Cost.MemoryDelta) / ops
}

type memoryFigure struct {
	Name    string           `json:"name"`
	Units   int              `json:"units"`
	Bytes   int64            `json:"bytes"`
	PerUnit float64          `json:"bytes_per_unit"`
	Detail  map[string]int64 `json:"detail,omitempty"`
}

func (self *perfReport) addMemory(name string, units int, bytes int64, detail map[string]int64) {
	figure := &memoryFigure{Name: name, Units: units, Bytes: bytes, Detail: detail}
	if 0 < units {
		figure.PerUnit = float64(bytes) / float64(units)
	}
	self.Memory = append(self.Memory, figure)
}

// A capacity or correctness outcome: what happened, and for a sample how many
// matched the named outcome and how many did not.
type outcome struct {
	Name     string `json:"name"`
	Outcome  string `json:"outcome"`
	Matching int    `json:"matching"`
	Other    int    `json:"other"`
}

func (self *perfReport) addOutcome(name string, label string, matching int, other int) {
	self.Outcomes = append(self.Outcomes, &outcome{Name: name, Outcome: label, Matching: matching, Other: other})
}

func (self *perfReport) outcome(prefix string) *outcome {
	for _, item := range self.Outcomes {
		if strings.HasPrefix(item.Name, prefix) {
			return item
		}
	}
	return nil
}

type stormReport struct {
	Name              string         `json:"name"`
	LiveSessions      int            `json:"live_sessions"`
	Synthetic         int            `json:"synthetic_sessions"`
	OpMs              float64        `json:"op_ms"`
	Revoked           int            `json:"revoked"`
	CleanupMs         float64        `json:"cleanup_ms"`
	CleanupBatches    int            `json:"cleanup_batches"`
	CleanupComplete   bool           `json:"cleanup_complete"`
	StormNode         string         `json:"storm_node"`
	RevokeCost        *clusterCost   `json:"revoke_cost"`
	CleanupCost       *clusterCost   `json:"cleanup_cost,omitempty"`
	Windows           []*stormWindow `json:"check_windows"`
	BackgroundLoads   []*loadResult  `json:"background_loads"`
	RecordBytesAfter  int64          `json:"record_bytes_after"`
	SessionBytesAfter int64          `json:"session_bytes_after"`
	ProbeRefused      int            `json:"probe_refused"`
	ProbeAccepted     int            `json:"probe_accepted"`
	RemintError       string         `json:"remint_error,omitempty"`
	Error             string         `json:"error,omitempty"`
}

type seriesPoint struct {
	Seconds      float64 `json:"seconds"`
	RedisCores   float64 `json:"redis_cores"`
	MaxNodeCores float64 `json:"max_node_cores"`
	UsedMemory   int64   `json:"used_memory"`
	Commands     float64 `json:"commands_per_second"`
	Connections  float64 `json:"connections_per_second"`
	Outbox       int64   `json:"outbox_rows"`
	HotLive      int64   `json:"hot_network_live"`
	LargeLive    int64   `json:"large_network_live"`
	// the test process, and the local containers as docker stats sees them
	HarnessCores float64 `json:"harness_cores"`
	PgCores      float64 `json:"postgres_container_cores"`
	RedisDocker  float64 `json:"redis_container_cores"`
}

type steadyReport struct {
	DurationSeconds float64        `json:"duration_seconds"`
	HotRate         float64        `json:"hot_proxy_rate"`
	CodeRate        float64        `json:"code_rate"`
	LargeSessions   int            `json:"large_network_sessions"`
	Loads           []*loadResult  `json:"loads"`
	Cost            *clusterCost   `json:"cost"`
	Series          []*seriesPoint `json:"series"`
	OutboxStart     int64          `json:"outbox_start"`
	OutboxEnd       int64          `json:"outbox_end"`
	HotLiveEnd      int64          `json:"hot_network_live_end"`
	HotHeldEnd      int64          `json:"hot_network_held_end"`
	PendingRemovals int            `json:"pending_removals"`
	// distinct networks with a registered mint in each 30 s interval
	MintingNetworksPerInterval []int `json:"minting_networks_per_30s"`
}

type outboxReport struct {
	Rows         int       `json:"rows"`
	RepairMs     []float64 `json:"repair_ms"`
	RepairError  string    `json:"repair_error,omitempty"`
	ObserveMs    float64   `json:"observe_ms"`
	ObserveError string    `json:"observe_error,omitempty"`
	RowsAfter    int       `json:"rows_after"`
}

type slowScript struct {
	Count   int     `json:"count"`
	TotalMs float64 `json:"total_ms"`
	MaxMs   float64 `json:"max_ms"`
	MaxKeys int     `json:"max_keys"`
	MaxNode string  `json:"max_node"`
}

type slowSummary struct {
	Name     string                 `json:"name"`
	ByScript map[string]*slowScript `json:"by_script"`
}

func (self *slowSummary) longest() (string, *slowScript) {
	var name string
	var longest *slowScript
	for key, script := range self.ByScript {
		if longest == nil || longest.MaxMs < script.MaxMs {
			name, longest = key, script
		}
	}
	return name, longest
}

type gateResult struct {
	Id        string `json:"id"`
	Name      string `json:"name"`
	Verdict   string `json:"verdict"`
	Measured  string `json:"measured"`
	Reference string `json:"reference"`
	Note      string `json:"note,omitempty"`
}

func (self *harness) op(name string) *opSummary {
	return self.ops.summary(name, time.Time{}, time.Time{}, nil)
}

func (self *harness) slow(name string) *slowSummary {
	for _, summary := range self.report.Slow {
		if summary.Name == name {
			return summary
		}
	}
	return nil
}

func (self *harness) finishReport() {
	for _, name := range self.ops.opNames() {
		self.report.Ops = append(self.report.Ops, self.op(name))
	}
	self.evaluateGates()
	self.project()
	dir := self.config.outDir
	if dir == "" {
		dir = self.t.TempDir()
	}
	self.must(os.MkdirAll(dir, 0o755), "create the output directory")
	encoded, err := json.MarshalIndent(self.report, "", "  ")
	self.must(err, "encode the report")
	self.must(os.WriteFile(filepath.Join(dir, "report.json"), encoded, 0o644), "write report.json")
	markdown := self.markdown()
	self.must(os.WriteFile(filepath.Join(dir, "report.md"), []byte(markdown), 0o644), "write report.md")
	self.t.Logf("report written to %s\n%s", dir, markdown)
}

func ms(value float64) string {
	switch {
	case value == 0:
		return "0"
	case value < 1:
		return fmt.Sprintf("%.3f", value)
	case value < 100:
		return fmt.Sprintf("%.2f", value)
	}
	return fmt.Sprintf("%.0f", value)
}

func (self *harness) gate(id, name, verdict, measured, reference, note string) {
	self.report.Gates = append(self.report.Gates, &gateResult{Id: id, Name: name, Verdict: verdict, Measured: measured, Reference: reference, Note: note})
}

// Budget gates: the call completed without failure inside the storage budget
// it runs under. The reference columns compare with the 2026-10-09 local
// profile in REVOKE-OPERATIONS.md, which measured the same paths alone.
func (self *harness) budgetGate(id, name, op string, reference string) {
	summary := self.op(op)
	if summary.Count == 0 && summary.errorCount() == 0 {
		self.gate(id, name, "SKIP", "not run", reference, "")
		return
	}
	verdict := "PASS"
	note := ""
	if 0 < summary.errorCount() {
		verdict = "FAIL"
		note = fmt.Sprintf("errors %v", summary.Errors)
	} else if storageBudgetMs < summary.MaxMs {
		verdict = "FAIL"
		note = "over the 2 s storage budget"
	}
	self.gate(id, name, verdict, fmt.Sprintf("p50 %s ms, max %s ms (%d calls)", ms(summary.P50Ms), ms(summary.MaxMs), summary.Count), reference, note)
}

func (self *harness) evaluateGates() {
	// G1 the maximum-state memory profile
	if self.report.MaxStateBytes == 0 {
		self.gate("G1", "Max-state memory (1,000 live + 9,000 markers)", "SKIP", "not run", "2,717,332 B", "")
	} else {
		ratio := float64(self.report.MaxStateBytes) / referenceMaxStateBytes
		verdict := "PASS"
		if 1.25 < ratio {
			verdict = "WARN"
		}
		if 2 < ratio {
			verdict = "FAIL"
		}
		self.gate("G1", "Max-state memory (1,000 live + 9,000 markers)", verdict, fmt.Sprintf("%d B (%.2fx)", self.report.MaxStateBytes, ratio), "2,717,332 B (pass at most 1.25x)", "")
	}
	self.budgetGate("G2", "List at the live cap (1,000)", "capacity.list@1000", fmt.Sprintf("%.1f ms; budget 2 s", referenceListMs))
	self.annotateScript("G2", "list@1000", "eval:list")
	self.budgetGate("G3", "List at the maximum state", "capacity.list@max", fmt.Sprintf("%.1f ms; budget 2 s", referenceListMs))
	self.annotateScript("G3", "list@max", "eval:list")
	self.budgetGate("G4", "Bulk revoke at the live cap (1,000)", "capacity.revoke.others@1000", fmt.Sprintf("%.1f ms (Lua only); budget 2 s", referenceBulkRevokeMs))
	self.annotateScript("G4", "revoke.others@1000.cycle", "eval:revoke")
	self.budgetGate("G5", "Bulk revoke at the maximum state", "capacity.revoke.others@max", fmt.Sprintf("%.1f ms (Lua only); budget 2 s", referenceBulkRevokeMs))
	self.annotateScript("G5", "revoke.others@max", "eval:revoke")
	large := fmt.Sprintf("%d", self.config.profile.largeSessions)
	self.budgetGate("G6", "List at the new cap ("+large+")", "capacity.list@"+large, "budget 2 s")
	self.annotateScript("G6", "list@"+large, "eval:list")

	// G7 the bulk Lua maximum at the new cap, and the node time it holds
	stormOp := ""
	var storm *stormReport
	for _, item := range self.report.Storms {
		if strings.HasPrefix(item.Name, "sign out other") {
			storm = item
			stormOp = fmt.Sprintf("storm.revoke.others@%d", item.LiveSessions)
		}
	}
	if storm == nil {
		self.gate("G7", "Bulk revoke at the new cap ("+large+")", "SKIP", "not run", "budget 2 s", "")
	} else {
		self.budgetGate("G7", fmt.Sprintf("Bulk revoke at the new cap (%d live)", storm.LiveSessions), stormOp, "budget 2 s")
		if slow := self.slow(stormOp); slow != nil {
			name, longest := slow.longest()
			if longest != nil {
				verdict := "PASS"
				if 100 < longest.MaxMs {
					verdict = "WARN"
				}
				self.gate("G7b", "Longest single script of that revoke (node held)", verdict, fmt.Sprintf("%s %s ms on %s (%d keys)", name, ms(longest.MaxMs), longest.MaxNode, longest.MaxKeys), "warn above 100 ms: every command on the node waits", "")
			}
		}
		for _, window := range storm.Windows {
			if window.Name != "revoke" {
				continue
			}
			before := storm.Windows[0]
			verdict := "PASS"
			if 100 < window.Storm.MaxMs || (0 < before.Storm.P99Ms && 10*before.Storm.P99Ms < window.Storm.P99Ms) {
				verdict = "WARN"
			}
			self.gate("G7c", "Checks on the revoked network's node during that revoke", verdict,
				fmt.Sprintf("p99 %s ms, max %s ms (%d checks)", ms(window.Storm.P99Ms), ms(window.Storm.MaxMs), window.Storm.Count),
				fmt.Sprintf("before: p99 %s ms, max %s ms", ms(before.Storm.P99Ms), ms(before.Storm.MaxMs)),
				"warn above 100 ms max or 10x the p99 before")
		}
	}
	for _, item := range self.report.Storms {
		if strings.HasPrefix(item.Name, "sign-out-all") {
			verdict := "PASS"
			note := fmt.Sprintf("%d revoked, cleanup complete %t", item.Revoked, item.CleanupComplete)
			if item.Error != "" {
				verdict = "FAIL"
				note += "; enforcement error: " + item.Error
			}
			self.gate("G8", fmt.Sprintf("Sign-out-all at %d live", item.LiveSessions), verdict, fmt.Sprintf("%s ms", ms(item.OpMs)), "completes; Redis calls within 2 s each", note)
		}
	}

	// G9, G10 checks under the steady production shape
	checkGate := func(id, name, op string) {
		summary := self.op(op)
		if summary.Count == 0 {
			self.gate(id, name, "SKIP", "not run", "", "")
			return
		}
		verdict := "PASS"
		note := ""
		if 0 < summary.failureCount() {
			verdict = "FAIL"
			note = fmt.Sprintf("errors %v", summary.Errors)
		} else if storageBudgetMs < summary.MaxMs {
			verdict = "FAIL"
			note = "over the 2 s budget"
		} else if 10*referenceCheckP99Ms < summary.P99Ms {
			verdict = "WARN"
			note = "p99 above 10x the sequential reference"
		}
		self.gate(id, name, verdict, fmt.Sprintf("p50 %s, p99 %s, max %s ms at %.0f/s", ms(summary.P50Ms), ms(summary.P99Ms), ms(summary.MaxMs), summary.Rate), fmt.Sprintf("sequential p50 %.3f, p99 %.3f ms; budget 2 s", referenceCheckP50Ms, referenceCheckP99Ms), note)
	}
	checkGate("G9", "Periodic revalidation (lease checks) under steady load", "steady.lease.check")
	checkGate("G10", "API request authentication under steady load", "steady.api.auth")
	if rtt := self.op("steady.redis.rtt"); 0 < rtt.Count {
		for _, gate := range self.report.Gates {
			if gate.Id == "G9" || gate.Id == "G10" {
				gate.Note = strings.TrimSpace(gate.Note + fmt.Sprintf(" (a bare Redis round trip under the same load: p50 %s, p99 %s ms)", ms(rtt.P50Ms), ms(rtt.P99Ms)))
			}
		}
	}

	// G11 mints and removals under steady load fail only by design
	mintOps := []string{"steady.authclient.apikey", "steady.authclient.jwt", "steady.mint.child", "steady.refresh", "steady.mint.signin", "steady.hotproxy.create", "steady.hotproxy.device_open", "steady.code.login", "steady.remove.client", "steady.remove.child", "steady.hotproxy.remove", "steady.revoke.single", "steady.list.large", "steady.hint.large", "steady.hint.small"}
	for _, name := range self.ops.opNames() {
		if strings.HasPrefix(name, "load.") {
			mintOps = append(mintOps, name)
		}
	}
	failures := []string{}
	refusals := []string{}
	ran := false
	for _, name := range mintOps {
		summary := self.op(name)
		if summary.Count+summary.errorCount() == 0 {
			continue
		}
		ran = true
		for class, count := range summary.Errors {
			if strings.Contains(class, "refused:") {
				refusals = append(refusals, fmt.Sprintf("%s %s x%d", name, class, count))
			}
			if !strings.HasPrefix(class, "refused:") {
				failures = append(failures, fmt.Sprintf("%s %s x%d", name, class, count))
			}
		}
	}
	sort.Strings(failures)
	sort.Strings(refusals)
	if !ran {
		self.gate("G11", "Mints, removals, lists and hints under steady load", "SKIP", "not run", "", "")
		self.gate("G12", "No capacity refusal at production shapes", "SKIP", "not run", "", "")
	} else {
		verdict := "PASS"
		if 0 < len(failures) {
			verdict = "FAIL"
		}
		self.gate("G11", "Mints, removals, lists and hints under steady load", verdict, fmt.Sprintf("%d failure classes", len(failures)), "no failure but designed refusals", strings.Join(failures, "; "))
		verdict = "PASS"
		if 0 < len(refusals) {
			verdict = "FAIL"
		}
		self.gate("G12", "No capacity refusal at production shapes", verdict, fmt.Sprintf("%d refusal classes", len(refusals)), "decision 6: the cap is not enforced", strings.Join(refusals, "; "))
	}

	// G13 no eviction
	if steady := self.report.Steady; steady != nil {
		verdict := "PASS"
		if 0 < steady.Cost.Evicted {
			verdict = "FAIL"
		}
		self.gate("G13", "No key eviction", verdict, fmt.Sprintf("%d evicted", steady.Cost.Evicted), "0 (volatile-ttl would evict authority keys)", "")

		// G14 the index outbox drains as fast as minting networks fill it:
		// every registered mint upserts its network's row, and only shard
		// zero's repair drains them, 64 a 30 s run
		growth := steady.OutboxEnd - steady.OutboxStart
		perMinute := float64(growth) / (steady.DurationSeconds / 60)
		busiest := 0
		for _, count := range steady.MintingNetworksPerInterval {
			busiest = max(busiest, count)
		}
		verdict = "PASS"
		note := "the outbox holds one row a network, so this run's backlog is bounded by its own network count"
		if 64 < growth || 64 < busiest {
			verdict = "FAIL"
			note = "more distinct networks mint per 30 s than one repair drains; with Main's network diversity the backlog grows toward every network that mints"
		}
		self.gate("G14", "Index outbox keeps up with minting networks", verdict, fmt.Sprintf("%d distinct minting networks per 30 s at most; outbox %d -> %d rows (%+.0f/min)", busiest, steady.OutboxStart, steady.OutboxEnd, perMinute), "drain: one 64-row repair per 30 s, shard 0 only", note)
	}

	// G15 the repair and its gauges at a backlog
	for _, item := range self.report.Outbox {
		longest := 0.0
		for _, value := range item.RepairMs {
			longest = max(longest, value)
		}
		verdict := "PASS"
		note := ""
		if 1000 < longest || item.RepairError != "" {
			verdict = "FAIL"
			note = "repair " + item.RepairError
		}
		if 100 < item.ObserveMs || item.ObserveError != "" {
			if verdict == "PASS" {
				verdict = "WARN"
			}
			note = strings.TrimSpace(note + " backlog gauges " + item.ObserveError + " (over their 100 ms budget: the alert metric goes stale)")
		}
		self.gate("G15", fmt.Sprintf("Index repair at a %d-row backlog", item.Rows), verdict, fmt.Sprintf("repair max %s ms, gauges %s ms, %d rows after 3 repairs", ms(longest), ms(item.ObserveMs), item.RowsAfter), "repair 1 s budget, gauges 100 ms", note)
	}

	// G16 revocation takes effect
	effective := []string{}
	verdict := "SKIP"
	for _, prefix := range []string{"removed clients' credentials", "revoked hosted clients' credentials", "idle-reaped clients' credentials"} {
		if item := self.report.outcome(prefix); item != nil {
			if verdict == "SKIP" {
				verdict = "PASS"
			}
			effective = append(effective, fmt.Sprintf("%s: %d refused, %d accepted", item.Name, item.Matching, item.Other))
			if 0 < item.Other {
				verdict = "FAIL"
			}
		}
	}
	self.gate("G16", "Revocation and removal take effect at the next check", verdict, strings.Join(effective, "; "), "every revoked or removed credential refused", "")

	// G17 decision 4
	if item := self.report.outcome("hosted proxy credentials after sign-out-all"); item != nil {
		remint := self.report.outcome("hosted proxy re-mints after sign-out-all")
		verdict := "PASS"
		if 0 < item.Other || (remint != nil && 0 < remint.Other) {
			verdict = "FAIL"
		}
		measured := fmt.Sprintf("checks %d accepted / %d refused", item.Matching, item.Other)
		if remint != nil {
			measured += fmt.Sprintf("; re-mints %d accepted / %d refused", remint.Matching, remint.Other)
		}
		self.gate("G17", "Hosted proxies survive sign-out-all (decision 4, S2 N9)", verdict, measured, "all accepted", "")
	} else {
		self.gate("G17", "Hosted proxies survive sign-out-all (decision 4, S2 N9)", "SKIP", "not run", "all accepted", "")
	}
	self.gate("G18", "Stream lease fan-out (1,000-stream grant renewal)", "SKIP", "not exercised", "p50 1.292, p99 3.941 ms", "connect's own tests own it; its credential checks are the lease checks of G9")

	// G19, G20 R-free: a removal frees the client sessions it owns
	freedGate := func(id, name, prefix string) {
		item := self.report.outcome(prefix)
		if item == nil {
			self.gate(id, name, "SKIP", "not run", "every owned session freed", "")
			return
		}
		verdict := "PASS"
		if 0 < item.Other || item.Matching == 0 {
			verdict = "FAIL"
		}
		self.gate(id, name, verdict, fmt.Sprintf("%d freed, %d still live", item.Matching, item.Other), "every owned session freed (FIXPLAN R-free)", "")
	}
	freedGate("G19", "Removing API key clients frees their sessions", "sessions freed by removing API key clients")
	freedGate("G20", "The 30-day idle reap frees its clients' sessions", "sessions freed by the idle reap")
}

func (self *harness) markdown() string {
	var out strings.Builder
	report := self.report
	env := report.Environment
	fmt.Fprintf(&out, "# Session perf report: %s\n\n", env.Label)
	fmt.Fprintf(&out, "- commit %s, profile %s, %s\n", env.Commit, env.Profile, env.Machine)
	fmt.Fprintf(&out, "- Redis %s, a disposable cluster of %d masters; pools: %s; started %s, ran %.0f s\n", env.RedisVersion, env.RedisMasters, env.Pools, env.Started.Format(time.RFC3339), env.Seconds)
	fmt.Fprintf(&out, "- idle cluster CPU %.4f cores, subtracted from every measured interval\n", env.IdleRedisCores)
	phases := []string{}
	for name, seconds := range report.PhaseSeconds {
		phases = append(phases, fmt.Sprintf("%s %.0f s", name, seconds))
	}
	sort.Strings(phases)
	fmt.Fprintf(&out, "- phases: %s\n\n", strings.Join(phases, ", "))

	fmt.Fprintf(&out, "## Gates\n\n| Gate | Verdict | Measured | Reference / budget | Note |\n|---|---|---|---|---|\n")
	for _, gate := range report.Gates {
		fmt.Fprintf(&out, "| %s %s | %s | %s | %s | %s |\n", gate.Id, gate.Name, gate.Verdict, gate.Measured, gate.Reference, gate.Note)
	}

	fmt.Fprintf(&out, "\n## Latency by operation (ms)\n\n| Operation | OK | Errors | p50 | p90 | p99 | max | rate/s |\n|---|---|---|---|---|---|---|---|\n")
	for _, summary := range report.Ops {
		errorsText := ""
		if 0 < len(summary.Errors) {
			parts := []string{}
			for class, count := range summary.Errors {
				parts = append(parts, fmt.Sprintf("%s %d", class, count))
			}
			sort.Strings(parts)
			errorsText = strings.Join(parts, ", ")
		}
		fmt.Fprintf(&out, "| %s | %d | %s | %s | %s | %s | %s | %.1f |\n", summary.Op, summary.Count, errorsText, ms(summary.P50Ms), ms(summary.P90Ms), ms(summary.P99Ms), ms(summary.MaxMs), summary.Rate)
	}

	fmt.Fprintf(&out, "\n## Redis cost per operation, each run alone\n\nRedis CPU is the cluster's process CPU (user + system, all threads) between snapshots; eval is the server-side time of EVAL calls. Memory is the change in used_memory.\n\n| Operation | Ops | Wall s | Redis CPU µs/op | main thread µs/op | commands/op | evals/op | eval µs/op | memory B/op |\n|---|---|---|---|---|---|---|---|---|\n")
	for _, unit := range report.Units {
		fmt.Fprintf(&out, "| %s | %d | %.2f | %.1f | %.1f | %.2f | %.2f | %.1f | %.0f |\n", unit.Name, unit.Ops, unit.WallSeconds, unit.CpuMicrosPerOp, unit.CpuMainMicrosPerOp, unit.CommandsPerOp, unit.EvalsPerOp, unit.EvalMicrosPerOp, unit.MemoryBytesPerOp)
	}

	if 0 < len(report.Projection) {
		total := 0.0
		fmt.Fprintf(&out, "\n## Projected Redis CPU at Main's rates\n\nThe isolated Redis CPU per call times Main's measured rate (2026-11-18). This machine's cores are not Main's, so read it as an order of magnitude. Main has 48 masters.\n\n| Operation | Measured as | Main rate/s | Redis CPU µs/op | Cores | Source of the rate |\n|---|---|---|---|---|---|\n")
		for _, item := range report.Projection {
			total += item.Cores
			fmt.Fprintf(&out, "| %s | %s | %.1f | %.1f | %.3f | %s |\n", item.Name, item.Unit, item.Rate, item.MicrosPerOp, item.Cores, item.Source)
		}
		fmt.Fprintf(&out, "| total | | | | %.3f | %.4f cores a master |\n", total, total/48)
	}

	fmt.Fprintf(&out, "\n## Memory (MEMORY USAGE of the keys, every element sampled)\n\n| Item | Units | Bytes | Bytes/unit | Per 1,000 units |\n|---|---|---|---|---|\n")
	for _, figure := range report.Memory {
		perThousand := "-"
		if 1 < figure.Units {
			perThousand = fmt.Sprintf("%.2f MB", figure.PerUnit*1000/1e6)
		}
		fmt.Fprintf(&out, "| %s | %d | %d | %.0f | %s |\n", figure.Name, figure.Units, figure.Bytes, figure.PerUnit, perThousand)
	}

	fmt.Fprintf(&out, "\n## Capacity and correctness outcomes\n\n| Event | Outcome | Matching | Other |\n|---|---|---|---|\n")
	for _, item := range report.Outcomes {
		fmt.Fprintf(&out, "| %s | %s | %d | %d |\n", item.Name, item.Outcome, item.Matching, item.Other)
	}

	for _, storm := range report.Storms {
		fmt.Fprintf(&out, "\n## Storm: %s at %d live sessions\n\n", storm.Name, storm.LiveSessions)
		if 0 < storm.Synthetic {
			fmt.Fprintf(&out, "- %d of the sessions were written with the mint script's key layout after the mint path refused at its live cap.\n", storm.Synthetic)
		}
		fmt.Fprintf(&out, "- the call: %s ms, %d revoked; Redis %.3f CPU s, %d commands, used memory %+d B\n", ms(storm.OpMs), storm.Revoked, storm.RevokeCost.CpuSeconds, storm.RevokeCost.TotalCommands, storm.RevokeCost.MemoryDelta)
		if storm.CleanupCost != nil {
			fmt.Fprintf(&out, "- client cleanup: %s ms in %d batches; Redis %.3f CPU s, %d commands\n", ms(storm.CleanupMs), storm.CleanupBatches, storm.CleanupCost.CpuSeconds, storm.CleanupCost.TotalCommands)
		}
		fmt.Fprintf(&out, "- after: session keys %d B, record shards %d B; probe of the targets' credentials: %d refused, %d accepted\n", storm.SessionBytesAfter, storm.RecordBytesAfter, storm.ProbeRefused, storm.ProbeAccepted)
		if storm.RemintError != "" {
			fmt.Fprintf(&out, "- hosted re-mint refusal: %s\n", storm.RemintError)
		}
		fmt.Fprintf(&out, "- background checks: %s\n\n| Window | storm node p50 / p99 / max (n) | other nodes p50 / p99 / max (n) |\n|---|---|---|\n", describeLoads(storm.BackgroundLoads))
		for _, window := range storm.Windows {
			fmt.Fprintf(&out, "| %s (%.1f s) | %s / %s / %s (%d) | %s / %s / %s (%d) |\n", window.Name, window.To.Sub(window.From).Seconds(),
				ms(window.Storm.P50Ms), ms(window.Storm.P99Ms), ms(window.Storm.MaxMs), window.Storm.Count,
				ms(window.Other.P50Ms), ms(window.Other.P99Ms), ms(window.Other.MaxMs), window.Other.Count)
		}
	}

	if steady := report.Steady; steady != nil {
		fmt.Fprintf(&out, "\n## Steady production shape (%.0f s)\n\n", steady.DurationSeconds)
		fmt.Fprintf(&out, "- hot proxy network: %.2f creates/s (7,300/day compressed), live sessions at the end %d, held %d\n", steady.HotRate, steady.HotLiveEnd, steady.HotHeldEnd)
		fmt.Fprintf(&out, "- code fleet: %.2f redemptions/s; large network %d live sessions\n", steady.CodeRate, steady.LargeSessions)
		fmt.Fprintf(&out, "- Redis: %.2f cores on average across the cluster (%.2f on the busiest node), %.0f commands/s, %d evicted\n", steady.Cost.CpuSeconds/steady.Cost.Wall, steady.Cost.MaxNodeCpu/steady.Cost.Wall, float64(steady.Cost.TotalCommands)/steady.Cost.Wall, steady.Cost.Evicted)
		fmt.Fprintf(&out, "- top commands: %s\n", strings.Join(steady.Cost.topCommands(12), ", "))
		fmt.Fprintf(&out, "- index outbox: %d rows at the start, %d at the end; distinct minting networks per 30 s: %v (one repair drains 64)\n\n| Load | Target/s | Achieved/s | Dropped |\n|---|---|---|---|\n", steady.OutboxStart, steady.OutboxEnd, steady.MintingNetworksPerInterval)
		for _, load := range steady.Loads {
			fmt.Fprintf(&out, "| %s | %.2f | %.2f | %d |\n", load.Name, load.Target, load.Achieved, load.Dropped)
		}
		fmt.Fprintf(&out, "\nRedis cores come from INFO (the nodes' own accounting); the container columns are docker stats, which also count the container's network path.\n\n| t (s) | Redis cores | busiest node | commands/s | new connections/s | used MB | outbox rows | hot network live | large network live | harness cores | PostgreSQL container | Redis container |\n|---|---|---|---|---|---|---|---|---|---|---|---|\n")
		for _, point := range steady.Series {
			fmt.Fprintf(&out, "| %.0f | %.2f | %.2f | %.0f | %.1f | %.1f | %d | %d | %d | %.2f | %.2f | %.2f |\n", point.Seconds, point.RedisCores, point.MaxNodeCores, point.Commands, point.Connections, float64(point.UsedMemory)/1e6, point.Outbox, point.HotLive, point.LargeLive, point.HarnessCores, point.PgCores, point.RedisDocker)
		}
	}

	if 0 < len(report.Outbox) {
		fmt.Fprintf(&out, "\n## Index outbox backlog\n\n| Rows | Repair ms (3 runs, 64 rows each) | Gauges ms | Error | Rows after |\n|---|---|---|---|---|\n")
		for _, item := range report.Outbox {
			parts := []string{}
			for _, value := range item.RepairMs {
				parts = append(parts, ms(value))
			}
			fmt.Fprintf(&out, "| %d | %s | %s | %s %s | %d |\n", item.Rows, strings.Join(parts, ", "), ms(item.ObserveMs), item.RepairError, item.ObserveError, item.RowsAfter)
		}
	}

	if 0 < len(report.Slow) {
		fmt.Fprintf(&out, "\n## Slow commands (slowlog, at least 1 ms)\n\n| During | Command | Count | Total ms | Longest ms | Keys | Node |\n|---|---|---|---|---|---|---|\n")
		for _, summary := range report.Slow {
			names := []string{}
			for name := range summary.ByScript {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				script := summary.ByScript[name]
				fmt.Fprintf(&out, "| %s | %s | %d | %s | %s | %d | %s |\n", summary.Name, name, script.Count, ms(script.TotalMs), ms(script.MaxMs), script.MaxKeys, script.MaxNode)
			}
		}
	}
	return out.String()
}

// Main's session traffic priced at the isolated per-call Redis CPU.
type projectedCost struct {
	Name        string  `json:"name"`
	Unit        string  `json:"unit"`
	Rate        float64 `json:"rate_per_second"`
	MicrosPerOp float64 `json:"redis_cpu_us_per_op"`
	Cores       float64 `json:"cores"`
	Source      string  `json:"source"`
}

func (self *harness) project() {
	units := map[string]*unitCost{}
	for _, unit := range self.report.Units {
		units[unit.Name] = unit
	}
	for _, item := range []struct {
		name, unit string
		rate       float64
		source     string
	}{
		{"record checks (api, connect, proxy, alt)", "check.hit", 14700, "S3: jwt_state_record_checks ~14.7k/s"},
		{"API authentications beyond the record check", "api.auth.client", 0, "counted in the record checks"},
		{"session hint reads (connect)", "hint.small", 5300, "S3: connect inventory EVALs ~5.3k/s"},
		{"auth-client with a network credential", "authclient.jwt", 158, "S4: 172.7/s auth-client less API-key mints"},
		{"auth-client with an API key (own session)", "authclient.apikey", 14.8, "S4: api_key auth attempts 14.8/s, an upper bound for its mints"},
		{"hosted derived child mints", "mint.child", 223, "S4: 31 reserved + 192 shared admissions/s"},
		{"hosted device opens", "mint.hosted.reuse", 15.3, "S4: 1.1 cold + 14.2 reopen builds/s"},
		{"refresh", "refresh", 7.4, "S4: /auth/refresh 7.3/s + network-refresh 0.13/s"},
		{"password sign-ins", "mint.signin", 1.6, "S4: lb log, ~141k/day"},
		{"remove-client", "remove.client.jwt", 162, "S4: remove-client 162/s"},
		{"code redemptions", "code.login", 0.0097, "S4: ~840/day real redemptions"},
	} {
		unit := units[item.unit]
		if unit == nil || item.rate == 0 {
			continue
		}
		self.report.Projection = append(self.report.Projection, &projectedCost{
			Name:        item.name,
			Unit:        item.unit,
			Rate:        item.rate,
			MicrosPerOp: unit.CpuMicrosPerOp,
			Cores:       unit.CpuMicrosPerOp * item.rate / 1e6,
			Source:      item.source,
		})
	}
}

// Adds the longest node time one script of the measured calls held, from the
// slowlog, to a gate's measured column.
func (self *harness) annotateScript(id string, slowPrefix string, script string) {
	longest := 0.0
	for _, summary := range self.report.Slow {
		if !strings.HasPrefix(summary.Name, slowPrefix) {
			continue
		}
		if item := summary.ByScript[script]; item != nil {
			longest = max(longest, item.MaxMs)
		}
	}
	if longest == 0 {
		return
	}
	for _, gate := range self.report.Gates {
		if gate.Id == id {
			gate.Measured += fmt.Sprintf("; the %s script itself %s ms at most (slowlog)", strings.TrimPrefix(script, "eval:"), ms(longest))
		}
	}
}
