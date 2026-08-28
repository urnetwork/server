#!/usr/bin/env python3
"""Run one fresh v5 pilot with stronger controls and a shared baseline."""

from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import math
import subprocess
import time
from pathlib import Path
from typing import Any


SERVER = Path("/home/by/urnetwork/server")
ROOT = SERVER / "connect/sim-latency/eval-12c/final-calibration-p1800-cf0fd3a9"
V5 = ROOT / "reference-requalification-v5"
RUNTIME = V5 / "pilot-runtime"
CAMPAIGN = RUNTIME / "independent-references"
SOURCE_COMPAT = ROOT / "independent-launch-compromise"
SOURCE_LOCK = ROOT / "source-lock.json"
PILOT_SOURCE = V5 / "pilot-source.py"
DESIGN = V5 / "design.json"
PROTOCOL = V5 / "pilot-protocol.json"
STATIC_QUALIFICATION = V5 / "static-qualification.json"
RETIRED_COMMITMENTS = V5 / "retired-seed-commitments.json"
PILOT_DECISION = V5 / "pilot-decision.json"
PILOT_REVEAL = V5 / "pilot-seed-reveal.json"
V1_REJECTION = ROOT / "independent-reference-v1-rejection.json"
V2_INVALID_REJECTION = ROOT / "reference-requalification-v2/pilot-attempt-02-invalid-worse-rejection.json"
V3_REJECTION = ROOT / "reference-requalification-v3/pilot-rejection.json"
V4_REJECTION = ROOT / "reference-requalification-v4/hidden-campaign-rejection.json"
BETTER = V5 / "better.patch"
NOOP = V5 / "noop.patch"
WORSE = V5 / "worse.patch"

SOURCE_LOCK_SHA256 = "0cf71458833f3b1ae96a663357c583eba3a9c25a19d6c795c8549e4154141838"
PILOT_SOURCE_SHA256 = "c347d61a959d4ba18282737077a22ec13b14c152e52b8a8ad4b351f2d0e626a2"
TRANSFORMED_SOURCE_SHA256 = "77c66465fb27b4bd8ee84aede3f03803349e1d62c9bb824396a224a8889f9078"
DESIGN_SHA256 = "6e05b1872648a0d9f28755e5ca4b0470445ea40e95b4fdb991e676d2d453ffa1"
STATIC_QUALIFICATION_SHA256 = "4b51548ff4910cd8d1b79247973cd47e65680b814b4ffa5c1b9153bf61d718fd"
RETIRED_COMMITMENTS_SHA256 = "1a17718ead0b2d5114be670a2b155679c92ac95d79a58160b161c8f0b03a7a04"
V1_REJECTION_SHA256 = "2a7ed54c73c7d07c27abde6b84a6147c577d199d47ce48e56c9f7ec843420bbf"
V2_INVALID_REJECTION_SHA256 = "89205a071866417fd768364539aa8092eea5d3bd5156a6507969fa37abe6b4a4"
V3_REJECTION_SHA256 = "f160ef6188e3f962317355a6971cea8b8dbadfb84f62d499be7f5f49cc39620c"
V4_REJECTION_SHA256 = "d1a782831e9cfedfbe9c5835385f490e655ca38856698b88c59ee91f1ca993e1"
BETTER_SHA256 = "1a81e5a5fb7897cee38eb3952ed0db82a6cccb4a7821eb9db84d93eb55d9ff82"
NOOP_SHA256 = "8bd57a48ac82a6e846b607a9301c48145da5c66717c9e3a341138d034d1e0775"
WORSE_SHA256 = "982b192198ffa63942db1804629844f1cf9801bd4a71f64d2847a305217257a0"
EXPECTED_BOOT_ID = "34760d1b-a0b6-46a0-b8c1-264abd1affba"
ORDERING_METRIC = "candidate_raw_score_ms_over_designated_baseline_raw_score_ms"


class PilotError(RuntimeError):
    pass


