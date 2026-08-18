#!/usr/bin/env bash
# Verify an already-finished eval-48d local baseline and its held-out A/A check.
# This script compiles and runs tests, so never invoke it during a measurement.

set -euo pipefail
cd "$(dirname "$0")"

expected_binary_sha="62665d25290c9ee9e81434a542e1ccd959709eb35ccdc82fef511e33204c5b29"
expected_config_sha="549ec41c033f344d6e0a6b1de82b404bb63d5a8dfb5861b6c4b6d55886cdace4"

log() { printf '[baseline-verify] %s %s\n' "$(date -u '+%F %T UTC')" "$*"; }

if pgrep -x sim-latency >/dev/null 2>&1; then
    log "sim-latency is still running; refusing to contaminate a measurement"
    exit 1
fi

for path in \
    eval-48g/baseline.json \
    eval-48g/baseline-summary.json \
    eval-48g/baseline-summary.md \
    eval-48g/baseline-stability.svg \
    eval-48g/heldout-aa-compare.json; do
    [ -s "$path" ] || { log "missing required artifact: $path"; exit 1; }
done

snapshot_hashes() {
    sha256sum \
        eval-48g/baseline.json \
        eval-48g/baseline-summary.json \
        eval-48g/baseline-summary.md \
        eval-48g/baseline-stability.svg
}

hashes_before="$(snapshot_hashes)"
./eval-48.sh baseline >/dev/null
./eval-48.sh summary >/dev/null
hashes_after_first="$(snapshot_hashes)"
./eval-48.sh baseline >/dev/null
./eval-48.sh summary >/dev/null
hashes_after_second="$(snapshot_hashes)"
[ "$hashes_before" = "$hashes_after_first" ] || {
    log "baseline artifacts changed on first deterministic replay"
    diff <(printf '%s\n' "$hashes_before") <(printf '%s\n' "$hashes_after_first") || true
    exit 1
}
[ "$hashes_after_first" = "$hashes_after_second" ] || {
    log "baseline artifacts changed on second deterministic replay"
    diff <(printf '%s\n' "$hashes_after_first") <(printf '%s\n' "$hashes_after_second") || true
    exit 1
}
log "baseline and summary artifacts replay byte-identically"

python3 -B - "$expected_binary_sha" "$expected_config_sha" <<'PY'
import hashlib
import importlib.util
import json
import math
import pathlib
import re
import subprocess
import sys
import xml.etree.ElementTree as ET

expected_binary_sha, expected_config_sha = sys.argv[1:]
root = pathlib.Path("eval-48g")

with (root / "baseline-summary.json").open(encoding="utf-8") as handle:
    summary = json.load(handle)
with (root / "baseline.json").open(encoding="utf-8") as handle:
    baseline = json.load(handle)
with (root / "heldout-aa-compare.json").open(encoding="utf-8") as handle:
    heldout = json.load(handle)

def require(condition, message):
    if not condition:
        raise SystemExit(message)

def sha256(path):
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for block in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()

def is_sha256(value):
    return isinstance(value, str) and re.fullmatch(r"[0-9a-f]{64}", value) is not None

def verify_fingerprint(record, label, expected_path=None):
    require(isinstance(record, dict), f"{label}: fingerprint missing")
    path = pathlib.Path(str(record.get("path", "")))
    if expected_path is not None:
        require(path == expected_path, f"{label}: artifact path mismatch")
    require(path.is_file(), f"{label}: artifact missing: {path}")
    require(record.get("bytes") == path.stat().st_size, f"{label}: byte count mismatch")
    require(is_sha256(record.get("sha256")), f"{label}: malformed SHA-256")
    require(sha256(path) == record["sha256"], f"{label}: SHA-256 mismatch")
    return path