def require(condition: bool, message: str) -> None:
    if not condition:
        raise PilotError(message)


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def load_json(path: Path) -> dict[str, Any]:
    require(path.is_file() and not path.is_symlink(), f"unsafe JSON input: {path}")
    value = json.loads(path.read_text(encoding="utf-8"))
    require(isinstance(value, dict), f"{path}: expected an object")
    return value


def atomic_json(path: Path, value: dict[str, Any], mode: int = 0o400) -> None:
    encoded = json.dumps(value, indent=2, sort_keys=True, allow_nan=False) + "\n"
    if path.exists():
        require(path.is_file() and not path.is_symlink(), f"unsafe artifact: {path}")
        require(path.read_text(encoding="utf-8") == encoded, f"artifact changed: {path}")
        return
    pending = path.with_name(path.name + ".new")
    require(not pending.exists(), f"pending artifact exists: {pending}")
    pending.write_text(encoded, encoding="utf-8")
    pending.chmod(mode)
    pending.replace(path)


def atomic_copy(source: Path, destination: Path, expected_sha256: str) -> None:
    require(sha256(source) == expected_sha256, f"copy source changed: {source}")
    if destination.exists():
        require(
            destination.is_file()
            and not destination.is_symlink()
            and sha256(destination) == expected_sha256,
            f"copied artifact changed: {destination}",
        )
        return
    pending = destination.with_name(destination.name + ".new")
    require(not pending.exists(), f"pending artifact exists: {pending}")
    pending.write_bytes(source.read_bytes())
    pending.chmod(0o400)
    pending.replace(destination)


def command_text(args: list[str], cwd: Path = SERVER) -> str:
    result = subprocess.run(args, cwd=cwd, check=True, capture_output=True, text=True)
    return result.stdout.strip()


def transformed_source() -> str:
    require(sha256(PILOT_SOURCE) == PILOT_SOURCE_SHA256, "pilot source adapter changed")
    spec = importlib.util.spec_from_file_location("reference_v5_pilot_source", PILOT_SOURCE)
    require(spec is not None and spec.loader is not None, "cannot load pilot source adapter")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    source = module.transformed_source()
    require(source.count(ORDERING_METRIC) == 2, "ordering metric transform coverage")
    require(hashlib.sha256(source.encode("utf-8")).hexdigest() == TRANSFORMED_SOURCE_SHA256, "transformed source changed")
    return source


def shared_ratios(result: dict[str, Any]) -> dict[str, float]:
    designated = result.get("designated_baseline")
    require(isinstance(designated, dict), "designated baseline")
    baseline = designated.get("raw_score_ms")
    require(isinstance(baseline, (int, float)) and math.isfinite(baseline) and baseline > 0, "designated baseline score")
    references = result.get("references")
    require(isinstance(references, dict), "references")
    ratios: dict[str, float] = {}
    for name in ("better", "noop", "worse"):
        entry = references.get(name)
        require(isinstance(entry, dict), f"{name}: reference")
        candidate = entry.get("candidate_raw_score_ms")
        require(isinstance(candidate, (int, float)) and math.isfinite(candidate) and candidate > 0, f"{name}: candidate score")
        ratios[name] = float(candidate) / float(baseline)
    return ratios


def verify_seed_result(path: Path) -> dict[str, Any]:
    result = load_json(path)
    require(
        result.get("schema") == 1
        and result.get("kind") == "sim-latency-independent-seed-result"
        and result.get("seed_index") == 1
        and result.get("replicates_per_reference") == 1
        and result.get("ordering_metric") == ORDERING_METRIC,
        "pilot seed-result identity",
    )
    references = result.get("references")
    require(isinstance(references, dict) and set(references) == {"better", "noop", "worse"}, "pilot references")
    expected = {"better": BETTER_SHA256, "noop": NOOP_SHA256, "worse": WORSE_SHA256}
    for name, patch_sha in expected.items():
        entry = references[name]
        require(isinstance(entry, dict) and entry.get("patch_sha256") == patch_sha, f"{name}: patch digest")
        paired = entry.get("paired_ratio")
        require(isinstance(paired, (int, float)) and math.isfinite(paired) and paired > 0, f"{name}: paired ratio diagnostic")
        relative = entry.get("attempt_directory")
        require(isinstance(relative, str) and not relative.startswith("/"), f"{name}: attempt path")
        attempt = CAMPAIGN / relative
        require(attempt.is_dir() and not attempt.is_symlink(), f"{name}: attempt directory")
        worker = load_json(attempt / "worker-result.json")
        require(worker.get("score", {}).get("raw_score") == entry.get("candidate_raw_score_ms"), f"{name}: candidate score binding")
        require(sha256(attempt / "worker-result.json") == entry.get("worker_result_sha256"), f"{name}: worker result hash")
        require(sha256(attempt / "evidence-manifest.json") == entry.get("evidence_manifest_sha256"), f"{name}: evidence hash")
    ratios = shared_ratios(result)
    require(result.get("ordering_passed") is (ratios["better"] < ratios["noop"] < ratios["worse"]), "shared-baseline ordering arithmetic")
    return result


def verify_static_inputs() -> None:
    expected = {
        SOURCE_LOCK: SOURCE_LOCK_SHA256,
        PILOT_SOURCE: PILOT_SOURCE_SHA256,
        DESIGN: DESIGN_SHA256,
        STATIC_QUALIFICATION: STATIC_QUALIFICATION_SHA256,
        RETIRED_COMMITMENTS: RETIRED_COMMITMENTS_SHA256,
        V1_REJECTION: V1_REJECTION_SHA256,
        V2_INVALID_REJECTION: V2_INVALID_REJECTION_SHA256,
        V3_REJECTION: V3_REJECTION_SHA256,
        V4_REJECTION: V4_REJECTION_SHA256,
        BETTER: BETTER_SHA256,
        NOOP: NOOP_SHA256,
        WORSE: WORSE_SHA256,
    }
    for path, digest in expected.items():
        require(path.is_file() and not path.is_symlink(), f"missing static input: {path}")
        require(sha256(path) == digest, f"static input changed: {path}")
    v3_rejection = load_json(V3_REJECTION)
    require(
        v3_rejection.get("kind") == "sim-latency-reference-v3-pilot-rejection"
        and v3_rejection.get("accepted") is False
        and v3_rejection.get("retired_pilot_seed", {}).get("seed_reuse_forbidden") is True,
        "v3 pilot rejection state",
    )
    v4_rejection = load_json(V4_REJECTION)
    require(
        v4_rejection.get("kind") == "sim-latency-reference-v4-hidden-campaign-rejection"
        and v4_rejection.get("accepted") is False
        and v4_rejection.get("completed_seeds") == 5
        and v4_rejection.get("required_passes") == 4
        and v4_rejection.get("all_precommitted_measurements_retained") is True,
        "v4 hidden campaign rejection state",
    )
    static_qualification = load_json(STATIC_QUALIFICATION)
    require(
        static_qualification.get("kind") == "sim-latency-reference-v5-static-qualification"
        and static_qualification.get("accepted_for_full_scale_evaluator_pilot") is True
        and static_qualification.get("official_reference_set_accepted") is False
        and static_qualification.get("performance_acceptance_pending") is True,
        "v5 static qualification",
    )
    retired = load_json(RETIRED_COMMITMENTS)
    retired_values = retired.get("commitments")
    require(
        retired.get("kind") == "sim-latency-retired-seed-commitment-set"
        and retired.get("seed_material_included") is False
        and retired.get("commitment_count") == 21
        and isinstance(retired_values, list)
        and all(isinstance(value, str) and len(value) == 64 for value in retired_values)
        and len(retired_values) == len(set(retired_values)) == 21,
        "retired seed commitments",
    )
    design = load_json(DESIGN)
    require(
        design.get("kind") == "sim-latency-reference-v5-design"
        and design.get("draft") is False
        and design.get("authorized") is True
        and design.get("ordering_metric") == ORDERING_METRIC
        and design.get("pilot_source_sha256") == PILOT_SOURCE_SHA256
        and design.get("transformed_campaign_shell_sha256") == TRANSFORMED_SOURCE_SHA256
        and design.get("v4_hidden_campaign_rejection_sha256") == V4_REJECTION_SHA256
        and design.get("static_qualification_sha256") == STATIC_QUALIFICATION_SHA256
        and design.get("retired_seed_commitments_sha256") == RETIRED_COMMITMENTS_SHA256,
        "v5 design",
    )
    protocol = load_json(PROTOCOL)
    require(
        protocol.get("kind") == "sim-latency-reference-v5-pilot-protocol"
        and protocol.get("draft") is False
        and protocol.get("authorized") is True
        and protocol.get("target_fresh_seeds") == 1
        and protocol.get("reference_required_passes") == 1
        and protocol.get("ordering_metric") == ORDERING_METRIC
        and protocol.get("placeability_is_diagnostic_only") is True
        and protocol.get("runner_sha256") == sha256(Path(__file__).resolve())
        and protocol.get("pilot_source_sha256") == PILOT_SOURCE_SHA256
        and protocol.get("transformed_campaign_shell_sha256") == TRANSFORMED_SOURCE_SHA256
        and protocol.get("design_sha256") == DESIGN_SHA256
        and protocol.get("v4_hidden_campaign_rejection_sha256") == V4_REJECTION_SHA256
        and protocol.get("static_qualification_sha256") == STATIC_QUALIFICATION_SHA256
        and protocol.get("retired_seed_commitments_sha256") == RETIRED_COMMITMENTS_SHA256
        and protocol.get("patch_sha256") == {"better": BETTER_SHA256, "noop": NOOP_SHA256, "worse": WORSE_SHA256},
        "v5 pilot protocol",
    )
    source_lock = load_json(SOURCE_LOCK)
    boot_id = Path("/proc/sys/kernel/random/boot_id").read_text(encoding="utf-8").strip()
    require(boot_id == EXPECTED_BOOT_ID, "host rebooted after qualification")
    require(source_lock.get("host", {}).get("boot_id") == boot_id, "source-lock boot id")
    repositories = source_lock.get("repositories")
    require(isinstance(repositories, dict), "source-lock repositories")
    for name, expected_head in repositories.items():
        repository = Path("/home/by/urnetwork") / str(name)
        require(command_text(["git", "rev-parse", "HEAD"], repository) == expected_head, f"{name}: HEAD changed")
        require(not command_text(["git", "status", "--porcelain", "--untracked-files=no"], repository), f"{name}: tracked worktree changed")
    for patch in (BETTER, NOOP, WORSE):
        subprocess.run(["git", "apply", "--check", "--whitespace=error-all", str(patch)], cwd=SERVER, check=True, capture_output=True)
    require(
        not command_text(["sudo", "-n", "docker", "ps", "-aq", "--filter", "label=com.urnetwork.competition.job-id"]),
        "another evaluator job exists",
    )