require(summary.get("schema") == 1, "summary schema mismatch")
require(summary.get("kind") == "sim-latency-local-baseline-summary", "summary kind mismatch")
require(summary.get("status") == "complete", "summary is not complete")
require(summary.get("classification") == "local_directional", "summary classification mismatch")
require(summary.get("replicate_count", 0) >= 20, "fewer than 20 authenticated replicates")
require(baseline.get("replicates") == summary["replicate_count"], "baseline run count mismatch")
require(baseline.get("alpha") == 0.05, "baseline alpha mismatch")
require(summary.get("baseline_alpha") == 0.05, "summary baseline alpha mismatch")
require(summary.get("binary_sha256") == expected_binary_sha, "frozen simulator hash mismatch")
require(pathlib.Path("sim-latency").is_file(), "frozen simulator is missing")
require(sha256(pathlib.Path("sim-latency")) == expected_binary_sha, "simulator bytes changed")
verify_fingerprint(
    summary.get("campaign_script"),
    "campaign wrapper",
    pathlib.Path("eval-48.sh"),
)
require(summary["identity"].get("config_sha256") == expected_config_sha, "fixture hash mismatch")
require((root / "providers-eval48d.yml").is_file(), "frozen provider fixture is missing")
require(
    sha256(root / "providers-eval48d.yml") == expected_config_sha,
    "fixture bytes changed",
)
require(summary["identity"].get("seed") == 48, "seed mismatch")
require(
    summary["identity"].get("build_revision")
    == "05c745050657fdd31f908d7a0e06ef4e26f636d8",
    "build revision mismatch",
)
require(summary["identity"].get("build_modified") is True, "local build identity mismatch")
require(summary["identity"].get("official") is False, "local campaign unexpectedly asserted official mode")
require(summary["identity"].get("score_schema") == 1, "score schema mismatch")
require(
    summary["identity"].get("scorer_version") == "sim-latency-score/1",
    "scorer version mismatch",
)
require(summary["identity"].get("request_timeout_ms") == 120_000, "request timeout mismatch")
require(summary["identity"].get("duration_ms") == 1_800_000, "run duration mismatch")
require(summary["identity"].get("hostname") == "sille", "local host identity changed")
require(summary["identity"].get("os") == "linux", "local OS identity changed")
require(summary["identity"].get("arch") == "amd64", "local architecture changed")
require(summary["identity"].get("num_cpu") == 24, "local host CPU identity changed")
expected_flags = {
    "announce_timeout": "2s",
    "api_port": "7640",
    "duration": "30m0s",
    "exchange_port_base": "7750",
    "fleet_shards": "4",
    "hosts": "4",
    "impair": "true",
    "pipeline_interval": "10s",
    "prewarm": "13h0m0s",
    "ramp": "1m0s",
    "request_timeout": "2m0s",
    "reset": "true",
    "settle": "1m0s",
    "site_listen": "127.0.0.1:0",
    "test_timeout": "3s",
    "ws_port_base": "7650",
}
require(summary["identity"].get("flags") == expected_flags, "frozen run flags mismatch")
telemetry = summary.get("telemetry_artifacts", {})
verify_fingerprint(
    telemetry.get("rss"),
    "campaign RSS telemetry",
    root / "campaign-rss.csv",
)
verify_fingerprint(
    telemetry.get("host_resources"),
    "campaign host telemetry",
    root / "campaign-host-resources.csv",
)
verify_fingerprint(
    telemetry.get("native_services"),
    "campaign native-service telemetry",
    root / "campaign-service-resources.csv",
)
require(
    (root / "baseline-samples").is_file(),
    "sample-audit helper is missing",
)
require(
    sha256(root / "baseline-samples") == summary.get("sample_audit_sha256"),
    "sample-audit helper hash mismatch",
)
require(summary["aggregate"].get("all_warm_pools_complete") is True, "warm pool gate failed")
require(summary["aggregate"].get("all_success_rates_pass_97_percent") is True, "success gate failed")
require(summary["aggregate"].get("all_logs_clean") is True, "stability log gate failed")
resources = summary["aggregate"].get("resources", {})
require(resources.get("all_runs_have_rss_samples") is True, "RSS telemetry coverage failed")
require(
    resources.get("all_runs_have_exactly_five_sim_processes") is True,
    "runner/fleet process-count continuity failed",
)
require(resources.get("all_runs_zero_swap") is True, "swap activity found during baseline")
require(
    resources.get("service_telemetry_full_coverage_runs", 0) > 0,
    "native PostgreSQL/Redis telemetry fully covers no baseline run",
)
require(
    resources.get("all_service_covered_runs_have_postgres_and_redis") is True,
    "PostgreSQL or Redis disappeared from a service-covered run",
)
require(
    "postgres_summed_rss_peak_bytes" in resources,
    "PostgreSQL summed-process-RSS telemetry missing",
)
require(
    "redis_summed_rss_peak_bytes" in resources,
    "Redis process-RSS telemetry missing",
)
samples = summary["aggregate"].get("samples", {})
require(samples.get("all_pools_nonempty") is True, "empty FindProviders2 pool found")
require(samples.get("total_samples", 0) > 0, "no FindProviders2 samples")
require(
    samples.get("all_runs_sample_span_at_least_90_percent") is True,
    "FindProviders2 samples do not span every measured window",
)
campaign = summary.get("campaign_attempts", {})
require(campaign.get("authenticated_count") == summary["replicate_count"], "attempt inventory mismatch")
require(campaign.get("attempt_count", 0) >= summary["replicate_count"], "attempt count mismatch")
require(campaign.get("attempt_tags_contiguous") is True, "campaign attempt tags are not contiguous")
require(campaign.get("first_attempt_tag") == "r001", "campaign does not begin at r001")
require(
    campaign.get("has_20_consecutive_authenticated_tags") is True,
    "campaign has no streak of 20 consecutive authenticated attempts",
)
require(
    campaign.get("last_attempt_tag")
    == f"r{campaign['attempt_count']:03d}",
    "campaign attempt range does not match its inventory count",
)
excluded_reason_counts = {}
for attempt in campaign.get("excluded", []):
    reason = attempt.get("reason")
    excluded_reason_counts[reason] = excluded_reason_counts.get(reason, 0) + 1
    artifacts = attempt.get("artifacts", {})
    for path_field in ("csv", "manifest", "marker", "log"):
        path_value = artifacts.get(path_field)
        if path_value is None:
            require(artifacts.get(f"{path_field}_bytes") is None, f"{attempt['tag']}: absent {path_field} has a byte count")
            require(artifacts.get(f"{path_field}_sha256") is None, f"{attempt['tag']}: absent {path_field} has a hash")
            continue
        path = pathlib.Path(path_value)
        require(path.is_file(), f"{attempt['tag']}: excluded {path_field} artifact missing")
        require(artifacts.get(f"{path_field}_bytes") == path.stat().st_size, f"{attempt['tag']}: excluded {path_field} byte count mismatch")
        require(is_sha256(artifacts.get(f"{path_field}_sha256")), f"{attempt['tag']}: malformed excluded {path_field} hash")
        require(sha256(path) == artifacts[f"{path_field}_sha256"], f"{attempt['tag']}: excluded {path_field} hash mismatch")
    if (
        artifacts.get("csv") is not None
        and artifacts.get("manifest") is not None
        and attempt.get("manifest_error") is None
    ):
        expected_csv_match = (
            artifacts["csv_bytes"] == attempt.get("recorded_results_csv_bytes")
            and artifacts["csv_sha256"] == attempt.get("recorded_results_csv_sha256")
        )
    else:
        expected_csv_match = None
    require(
        attempt.get("csv_identity_matches_manifest") is expected_csv_match,
        f"{attempt['tag']}: excluded CSV identity diagnostic mismatch",
    )
require(
    campaign.get("excluded_csv_identity_mismatch_count")
    == sum(
        attempt.get("csv_identity_matches_manifest") is False
        for attempt in campaign.get("excluded", [])
    ),
    "excluded CSV identity mismatch count is incorrect",
)
require(
    campaign.get("excluded_reason_counts")
    == dict(sorted(excluded_reason_counts.items())),
    "excluded-attempt reason counts are incorrect",
)
require(len(summary.get("baseline_convergence", {})) == 3, "convergence report missing")
raw_convergence = summary["aggregate"].get("apex_raw_score_sd_convergence", {})
require(
    len(raw_convergence.get("sd_by_replicates", [])) == summary["replicate_count"] - 1,
    "raw-score convergence series length mismatch",
)
require(
    abs(
        raw_convergence["sd_by_replicates"][-1]
        - summary["aggregate"]["metrics"]["apex_raw_score_ms"]["sd"]
    )
    <= 1e-9,
    "raw-score convergence endpoint mismatch",
)
for run in summary["runs"]:
    for path_field in ("csv", "manifest", "marker", "log"):
        path = pathlib.Path(str(run.get(path_field, "")))
        require(path.is_file(), f"{run['tag']}: {path_field} artifact missing")
        require(
            run.get(f"{path_field}_bytes") == path.stat().st_size,
            f"{run['tag']}: {path_field} byte count mismatch",
        )
        require(
            is_sha256(run.get(f"{path_field}_sha256")),
            f"{run['tag']}: malformed {path_field} SHA-256",
        )
        require(
            sha256(path) == run[f"{path_field}_sha256"],
            f"{run['tag']}: {path_field} SHA-256 mismatch",
        )
    segments = run.get("sample_segment_manifest", [])
    require(len(segments) == run.get("sample_segments"), f"{run['tag']}: sample manifest count mismatch")
    require(sum(item["bytes"] for item in segments) == run.get("sample_segment_bytes"), f"{run['tag']}: sample byte count mismatch")
    corpus = hashlib.sha256()
    stream_root = pathlib.Path(run["stats_root"]) / "findproviders2"
    for item in segments:
        name = item.get("name")
        require(isinstance(name, str) and pathlib.Path(name).name == name, f"{run['tag']}: unsafe sample segment name")
        segment = stream_root / name
        require(segment.is_file(), f"{run['tag']}: sample segment missing: {name}")
        require(item.get("bytes") == segment.stat().st_size, f"{run['tag']}: sample segment byte count mismatch: {name}")
        require(is_sha256(item.get("sha256")), f"{run['tag']}: malformed sample segment hash: {name}")
        require(sha256(segment) == item["sha256"], f"{run['tag']}: sample segment hash mismatch: {name}")
        corpus.update(name.encode("utf-8"))
        corpus.update(b"\0")
        corpus.update(str(item["bytes"]).encode("ascii"))
        corpus.update(b"\0")
        corpus.update(item["sha256"].encode("ascii"))
        corpus.update(b"\n")
    require(is_sha256(run.get("sample_corpus_sha256")), f"{run['tag']}: sample corpus hash missing")
    require(corpus.hexdigest() == run["sample_corpus_sha256"], f"{run['tag']}: sample corpus hash mismatch")