def prepare_runtime() -> None:
    RUNTIME.mkdir(mode=0o700, parents=True, exist_ok=True)
    source_analysis = SOURCE_COMPAT / "same-seed-analysis.json"
    source_scale = SOURCE_COMPAT / "frontier-scale-decision.json"
    source_decision = SOURCE_COMPAT / "calibration-decision.json"
    atomic_copy(source_analysis, RUNTIME / source_analysis.name, sha256(source_analysis))
    atomic_copy(source_scale, RUNTIME / source_scale.name, sha256(source_scale))
    atomic_copy(SOURCE_LOCK, RUNTIME / SOURCE_LOCK.name, SOURCE_LOCK_SHA256)
    decision = load_json(source_decision)
    decision.update(
        {
            "independent_seed_target": 1,
            "reference_required_passes": 1,
            "reference_patch_sha256": {"better": BETTER_SHA256, "noop": NOOP_SHA256, "worse": WORSE_SHA256},
            "reference_v5_pilot": True,
            "reference_ordering_metric": ORDERING_METRIC,
            "one_designated_same_round_baseline_per_seed": True,
            "reference_placeability_diagnostic_only": True,
            "reference_v5_pilot_protocol_sha256": sha256(PROTOCOL),
            "reference_v5_design_sha256": DESIGN_SHA256,
            "reference_v5_static_qualification_sha256": STATIC_QUALIFICATION_SHA256,
            "retired_seed_commitments_sha256": RETIRED_COMMITMENTS_SHA256,
            "reference_v4_hidden_campaign_rejection_sha256": V4_REJECTION_SHA256,
            "reference_v3_pilot_rejection_sha256": V3_REJECTION_SHA256,
            "reference_v2_invalid_pilot_rejection_sha256": V2_INVALID_REJECTION_SHA256,
            "reference_v1_rejection_sha256": V1_REJECTION_SHA256,
        }
    )
    atomic_json(RUNTIME / "calibration-decision.json", decision)


def write_reveal() -> None:
    private_path = CAMPAIGN / "seed-01/round-private.json"
    public_path = CAMPAIGN / "seed-01/round-public.json"
    commitment_path = CAMPAIGN / "campaign-commitment.json"
    private = load_json(private_path)
    public = load_json(public_path)
    commitment = load_json(commitment_path)
    require(private.get("seed_commitment") == public.get("seed_commitment"), "pilot commitment mismatch")
    require(
        isinstance(commitment.get("seeds"), list)
        and len(commitment["seeds"]) == 1
        and commitment["seeds"][0].get("seed_commitment") == private.get("seed_commitment"),
        "pilot campaign commitment",
    )
    retired = load_json(RETIRED_COMMITMENTS)
    prior_commitments = retired.get("commitments")
    require(isinstance(prior_commitments, list), "retired commitment set")
    require(private.get("seed_commitment") not in prior_commitments, "retired seed reused")
    atomic_json(
        PILOT_REVEAL,
        {
            "schema": 1,
            "kind": "sim-latency-reference-v5-pilot-seed-reveal",
            "revealed_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "round_id": private["round_id"],
            "round_seed_hex": private["round_seed_hex"],
            "generator_seed": private["generator_seed"],
            "seed_commitment": private["seed_commitment"],
            "providers_sha256": private["providers_sha256"],
            "campaign_commitment_sha256": sha256(commitment_path),
            "v4_hidden_campaign_rejection_sha256": V4_REJECTION_SHA256,
            "retired_seed_commitments_sha256": RETIRED_COMMITMENTS_SHA256,
            "pilot_seed_reuse_in_hidden_campaign_forbidden": True,
        },
    )


def residual_counts() -> tuple[int, int]:
    containers = command_text(["sudo", "-n", "docker", "ps", "-aq", "--filter", "label=com.urnetwork.competition.job-id"]).splitlines()
    networks = command_text(["sudo", "-n", "docker", "network", "ls", "-q", "--filter", "label=com.urnetwork.competition.job-id"]).splitlines()
    return len([item for item in containers if item]), len([item for item in networks if item])


def pilot_accepted(
    campaign_rc: int,
    result_ordering_passed: bool,
    strict_ordering_passed: bool,
    residual_containers: int,
    residual_networks: int,
) -> bool:
    """Apply only the predeclared ordering, process, and cleanup pilot gates."""
    return (
        campaign_rc == 0
        and result_ordering_passed
        and strict_ordering_passed
        and residual_containers == 0
        and residual_networks == 0
    )