summary_heldout = summary.get("heldout_aa", {})
verify_fingerprint(
    summary_heldout.get("artifact"),
    "held-out A/A comparison",
    root / "heldout-aa-compare.json",
)
verify_fingerprint(
    summary_heldout.get("host_telemetry_artifact"),
    "held-out host telemetry",
    root / "heldout-host-resources.csv",
)
require(summary_heldout.get("result") == heldout, "summary held-out A/A result mismatch")
require(heldout.get("verdict") == "indistinguishable", "held-out A/A verdict failed")
require(len(heldout.get("runs_a", [])) == 1, "held-out side A is not exactly one run")
require(len(heldout.get("runs_b", [])) == 1, "held-out side B is not exactly one run")
replayed_heldout = subprocess.run(
    [
        str(pathlib.Path.cwd() / "sim-latency"),
        "compare",
        f"--a={heldout['runs_a'][0]}",
        f"--b={heldout['runs_b'][0]}",
        f"--baseline={root / 'baseline.json'}",
        "--json",
    ],
    check=True,
    stdout=subprocess.PIPE,
).stdout
require(
    replayed_heldout == (root / "heldout-aa-compare.json").read_bytes(),
    "held-out A/A artifact did not replay byte-identically",
)

module_path = pathlib.Path("summarize-baseline.py")
spec = importlib.util.spec_from_file_location("baseline_summary", module_path)
require(spec is not None and spec.loader is not None, "cannot load baseline auditor")
auditor = importlib.util.module_from_spec(spec)
spec.loader.exec_module(auditor)
heldout_host_rows, heldout_host_artifact = auditor.load_telemetry_artifact(
    root / "heldout-host-resources.csv",
    auditor.HOST_TELEMETRY_COLUMNS,
)
require(
    heldout_host_artifact == summary_heldout["host_telemetry_artifact"],
    "held-out host telemetry audit mismatch",
)
service_rows, service_artifact = auditor.load_telemetry_artifact(
    root / "campaign-service-resources.csv",
    auditor.SERVICE_TELEMETRY_COLUMNS,
)
require(
    service_artifact == summary["telemetry_artifacts"]["native_services"],
    "native-service telemetry audit mismatch",
)
require(
    service_artifact == summary_heldout["service_telemetry_artifact"],
    "held-out native-service telemetry audit mismatch",
)
heldout_audits = []
for side, labels in (("a", heldout["runs_a"]), ("b", heldout["runs_b"])):
    for index, csv_label in enumerate(labels):
        csv_path = auditor.resolved(csv_label, pathlib.Path.cwd().resolve())
        require(csv_path.suffix == ".csv", f"held-out label is not a CSV: {csv_path}")
        marker_path = csv_path.with_name(csv_path.stem + ".run.json.complete.json")
        audit = auditor.audit_run(
            marker_path,
            pathlib.Path.cwd().resolve(),
            [],
            heldout_host_rows,
            service_rows,
        )
        require(audit["identity"] == summary["identity"], f"{csv_path}: baseline identity mismatch")
        require(
            audit == summary_heldout["audited_runs"][side][index],
            f"{csv_path}: summary held-out audit mismatch",
        )
        heldout_audits.append(audit)