def write_decision(campaign_rc: int) -> bool:
    result_path = CAMPAIGN / "seed-01/seed-result.json"
    result = verify_seed_result(result_path)
    write_reveal()
    references = result["references"]
    ratios = shared_ratios(result)
    paired = {name: references[name]["paired_ratio"] for name in ("better", "noop", "worse")}
    candidates = {name: references[name]["candidate_raw_score_ms"] for name in ("better", "noop", "worse")}
    placeable = {name: references[name]["placeable"] for name in ("better", "noop", "worse")}
    ordering = ratios["better"] < ratios["noop"] < ratios["worse"]
    containers, networks = residual_counts()
    accepted = pilot_accepted(
        campaign_rc,
        result.get("ordering_passed") is True,
        ordering,
        containers,
        networks,
    )
    decision = {
        "schema": 1,
        "kind": "sim-latency-reference-v5-pilot-decision",
        "decided_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "accepted": accepted,
        "campaign_exit_code": campaign_rc,
        "ordering_metric": ORDERING_METRIC,
        "one_designated_same_round_baseline_per_seed": True,
        "designated_baseline": result["designated_baseline"],
        "strict_ordering_passed": ordering,
        "better_and_noop_placeable": placeable["better"] is True and placeable["noop"] is True,
        "placeability_is_diagnostic_only": True,
        "designated_baseline_ratios": ratios,
        "candidate_raw_score_ms": candidates,
        "private_paired_ratios_diagnostic_only": paired,
        "placeable": placeable,
        "failed_gate_ids": {name: references[name]["failed_gate_ids"] for name in ("better", "noop", "worse")},
        "source_lock_sha256": SOURCE_LOCK_SHA256,
        "design_sha256": DESIGN_SHA256,
        "protocol_sha256": sha256(PROTOCOL),
        "runner_sha256": sha256(Path(__file__).resolve()),
        "pilot_source_sha256": PILOT_SOURCE_SHA256,
        "transformed_campaign_shell_sha256": TRANSFORMED_SOURCE_SHA256,
        "v1_rejection_sha256": V1_REJECTION_SHA256,
        "v2_invalid_pilot_rejection_sha256": V2_INVALID_REJECTION_SHA256,
        "v3_pilot_rejection_sha256": V3_REJECTION_SHA256,
        "v4_hidden_campaign_rejection_sha256": V4_REJECTION_SHA256,
        "static_qualification_sha256": STATIC_QUALIFICATION_SHA256,
        "retired_seed_commitments_sha256": RETIRED_COMMITMENTS_SHA256,
        "campaign_commitment_sha256": sha256(CAMPAIGN / "campaign-commitment.json"),
        "seed_result_sha256": sha256(result_path),
        "seed_reveal_sha256": sha256(PILOT_REVEAL),
        "cleanup": {"residual_competition_containers": containers, "residual_competition_networks": networks},
        "promotion_scope": "Admission to a fresh hidden-seed campaign only; this disclosed pilot is not separability evidence.",
        "pilot_seed_reuse_in_hidden_campaign_forbidden": True,
    }
    atomic_json(PILOT_DECISION, decision)
    return accepted


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    verify_static_inputs()
    source = transformed_source()
    syntax = subprocess.run(["bash", "-n", "-s"], cwd=SERVER, input=source.encode("utf-8"), check=False, capture_output=True)
    require(syntax.returncode == 0, "pilot shell syntax: " + syntax.stderr.decode("utf-8", "replace"))
    for better, noop, worse, baseline in ((10.0, 11.0, 12.0, 7.0), (1.0, 2.0, 3.0, 100.0)):
        require(better / baseline < noop / baseline < worse / baseline, "common-baseline ordering self-test")
    require(pilot_accepted(0, True, True, 0, 0), "pilot acceptance positive self-test")
    for rejected in (
        pilot_accepted(1, True, True, 0, 0),
        pilot_accepted(0, False, True, 0, 0),
        pilot_accepted(0, True, False, 0, 0),
        pilot_accepted(0, True, True, 1, 0),
        pilot_accepted(0, True, True, 0, 1),
    ):
        require(not rejected, "pilot acceptance negative self-test")
    print("reference-v5 pilot self-test: inputs, common-baseline arithmetic, and transformed runner passed", flush=True)
    if args.self_test:
        return 0
    if PILOT_DECISION.exists():
        decision = load_json(PILOT_DECISION)
        require(decision.get("kind") == "sim-latency-reference-v5-pilot-decision", "existing pilot decision")
        return 0 if decision.get("accepted") is True else 1
    prepare_runtime()
    result = subprocess.run(["bash", "-s", "--"], cwd=SERVER, input=source.encode("utf-8"), check=False)
    accepted = write_decision(result.returncode)
    print(f"reference-v5 pilot decision: accepted={str(accepted).lower()}", flush=True)
    return 0 if accepted else 1


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (PilotError, OSError, ValueError, subprocess.SubprocessError) as exc:
        raise SystemExit(f"reference-v5 pilot: {exc}") from exc