require(
    len({audit["evaluation_id"] for audit in heldout_audits}) == 2,
    "held-out evaluations do not have unique identities",
)
require(
    not ({audit["evaluation_id"] for audit in heldout_audits}
         & {run["evaluation_id"] for run in summary["runs"]}),
    "held-out evaluation was included in the baseline",
)
heldout_a_raw = heldout_audits[0]["metrics"]["apex_raw_score_ms"]
heldout_b_raw = heldout_audits[1]["metrics"]["apex_raw_score_ms"]
require(
    math.isclose(
        summary_heldout.get("apex_raw_score_delta_b_minus_a_ms"),
        heldout_b_raw - heldout_a_raw,
        rel_tol=1e-12,
        abs_tol=1e-9,
    ),
    "held-out Apex raw-score delta mismatch",
)
require(
    math.isclose(
        summary_heldout.get("apex_raw_score_relative_delta_b_vs_a"),
        (heldout_b_raw - heldout_a_raw) / heldout_a_raw,
        rel_tol=1e-12,
        abs_tol=1e-12,
    ),
    "held-out Apex relative raw-score delta mismatch",
)

baseline_path = root / "baseline.json"
plot_path = root / "baseline-stability.svg"
require(sha256(baseline_path) == summary.get("baseline_sha256"), "baseline hash mismatch")
require(sha256(plot_path) == summary.get("stability_plot_sha256"), "plot hash mismatch")
ET.parse(plot_path)

drift = {}
for name, metric in summary["aggregate"]["metrics"].items():
    value = metric["drift"]
    t_stat = value.get("t_stat")
    if t_stat is None:
        drift[name] = (
            "+infinity"
            if value.get("perfect_fit_infinite_t_direction") == 1
            else "-infinity"
        )
    elif abs(t_stat) >= 2:
        drift[name] = t_stat
print(f"authenticated replicates: {summary['replicate_count']}")
print(f"excluded attempts: {campaign.get('excluded_count', 0)}")
print(f"longest authenticated tag streak: {campaign.get('longest_consecutive_authenticated_tag_streak')}")
print("|drift t| >= 2 diagnostics: " + (json.dumps(drift, sort_keys=True) if drift else "none"))
outliers = summary["aggregate"].get("robust_outlier_candidates", [])
print("robust outlier candidates: " + (json.dumps(outliers, sort_keys=True) if outliers else "none"))
print(f"held-out A/A verdict: {heldout['verdict']}")
PY
log "artifact invariants passed"

export WARP_ENV="local"
export WARP_SERVICE="test"
export WARP_DOMAIN="bringyour.com"
export WARP_BLOCK="test"
export WARP_VERSION="0.0.0"
export BRINGYOUR_POSTGRES_HOSTNAME="local-pg.bringyour.com"
export BRINGYOUR_REDIS_HOSTNAME="local-redis.bringyour.com"
export GOTOOLCHAIN="go1.26.5"

server_root="$(cd ../.. && pwd)"
cd "$server_root"
log "running package tests"
go test -count=1 ./connect/sim-latency ./stats
log "running race tests"
go test -race -count=1 ./connect/sim-latency ./stats
log "running vet and repository compile gate"
go vet ./connect/sim-latency ./stats
go test -run '^$' ./...
go test -count=1 ./connect -run '^(TestExchangeWaitForIdleJoinsAcceptedConnectionOwnership|TestExchangeWaitForIdleJoinsResidentInternalClientOwnership|TestConnectHandlerCloseJoinsAndClosesPreboundPacketConns|TestConnectionAnnounceChildWorkersCloseAdmissionAndJoin|TestHandleContractManagerDoneSuppressesCancellation|TestHandleContractManagerDoneRepanicsUnexpectedError|TestResidentContractManagerCanceledLookupReturnsInactive)$'

bash -n \
    connect/sim-latency/eval-48.sh \
    connect/sim-latency/finalize-local-baseline.sh \
    connect/sim-latency/official-run.sh \
    connect/sim-latency/sample-host-resources.sh \
    connect/sim-latency/sample-service-resources.sh \
    connect/sim-latency/sample-rss.sh \
    connect/sim-latency/verify-local-baseline.sh
python3 -B - <<'PY'
from pathlib import Path

path = Path("connect/sim-latency/summarize-baseline.py")
compile(path.read_text(encoding="utf-8"), str(path), "exec")
PY
git diff --check
log "all post-campaign verification gates passed"
