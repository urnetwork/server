#!/usr/bin/env python3
"""Fail-closed completion audit for the locked p1800 finalization campaign."""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import subprocess
import sys
import tempfile
from html.parser import HTMLParser
from pathlib import Path
from typing import Any
import uuid


SERVER = Path("/home/by/urnetwork/server")
URNETWORK = Path("/home/by/urnetwork")
ROOT = Path(
    "/home/by/urnetwork/server-finalization-evidence/connect/sim-latency/"
    "eval-12c/final-calibration-p1800-cf0fd3a9"
)
EVIDENCE_WORKTREE = Path("/home/by/urnetwork/server-finalization-evidence")
HISTORICAL_ROOT = (
    SERVER / "connect/sim-latency/eval-12c/final-calibration-p1800-cf0fd3a9"
)
ROUND = HISTORICAL_ROOT / "post-frontier/p1800-c200-r80-q2"
SAME = ROUND / "same-seed"
REFERENCE_V5 = HISTORICAL_ROOT / "reference-requalification-v5"
INDEPENDENT_ROOT = REFERENCE_V5 / "hidden-launch-runtime"
INDEPENDENT = INDEPENDENT_ROOT / "independent-references"

SOURCE_LOCK = ROOT / "source-lock.json"
SEASON_BASE_EQUIVALENCE = HISTORICAL_ROOT / "season-base-equivalence.json"
PATCH_POLICY = SERVER / "competition/container/policy.example.json"
FRONTIER = HISTORICAL_ROOT / "exact-frontier/frontier-decision.json"
POINT = HISTORICAL_ROOT / "exact-frontier/p1800-c200-r80-q2/point-summary.json"
AMENDMENT = HISTORICAL_ROOT / "launch-readiness-measurement-amendment.json"
PLACEABILITY_POLICY = (
    HISTORICAL_ROOT / "launch-readiness-placeability-policy-amendment.json"
)
POSTPROCESSING_REPAIR = HISTORICAL_ROOT / "same-seed-postprocessing-repair.json"
R1_CORRECTION = HISTORICAL_ROOT / "independent-reference-r1-correction.json"
R1_PROTOCOL = HISTORICAL_ROOT / "independent-launch-compromise-protocol.json"
R1_LAUNCH = HISTORICAL_ROOT / "independent-r1-launch.json"
PARALLEL_EVIDENCE = HISTORICAL_ROOT / "parallel-readiness-evidence.json"
CONTROL_BOUNDARY = HISTORICAL_ROOT / "control-plane-secret-boundary.json"
CONTROL_BOUNDARY_VERIFIER = (
    HISTORICAL_ROOT / "verify-control-plane-secret-boundary.py"
)
CONTROL_SOURCE_RELEASE = ROOT / "control-plane-release/source-release.json"
CONTROL_SOURCE_WORKTREE = Path("/home/by/urnetwork/server-finalization-control-plane")
SAME_PROGRESS = SAME / "progress.json"
SAME_ANALYSIS = ROUND / "same-seed-analysis.json"
STRICT_SAME_ANALYSIS = ROUND / "same-seed-analysis-familywise.json"
PRE_REPAIR_PROGRESS = SAME / "progress-before-postprocessing-repair.json"
SAME_DECISION = ROUND / "calibration-decision.json"
SELECTION = HISTORICAL_ROOT / "post-frontier/final-calibration-selection.json"
SELECTION_ATTESTATION = (
    HISTORICAL_ROOT / "post-frontier/launch-compromise-selection-attestation.json"
)
INDEPENDENT_PROGRESS = INDEPENDENT / "progress.json"
INDEPENDENT_COMMITMENT = INDEPENDENT / "campaign-commitment.json"
INDEPENDENT_REVEAL = INDEPENDENT / "seed-reveal.json"
INDEPENDENT_ATTESTATION = INDEPENDENT_ROOT / "independent-campaign-attestation.json"
INDEPENDENT_DECISION = INDEPENDENT_ROOT / "calibration-decision.json"
INDEPENDENT_ANALYSIS = INDEPENDENT_ROOT / "same-seed-analysis.json"
INDEPENDENT_TERMINAL_DECISION = REFERENCE_V5 / "hidden-launch-decision.json"
INDEPENDENT_TERMINAL_REVEAL = REFERENCE_V5 / "hidden-launch-seed-reveal.json"
INDEPENDENT_ATTESTATION_REPAIR = (
    REFERENCE_V5 / "hidden-attestation-path-repair.json"
)
INDEPENDENT_ATTESTATION_REPAIR_SCRIPT = (
    REFERENCE_V5 / "repair-hidden-attestation-path.py"
)
INDEPENDENT_PROTOCOL = REFERENCE_V5 / "hidden-launch-protocol.json"
INDEPENDENT_MEASUREMENT_AMENDMENT = (
    REFERENCE_V5 / "hidden-launch-measurement-amendment.json"
)
REFERENCE_V5_DESIGN = REFERENCE_V5 / "design.json"
REFERENCE_V5_STATIC_QUALIFICATION = REFERENCE_V5 / "static-qualification.json"
REFERENCE_V5_QUALIFICATION = REFERENCE_V5 / "qualification.json"
REFERENCE_V5_PILOT_DECISION = REFERENCE_V5 / "pilot-decision.json"
REFERENCE_V5_PILOT_REVEAL = REFERENCE_V5 / "pilot-seed-reveal.json"
REFERENCE_V5_RUNNER = REFERENCE_V5 / "run-hidden-launch.py"
REFERENCE_V5_RETIRED_COMMITMENTS = (
    REFERENCE_V5 / "retired-seed-commitments-before-hidden.json"
)
REFERENCE_V5_PRE_PILOT_RETIRED_COMMITMENTS = (
    REFERENCE_V5 / "retired-seed-commitments.json"
)
REFERENCE_V4_REJECTION = (
    HISTORICAL_ROOT / "reference-requalification-v4/hidden-campaign-rejection.json"
)
STAGING_REFERENCE_V5_AMENDMENT = (
    HISTORICAL_ROOT / "production-staging-reference-v5-amendment.json"
)
PRODUCTION_PROTOCOL = HISTORICAL_ROOT / "production-staging-protocol.json"
READINESS = ROOT / "production-readiness-final.json"
REMEDIATION_AMENDMENT = (
    ROOT / "production-staging-attempt-06-remediation-amendment.json"
)
HOST_SELF_CHECK = ROOT / "host-self-check-attempt-06.json"
CALIBRATION_MD = SERVER / "connect/sim-latency/APEX-CALIBRATION.md"
FINAL_REPORT = SERVER / "finalize-report.html"
FINAL_PREVIEW = SERVER / "final-preview.html"
FINAL_REPORT_EVIDENCE = ROOT / "finalize-report-evidence.json"

SOURCE_LOCK_SHA256 = (
    "94c25024a92b5fcb5fa8bf324ff8022fde1074fd62bc210fc0ad5efbba0e4022"
)
HISTORICAL_SOURCE_LOCK_SHA256 = (
    "0cf71458833f3b1ae96a663357c583eba3a9c25a19d6c795c8549e4154141838"
)
SERVER_COMMIT = "46515d82fe98ff666c61b2b5bb1d34a89cf4dad8"
HISTORICAL_SERVER_COMMIT = "5ca3d5242f4a7d40efe4415635608023b05a0956"
EVALUATOR_IMAGE = (
    "sha256:2abcf145c0f914899debbd2fd52e57a16cf20072165c8d13f04a0ba487198a4c"
)
HISTORICAL_EVALUATOR_IMAGE = (
    "sha256:cf0fd3a9e73385729ee8dcd8da7ea53eb59d5f372b9ff36789ec923056222038"
)
HOST_QUALIFICATION_SHA256 = (
    "acf226db6b8e50d67f8957cddb3903d5d4e9e82566935d61d270ccb5b03463a3"
)
REMEDIATION_AMENDMENT_SHA256 = (
    "7971eeeac22c73781c0de1ce34c5296f79b2f223afbfe67d4a7b3fd2642de65d"
)
RELEASE_SELF_CHECK_AMENDMENT_SHA256 = (
    "99d6010edcbc659d936e97cbc7cde48129d0af9146c6404a1bc03604d750ef5d"
)
SEASON_BASE_EQUIVALENCE_SHA256 = (
    "6bce6a80cecfee0297bcc11afbaa390576d8f542980d8797e4da33046daa07b3"
)
PATCH_POLICY_SHA256 = (
    "2dba553cd94d6d901e0fc590fd147d3e39273b41c24317e987b1bbf479382460"
)
AMENDMENT_SHA256 = (
    "3bd163e339cc7dc8e23757dd23ea238607f7eb6eaecc1959acd412661b9a770f"
)
PLACEABILITY_POLICY_SHA256 = (
    "359fe89572c81d6602cbf0ece03e5128c5ccbf38bf2d20f22fcdbadd30f2f638"
)
POSTPROCESSING_REPAIR_SHA256 = (
    "62c7e8c1398d8619bf9bfb4645b0e7965a4e9a83713d47e0da86fc705ecd59dd"
)
STRICT_SAME_ANALYSIS_SHA256 = (
    "7d11b12fba4b0168f52430cf55e2a3cfbc2df286193cfc9909f9d0aff5088741"
)
PRE_REPAIR_PROGRESS_SHA256 = (
    "6dca4d29e1a61df3427229923dfa22bbc4ac776c30db3f8f2e7a6bc574dcf8f0"
)
R1_CORRECTION_SHA256 = (
    "b500ac07ac7272e8ff839d3bdf6f5ebcdc327d254b1a6d0a5d6078b64831dafa"
)
R1_PROTOCOL_SHA256 = (
    "3e0a5cd98a2a96b2f1a4b59c5628c3d5e3d81de08bdbe52d82b83e2d9a20c583"
)
REFERENCE_PATCH_SHA256 = {
    "better": "1a81e5a5fb7897cee38eb3952ed0db82a6cccb4a7821eb9db84d93eb55d9ff82",
    "noop": "8bd57a48ac82a6e846b607a9301c48145da5c66717c9e3a341138d034d1e0775",
    "worse": "982b192198ffa63942db1804629844f1cf9801bd4a71f64d2847a305217257a0",
}
PARALLEL_EVIDENCE_SHA256 = (
    "f8fb1f228a610fe8715b861d927982ab58fd6075d1b400d21f5953af18bcf828"
)
CONTROL_BOUNDARY_SHA256 = (
    "56256912a912d7d0c5de1a1c7c6399eefa460fc69a187eac564062e49a769568"
)
CONTROL_BOUNDARY_VERIFIER_SHA256 = (
    "70c42b13b623ba92c81dc9089232fe216d1d13029bcb257d677133977e953fb6"
)
CONTROL_SOURCE_RELEASE_SHA256 = (
    "0fdf035ee23fb030936d1340ddfaeb80cbcb23fde4c0000bff9f93e70e736818"
)
CONTROL_SOURCE_COMMIT = "2ee4883f2b77cccfcbc69b3bcf1cb4ee613dad36"
HISTORICAL_CONTROL_SOURCE_COMMIT = "5070445ddb1764ad80f999102a9d71946e5a9e29"
HISTORICAL_CONTROL_SOURCE_RELEASE_SHA256 = (
    "b942c70bae7e69bf08c811084075a094d4cbb18d74083e53a8935de110f4c940"
)
PRODUCTION_PROTOCOL_SHA256 = (
    "6fc4a809779bf6e694ef3afa71522fa50d0512c56177b42da4249738a37dc7af"
)
REFERENCE_V5_DESIGN_SHA256 = (
    "6e05b1872648a0d9f28755e5ca4b0470445ea40e95b4fdb991e676d2d453ffa1"
)
REFERENCE_V5_STATIC_QUALIFICATION_SHA256 = (
    "4b51548ff4910cd8d1b79247973cd47e65680b814b4ffa5c1b9153bf61d718fd"
)
REFERENCE_V5_RETIRED_COMMITMENTS_SHA256 = (
    "4fe791ae5cc6fd838fad7cba6727c7325f27e5dda8fb5f4731b15359ddbd7eaf"
)
REFERENCE_V5_PRE_PILOT_RETIRED_COMMITMENTS_SHA256 = (
    "1a17718ead0b2d5114be670a2b155679c92ac95d79a58160b161c8f0b03a7a04"
)
REFERENCE_V4_REJECTION_SHA256 = (
    "d1a782831e9cfedfbe9c5835385f490e655ca38856698b88c59ee91f1ca993e1"
)
SAME_SEED_TARGET = 12
INDEPENDENT_TARGET = 5
INDEPENDENT_REQUIRED_PASSES = 4
PRIOR_INDEPENDENT_TARGET = 12
PRIOR_REQUIRED_PASSES = 11
ORDERING_METRIC = (
    "candidate_raw_score_ms_over_designated_baseline_raw_score_ms"
)
SECURITY_BOOLEAN_IDS = {
    "template_database_reset",
    "redis_reset",
    "cgroup_contained",
    "resource_limits",
    "management_cpu_reserved",
    "management_memory_reserved",
    "default_deny_network",
    "offline_build",
    "offline_build_resource_limits",
    "no_production_secrets",
    "structural_patch_check",
    "accounting_complete",
    "resource_report_complete",
    "cleanup_complete",
    "immutable_reports",
}
SECURITY_ID_IDS = {
    "cgroup_id",
    "template_database_id",
    "redis_generation_id",
}
PRODUCTION_ASSERTIONS = {
    "release_artifacts": {
        "api_binary_cgo_disabled",
        "worker_binary_cgo_disabled",
        "rebaseline_binary_cgo_disabled",
        "dbinit_binary_cgo_disabled",
        "api_image_sha256_pinned",
        "worker_image_sha256_pinned",
        "openapi_hash_verified",
        "source_commit_verified",
        "digest_pinned_buildkit_verified",
        "digest_pinned_sbom_generator_verified",
        "api_slsa_v1_provenance_verified",
        "worker_slsa_v1_provenance_verified",
        "api_spdx_sbom_verified",
        "worker_spdx_sbom_verified",
        "no_config_or_vault_in_images",
        "attempt_06_remediation_bound",
        "candidate_base_change_authorized",
    },
    "service_backed_fifo_cache_failover": {
        "postgres_dedicated_address",
        "redis_dedicated_address",
        "origin_migrations_before_local",
        "fifo_verified",
        "cache_acl_verified",
        "singleton_slot_verified",
        "lease_failover_verified",
        "infrastructure_retry_verified",
        "terminal_fields_immutable",
        "event_log_append_only",
        "test_exit_zero",
        "attempt_06_remediation_bound",
    },
    "authenticated_api_generate_submit_poll": {
        "operator_generate_authenticated",
        "submitter_submit_authenticated",
        "poll_authenticated",
        "second_principal_cache_hit",
        "active_raw_score_redacted",
        "reveal_commitment_verified",
        "providers_download_hash_verified",
    },
    "full_staging_round": {
        "same_round_baseline_verified",
        "exact_noop_patch_verified",
        "frozen_six_gate_set_verified",
        "evaluator_identity_verified",
        "host_identity_verified",
        "cleanup_verified",
        "artifact_manifest_verified",
    },
    "monitoring_and_recovery": {
        "single_job_fifo_verified",
        "lease_recovery_verified",
        "host_heartbeat_verified",
        "resource_reports_verified",
        "cleanup_after_failure_verified",
    },
    "artifact_retention": {
        "accounting_immutable",
        "resources_immutable",
        "artifact_quota_verified",
        "retain_until_verified",
        "failure_evidence_retained",
    },
    "no_secrets_audit": {
        "direct_config_local_read_only",
        "direct_vault_local_read_only",
        "no_parent_config_mount",
        "no_parent_vault_mount",
        "no_control_secret_mount",
        "no_docker_socket_mount",
        "evidence_secret_scan_passed",
        "raw_tokens_not_stored",
    },
}


class AuditError(RuntimeError):
    pass


class FinalReportParser(HTMLParser):
    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.section_visuals: list[int] = []
        self.section_depth = 0
        self.baseline_ids: set[str] = set()
        self.threshold_line_ids: set[str] = set()
        self.threshold_label_ids: set[str] = set()

    def handle_starttag(
        self, tag: str, attrs: list[tuple[str, str | None]]
    ) -> None:
        attributes = dict(attrs)
        if tag == "section":
            self.section_depth += 1
            self.section_visuals.append(0)
        elif tag == "svg" and self.section_depth == 1:
            self.section_visuals[-1] += 1
        baseline_id = attributes.get("data-baseline-id")
        if baseline_id:
            self.baseline_ids.add(baseline_id)
        threshold_for = attributes.get("data-threshold-for")
        if threshold_for and tag == "line":
            self.threshold_line_ids.add(threshold_for)
        threshold_label_for = attributes.get("data-threshold-label-for")
        if threshold_label_for:
            self.threshold_label_ids.add(threshold_label_for)

    def handle_startendtag(
        self, tag: str, attrs: list[tuple[str, str | None]]
    ) -> None:
        self.handle_starttag(tag, attrs)

    def handle_endtag(self, tag: str) -> None:
        if tag == "section":
            self.section_depth -= 1


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def load_json(path: Path) -> dict[str, Any]:
    if not path.is_file() or path.is_symlink():
        raise AuditError(f"unsafe JSON evidence: {path}")
    value = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(value, dict):
        raise AuditError(f"{path}: expected an object")
    return value


def finite_positive(value: Any, label: str) -> float:
    if not (
        isinstance(value, (int, float))
        and not isinstance(value, bool)
        and math.isfinite(value)
        and value > 0
    ):
        raise AuditError(f"invalid positive number: {label}")
    return float(value)


def security_evidence_authenticated(value: Any) -> bool:
    return bool(
        isinstance(value, dict)
        and set(value) == SECURITY_BOOLEAN_IDS | SECURITY_ID_IDS
        and all(value[key] is True for key in SECURITY_BOOLEAN_IDS)
        and all(
            isinstance(value[key], str) and bool(value[key])
            for key in SECURITY_ID_IDS
        )
    )


def authenticate_evidence_manifest(
    attempt_directory: Path, manifest: dict[str, Any]
) -> tuple[int, int]:
    artifacts = manifest.get("artifacts")
    if not isinstance(artifacts, list) or not artifacts:
        raise AuditError(f"empty evidence manifest: {attempt_directory}")
    attempt_root = attempt_directory.resolve(strict=True)
    seen: set[str] = set()
    total_bytes = 0
    for index, artifact in enumerate(artifacts):
        if not isinstance(artifact, dict):
            raise AuditError(
                f"evidence manifest entry {index} is not an object"
            )
        relative = artifact.get("path")
        expected_sha256 = artifact.get("sha256")
        expected_bytes = artifact.get("bytes")
        relative_path = Path(relative) if isinstance(relative, str) else None
        if not (
            isinstance(relative, str)
            and relative
            and "\\" not in relative
            and relative_path is not None
            and not relative_path.is_absolute()
            and all(part not in {"", ".", ".."} for part in relative_path.parts)
            and relative not in seen
            and isinstance(expected_sha256, str)
            and len(expected_sha256) == 64
            and expected_sha256
            == expected_sha256.lower()
            and all(character in "0123456789abcdef" for character in expected_sha256)
            and isinstance(expected_bytes, int)
            and not isinstance(expected_bytes, bool)
            and expected_bytes >= 0
        ):
            raise AuditError(
                f"evidence manifest entry {index} is invalid"
            )
        seen.add(relative)
        artifact_path = attempt_directory.joinpath(*relative_path.parts)
        path_cursor = attempt_directory
        symlink_component = False
        for part in relative_path.parts:
            path_cursor /= part
            symlink_component = symlink_component or path_cursor.is_symlink()
        try:
            resolved = artifact_path.resolve(strict=True)
        except OSError as exc:
            raise AuditError(
                f"manifest artifact is unavailable: {relative}: {exc}"
            ) from exc
        if not (
            resolved.is_relative_to(attempt_root)
            and not symlink_component
            and artifact_path.is_file()
            and artifact_path.stat().st_size == expected_bytes
            and sha256(artifact_path) == expected_sha256
        ):
            raise AuditError(
                f"manifest artifact is unauthenticated: {relative}"
            )
        total_bytes += expected_bytes
    return len(seen), total_bytes


def independent_seed_result_evidence(
    commitment: dict[str, Any],
) -> tuple[str, dict[str, str], dict[str, int], int, int]:
    entries = commitment.get("seeds")
    if not (
        isinstance(entries, list)
        and len(entries) == INDEPENDENT_TARGET
        and all(isinstance(entry, dict) for entry in entries)
        and [entry.get("seed_index") for entry in entries]
        == list(range(1, INDEPENDENT_TARGET + 1))
    ):
        raise AuditError("independent seed commitment order is invalid")
    commitments = {int(entry["seed_index"]): entry for entry in entries}
    digest = hashlib.sha256()
    result_hashes: dict[str, str] = {}
    placeability_counts = {
        reference: 0 for reference in ("better", "noop", "worse")
    }
    authenticated_manifest_entries = 0
    authenticated_manifest_bytes = 0
    expected_gates = {
        "G1_success",
        "G2_volume",
        "G3_path_integrity",
        "G4_matchmaking",
        "G5_stability",
        "G6_resources",
    }
    for index in range(1, INDEPENDENT_TARGET + 1):
        path = INDEPENDENT / f"seed-{index:02d}/seed-result.json"
        if path.stat().st_mode & 0o777 != 0o400:
            raise AuditError(f"independent seed result mode changed: {path}")
        result = load_json(path)
        entry = commitments[index]
        references = result.get("references")
        reference_order = result.get("reference_order")
        designated = result.get("designated_baseline")
        if not (
            result.get("schema") == 1
            and result.get("kind") == "sim-latency-independent-seed-result"
            and result.get("seed_index") == index
            and result.get("replicates_per_reference") == 1
            and result.get("round_id") == entry.get("round_id")
            and result.get("seed_commitment") == entry.get("seed_commitment")
            and result.get("providers_sha256") == entry.get("providers_sha256")
            and result.get("calibration_decision_sha256")
            == sha256(INDEPENDENT_DECISION)
            and isinstance(reference_order, list)
            and len(reference_order) == 3
            and set(reference_order) == {"better", "noop", "worse"}
            and isinstance(designated, dict)
            and designated.get("reference") == reference_order[0]
            and isinstance(references, dict)
            and set(references) == {"better", "noop", "worse"}
        ):
            raise AuditError(f"independent seed result {index} is invalid")
        ratios: dict[str, float] = {}
        baseline_raw_by_reference: dict[str, float] = {}
        for reference in ("better", "noop", "worse"):
            record = references[reference]
            if not isinstance(record, dict):
                raise AuditError(f"seed {index} {reference} result is invalid")
            relative = record.get("attempt_directory")
            parts = Path(relative).parts if isinstance(relative, str) else ()
            if not (
                len(parts) == 3
                and parts[0] == f"seed-{index:02d}"
                and parts[1] == f"reference-{reference}"
                and parts[2] in {"attempt-1", "attempt-2", "attempt-3"}
            ):
                raise AuditError(
                    f"seed {index} {reference} attempt path is unsafe"
                )
            attempt_directory = INDEPENDENT
            for part in parts:
                attempt_directory /= part
                if attempt_directory.is_symlink():
                    raise AuditError(
                        f"seed {index} {reference} attempt path is symlinked"
                    )
            if not attempt_directory.is_dir():
                raise AuditError(
                    f"seed {index} {reference} attempt directory is missing"
                )
            worker_path = attempt_directory / "worker-result.json"
            manifest_path = attempt_directory / "evidence-manifest.json"
            baseline_path = attempt_directory / "baseline.json"
            for artifact in (worker_path, manifest_path, baseline_path):
                if artifact.stat().st_mode & 0o777 != 0o400:
                    raise AuditError(f"evaluation artifact mode changed: {artifact}")
            if not (
                record.get("worker_result_sha256") == sha256(worker_path)
                and record.get("evidence_manifest_sha256")
                == sha256(manifest_path)
                and record.get("patch_sha256")
                == REFERENCE_PATCH_SHA256[reference]
            ):
                raise AuditError(
                    f"seed {index} {reference} evidence hashes are invalid"
                )
            worker = load_json(worker_path)
            manifest = load_json(manifest_path)
            baseline = load_json(baseline_path)
            score = worker.get("score")
            security = worker.get("security")
            if not (
                worker.get("schema") == 1
                and worker.get("eval_error") is None
                and isinstance(score, dict)
                and security_evidence_authenticated(security)
                and manifest.get("schema") == 1
                and manifest.get("kind") == "sim-latency-evidence-manifest"
                and manifest.get("job_id") == worker.get("job_id")
                and manifest.get("round_id") == result.get("round_id")
            ):
                raise AuditError(
                    f"seed {index} {reference} worker evidence is invalid"
                )
            manifest_entries, manifest_bytes = authenticate_evidence_manifest(
                attempt_directory, manifest
            )
            authenticated_manifest_entries += manifest_entries
            authenticated_manifest_bytes += manifest_bytes
            gates = score.get("gates")
            if not (
                isinstance(gates, dict)
                and set(gates) == expected_gates
                and all(
                    isinstance(gate, dict)
                    and isinstance(gate.get("passed"), bool)
                    for gate in gates.values()
                )
            ):
                raise AuditError(f"seed {index} {reference} gates are invalid")
            gate_passes = {
                gate_id: gate["passed"] for gate_id, gate in gates.items()
            }
            failed_gate_ids = [
                gate_id
                for gate_id, passed in gate_passes.items()
                if not passed
            ]
            placeable = score.get("placeable")
            takeover_eligible = score.get("takeover_eligible")
            if not (
                isinstance(placeable, bool)
                and isinstance(takeover_eligible, bool)
                and placeable == all(gate_passes.values())
                and record.get("placeable") is placeable
                and record.get("takeover_eligible") is takeover_eligible
                and record.get("gate_passes") == gate_passes
                and record.get("failed_gate_ids") == failed_gate_ids
            ):
                raise AuditError(
                    f"seed {index} {reference} gate projection is invalid"
                )
            baseline_replicates = baseline.get("replicates")
            if not (
                baseline.get("score_schema") == 1
                and baseline.get("kind") == "sim-latency-score-baseline"
                and baseline.get("round_id") == result.get("round_id")
                and baseline.get("config_sha256")
                == result.get("providers_sha256")
                and isinstance(baseline_replicates, list)
                and len(baseline_replicates) == 1
                and isinstance(baseline_replicates[0], dict)
            ):
                raise AuditError(
                    f"seed {index} {reference} baseline is invalid"
                )
            baseline_raw = finite_positive(
                baseline_replicates[0].get("raw_score"),
                f"seed {index} {reference} baseline raw score",
            )
            candidate_raw = finite_positive(
                score.get("raw_score"),
                f"seed {index} {reference} candidate raw score",
            )
            normalized = finite_positive(
                score.get("normalized_score"),
                f"seed {index} {reference} normalized score",
            )
            ratio = finite_positive(
                record.get("paired_ratio"),
                f"seed {index} {reference} paired ratio",
            )
            if not (
                math.isclose(
                    finite_positive(
                        record.get("baseline_raw_score_ms"),
                        f"seed {index} {reference} recorded baseline",
                    ),
                    baseline_raw,
                    rel_tol=1e-12,
                )
                and math.isclose(
                    finite_positive(
                        record.get("candidate_raw_score_ms"),
                        f"seed {index} {reference} recorded candidate",
                    ),
                    candidate_raw,
                    rel_tol=1e-12,
                )
                and math.isclose(
                    finite_positive(
                        record.get("normalized_score"),
                        f"seed {index} {reference} recorded normalized",
                    ),
                    normalized,
                    rel_tol=1e-12,
                )
                and math.isclose(
                    ratio, candidate_raw / baseline_raw, rel_tol=1e-12
                )
            ):
                raise AuditError(
                    f"seed {index} {reference} score projection is invalid"
                )
            ratios[reference] = candidate_raw
            baseline_raw_by_reference[reference] = baseline_raw
            placeability_counts[reference] += int(placeable)
        designated_reference = designated["reference"]
        if not math.isclose(
            finite_positive(
                designated.get("raw_score_ms"),
                f"seed {index} designated baseline",
            ),
            baseline_raw_by_reference[designated_reference],
            rel_tol=1e-12,
        ):
            raise AuditError(f"seed {index} designated baseline is invalid")
        designated_raw = baseline_raw_by_reference[designated_reference]
        common_baseline_ratios = {
            reference: candidate_raw / designated_raw
            for reference, candidate_raw in ratios.items()
        }
        if not (
            result.get("ordering_metric") == ORDERING_METRIC
            and result.get("ordering_passed")
            == (
                common_baseline_ratios["better"]
                < common_baseline_ratios["noop"]
                < common_baseline_ratios["worse"]
            )
        ):
            raise AuditError(f"seed {index} ordering projection is invalid")
        result_sha256 = sha256(path)
        result_hashes[f"{index:02d}"] = result_sha256
        relative_path = path.relative_to(HISTORICAL_ROOT).as_posix()
        digest.update(
            f"{index:02d}\t{relative_path}\t{result_sha256}\n".encode(
                "utf-8"
            )
        )
    return (
        digest.hexdigest(),
        result_hashes,
        placeability_counts,
        authenticated_manifest_entries,
        authenticated_manifest_bytes,
    )


def independent_reveal_public_projection(
    reveal: dict[str, Any],
) -> list[dict[str, Any]]:
    seeds = reveal.get("seeds")
    if not (
        isinstance(seeds, list)
        and len(seeds) == INDEPENDENT_TARGET
        and all(isinstance(seed, dict) for seed in seeds)
        and [seed.get("seed_index") for seed in seeds]
        == list(range(1, INDEPENDENT_TARGET + 1))
    ):
        raise AuditError("independent seed reveal order is invalid")
    projection: list[dict[str, Any]] = []
    expected_keys = {
        "seed_index",
        "round_id",
        "round_seed_hex",
        "seed_commitment",
        "generator_seed",
        "providers_sha256",
    }
    for seed in seeds:
        if set(seed) != expected_keys:
            raise AuditError("independent seed reveal fields are invalid")
        try:
            round_id = uuid.UUID(seed["round_id"])
            seed_bytes = bytes.fromhex(seed["round_seed_hex"])
        except (AttributeError, TypeError, ValueError) as exc:
            raise AuditError("independent seed reveal material is malformed") from exc
        if str(round_id) != seed["round_id"] or len(seed_bytes) != 32:
            raise AuditError("independent seed reveal material is noncanonical")
        commitment = hashlib.sha256(
            b"urnetwork-sim-latency-round-v1\0" + round_id.bytes + seed_bytes
        ).hexdigest()
        generator_digest = hashlib.sha256(
            b"urnetwork-sim-latency-generator-v1\0" + seed_bytes
        ).digest()
        generator_seed = int.from_bytes(generator_digest[:8], "big") & (
            (1 << 63) - 1
        )
        if generator_seed == 0:
            generator_seed = 1
        providers_sha256 = seed.get("providers_sha256")
        if not (
            seed.get("seed_commitment") == commitment
            and seed.get("generator_seed") == generator_seed
            and isinstance(providers_sha256, str)
            and len(providers_sha256) == 64
            and all(character in "0123456789abcdef" for character in providers_sha256)
        ):
            raise AuditError("independent seed reveal derivation is invalid")
        projection.append(
            {
                "seed_index": seed["seed_index"],
                "round_id": seed["round_id"],
                "seed_commitment": seed["seed_commitment"],
                "providers_sha256": providers_sha256,
            }
        )
    return projection


def load_production_check(
    readiness: dict[str, Any], check_id: str
) -> dict[str, Any]:
    checks = readiness.get("checks")
    record = checks.get(check_id) if isinstance(checks, dict) else None
    if not isinstance(record, dict) or record.get("passed") is not True:
        raise AuditError(f"production check {check_id!r} is not a passed record")
    relative = record.get("evidence_path")
    expected_sha = record.get("evidence_sha256")
    if not isinstance(relative, str) or not relative:
        raise AuditError(f"production check {check_id!r} has no evidence path")
    if not isinstance(expected_sha, str) or len(expected_sha) != 64:
        raise AuditError(f"production check {check_id!r} has no evidence hash")
    path = ROOT / relative
    try:
        resolved = path.resolve(strict=True)
    except OSError as exc:
        raise AuditError(
            f"production check {check_id!r} evidence is unavailable: {exc}"
        ) from exc
    evidence_root = (ROOT / "production-readiness").resolve()
    if not resolved.is_relative_to(evidence_root):
        raise AuditError(f"production check {check_id!r} evidence escaped its root")
    if path.is_symlink() or not path.is_file():
        raise AuditError(f"production check {check_id!r} evidence is unsafe")
    if path.stat().st_mode & 0o022:
        raise AuditError(f"production check {check_id!r} evidence is writable")
    if sha256(path) != expected_sha:
        raise AuditError(f"production check {check_id!r} evidence hash changed")
    evidence = load_json(path)
    assertions = evidence.get("assertions")
    required_assertions = PRODUCTION_ASSERTIONS[check_id]
    if not (
        evidence.get("schema") == 1
        and evidence.get("kind") == "sim-latency-production-readiness-check"
        and evidence.get("check_id") == check_id
        and evidence.get("passed") is True
        and evidence.get("source_lock_sha256") == SOURCE_LOCK_SHA256
        and evidence.get("production_staging_protocol_sha256")
        == PRODUCTION_PROTOCOL_SHA256
        and evidence.get("control_plane_commit") == CONTROL_SOURCE_COMMIT
        and isinstance(assertions, dict)
        and set(assertions) == required_assertions
        and all(value is True for value in assertions.values())
    ):
        raise AuditError(
            f"production check {check_id!r} content is incomplete or unbound"
        )
    if evidence.get(
        "production_staging_attempt_06_remediation_amendment_sha256"
    ) != readiness.get(
        "production_staging_attempt_06_remediation_amendment_sha256"
    ):
        raise AuditError(
            f"production check {check_id!r} is not bound to the attempt-06 remediation"
        )
    if check_id not in {
        "release_artifacts",
        "service_backed_fifo_cache_failover",
    } and evidence.get("production_staging_reference_v5_amendment_sha256") != (
        readiness.get("production_staging_reference_v5_amendment_sha256")
    ):
        raise AuditError(
            f"production check {check_id!r} is not bound to the v5 staging amendment"
        )
    if check_id not in {
        "release_artifacts",
        "service_backed_fifo_cache_failover",
    } and evidence.get(
        "production_release_self_check_contract_amendment_sha256"
    ) != readiness.get(
        "production_release_self_check_contract_amendment_sha256"
    ):
        raise AuditError(
            f"production check {check_id!r} is not bound to the release self-check amendment"
        )
    return {
        "evidence_path": relative,
        "evidence_sha256": expected_sha,
        "assertion_count": len(assertions),
    }


def command(args: list[str], cwd: Path) -> str:
    result = subprocess.run(
        args,
        cwd=cwd,
        check=True,
        capture_output=True,
        text=True,
    )
    return result.stdout.strip()


def check(
    checks: list[dict[str, Any]],
    check_id: str,
    state: str,
    summary: str,
    evidence: dict[str, Any] | None = None,
    *,
    launch_required: bool = True,
) -> None:
    if state not in {"pass", "pending", "fail"}:
        raise AuditError(f"invalid audit state: {state}")
    checks.append(
        {
            "id": check_id,
            "state": state,
            "launch_required": launch_required,
            "summary": summary,
            "evidence": evidence or {},
        }
    )


def audit_source(checks: list[dict[str, Any]]) -> None:
    try:
        lock = load_json(SOURCE_LOCK)
        if sha256(SOURCE_LOCK) != SOURCE_LOCK_SHA256:
            raise AuditError("source-lock digest changed")
        repositories = lock.get("repositories")
        if not isinstance(repositories, dict):
            raise AuditError("source-lock repositories are missing")
        remediation = load_json(REMEDIATION_AMENDMENT)
        season_base = load_json(SEASON_BASE_EQUIVALENCE)
        policy = load_json(PATCH_POLICY)
        authoring = season_base.get("public_authoring_base")
        evaluator = season_base.get("authoritative_evaluator")
        policy_evidence = season_base.get("patch_policy")
        editable_blobs = season_base.get("editable_blobs")
        editable_path = "connect/resident_contract_manager.go"
        editable = (
            editable_blobs.get(editable_path)
            if isinstance(editable_blobs, dict)
            else None
        )
        if not (
            sha256(SEASON_BASE_EQUIVALENCE)
            == SEASON_BASE_EQUIVALENCE_SHA256
            and repositories.get("server") == SERVER_COMMIT
            and lock.get("evaluator", {}).get("image_id") == EVALUATOR_IMAGE
            and lock.get("host", {}).get("qualification_sha256")
            == HOST_QUALIFICATION_SHA256
            and sha256(REMEDIATION_AMENDMENT) == REMEDIATION_AMENDMENT_SHA256
            and REMEDIATION_AMENDMENT.stat().st_mode & 0o777 == 0o400
            and remediation.get("authorized") is True
            and remediation.get("historical_calibration", {}).get(
                "source_lock_sha256"
            )
            == HISTORICAL_SOURCE_LOCK_SHA256
            and remediation.get("replacement", {}).get("source_lock_sha256")
            == SOURCE_LOCK_SHA256
            and remediation.get("replacement", {}).get("evaluator_image_digest")
            == EVALUATOR_IMAGE
            and SEASON_BASE_EQUIVALENCE.stat().st_mode & 0o777 == 0o400
            and season_base.get("kind")
            == "sim-latency-season-base-equivalence"
            and isinstance(authoring, dict)
            and authoring.get("tag") == "apex-season-1"
            and authoring.get("remote_tag_matches_local") is True
            and command(
                ["git", "rev-parse", "apex-season-1^{tag}"], SERVER
            )
            == authoring.get("annotated_tag_object")
            and command(["git", "rev-parse", "apex-season-1^{}"], SERVER)
            == authoring.get("commit")
            and isinstance(evaluator, dict)
            and evaluator.get("commit") == HISTORICAL_SERVER_COMMIT
            and evaluator.get("source_lock_sha256")
            == HISTORICAL_SOURCE_LOCK_SHA256
            and evaluator.get("image_digest") == HISTORICAL_EVALUATOR_IMAGE
            and sha256(PATCH_POLICY) == PATCH_POLICY_SHA256
            and policy.get("allowed_paths") == [editable_path]
            and policy.get("max_patch_bytes") == 262144
            and isinstance(policy_evidence, dict)
            and policy_evidence.get("sha256") == PATCH_POLICY_SHA256
            and policy_evidence.get("allowed_paths") == [editable_path]
            and policy_evidence.get("max_patch_bytes") == 262144
            and isinstance(editable, dict)
            and editable.get("identical") is True
            and editable.get("public_authoring_base_blob")
            == command(
                [
                    "git",
                    "rev-parse",
                    f"{authoring.get('commit')}:{editable_path}",
                ],
                SERVER,
            )
            and editable.get("authoritative_evaluator_blob")
            == command(
                [
                    "git",
                    "rev-parse",
                    f"{evaluator.get('commit')}:{editable_path}",
                ],
                SERVER,
            )
            and editable.get("authoritative_evaluator_blob")
            == command(
                ["git", "rev-parse", f"{SERVER_COMMIT}:{editable_path}"],
                SERVER,
            )
            and editable.get("public_authoring_base_blob")
            == editable.get("authoritative_evaluator_blob")
            and season_base.get("public_base_is_ancestor_of_evaluator") is True
            and season_base.get("all_allowed_path_blobs_identical") is True
            and season_base.get("local_reproduction_uses_evaluator_image") is True
            and season_base.get("force_move_published_tag_required") is False
            and season_base.get("seed_material_included") is False
        ):
            raise AuditError("public authoring base is not evaluator-equivalent")
        result_document_overlay: dict[str, str] | None = None
        for name, expected in repositories.items():
            repository = URNETWORK / str(name)
            if command(["git", "rev-parse", "HEAD"], repository) != expected:
                raise AuditError(f"{name}: HEAD changed")
            status = command(
                ["git", "status", "--porcelain", "--untracked-files=no"],
                repository,
            )
            if not status:
                continue
            allowed_status = "M connect/sim-latency/APEX-CALIBRATION.md"
            if name != "server" or status != allowed_status:
                raise AuditError(f"{name}: tracked worktree is dirty")
            if not FINAL_REPORT_EVIDENCE.exists():
                raise AuditError("terminal calibration document is not hash-bound")
            report_evidence = load_json(FINAL_REPORT_EVIDENCE)
            if not (
                CALIBRATION_MD.is_file()
                and not CALIBRATION_MD.is_symlink()
                and CALIBRATION_MD.stat().st_mode & 0o777 == 0o444
                and report_evidence.get("kind")
                == "sim-latency-finalize-report-evidence"
                and report_evidence.get("source_lock_sha256")
                == SOURCE_LOCK_SHA256
                and report_evidence.get(
                    "historical_calibration_source_lock_sha256"
                )
                == HISTORICAL_SOURCE_LOCK_SHA256
                and report_evidence.get(
                    "production_staging_attempt_06_remediation_amendment_sha256"
                )
                == REMEDIATION_AMENDMENT_SHA256
                and report_evidence.get("calibration_document_sha256")
                == sha256(CALIBRATION_MD)
            ):
                raise AuditError(
                    "terminal calibration document overlay is not immutable and authenticated"
                )
            result_document_overlay = {
                "path": "connect/sim-latency/APEX-CALIBRATION.md",
                "sha256": sha256(CALIBRATION_MD),
            }
        check(
            checks,
            "source_identity",
            "pass",
            "All nine repositories match the frozen commits; the public authoring tag is blob-identical across the entire one-file patch surface, and the sole allowed tracked overlay is the immutable terminal calibration result.",
            {
                "source_lock_sha256": SOURCE_LOCK_SHA256,
                "server_commit": repositories.get("server"),
                "repository_count": len(repositories),
                "evaluator_image_digest": lock.get("evaluator", {}).get("image_id"),
                "public_authoring_tag": authoring.get("tag"),
                "public_authoring_commit": authoring.get("commit"),
                "editable_blob": editable.get("authoritative_evaluator_blob"),
                "season_base_equivalence_sha256": (
                    SEASON_BASE_EQUIVALENCE_SHA256
                ),
                "result_document_overlay": result_document_overlay,
            },
        )
    except (AuditError, OSError, subprocess.SubprocessError, ValueError) as exc:
        check(checks, "source_identity", "fail", str(exc))


def audit_frontier(checks: list[dict[str, Any]]) -> None:
    try:
        frontier = load_json(FRONTIER)
        point = load_json(POINT)
        selected = frontier.get("selected")
        rejected = frontier.get("rejected_upper_bound")
        if not (
            frontier.get("accepted") is True
            and frontier.get("source_lock_sha256")
            == HISTORICAL_SOURCE_LOCK_SHA256
            and frontier.get("selected_point_id") == "p1800-c200-r80-q2"
            and isinstance(selected, dict)
            and selected.get("provider_count") == 1800
            and selected.get("client_pool_size") == 200
            and selected.get("arrivals_per_minute") == 80
            and isinstance(rejected, dict)
            and rejected.get("point_id") == "p2700-c300-r120-q2"
            and rejected.get("minimum_success_rate", 1) < 0.97
            and point.get("accepted") is True
            and point.get("source_lock_sha256")
            == HISTORICAL_SOURCE_LOCK_SHA256
            and point.get("impairment_modes_completed") is True
        ):
            raise AuditError("exact-image frontier decision is not the frozen p1800 result")
        check(
            checks,
            "frontier_and_scale",
            "pass",
            "The exact-image impairment on/off frontier selected p1800 and rejected p2700.",
            {
                "frontier_sha256": sha256(FRONTIER),
                "point_summary_sha256": sha256(POINT),
                "provider_count": 1800,
                "client_pool_size": 200,
                "arrivals_per_minute": 80,
                "minimum_accepted_success_rate": point.get("minimum_success_rate"),
                "rejected_upper_bound_success_rate": rejected.get(
                    "minimum_success_rate"
                ),
            },
        )
    except (AuditError, OSError, ValueError) as exc:
        check(checks, "frontier_and_scale", "fail", str(exc))


def audit_same_seed(checks: list[dict[str, Any]]) -> None:
    try:
        progress = load_json(SAME_PROGRESS)
    except (OSError, ValueError, AuditError) as exc:
        check(checks, "same_seed_calibration", "fail", str(exc))
        return
    completed = progress.get("completed_pairs")
    results = progress.get("results")
    if progress.get("complete") is not True:
        valid_partial = (
            isinstance(completed, int)
            and 0 <= completed < SAME_SEED_TARGET
            and progress.get("target_pairs") == SAME_SEED_TARGET
            and isinstance(results, list)
            and len(results) == completed
        )
        check(
            checks,
            "same_seed_calibration",
            "pending" if valid_partial else "fail",
            f"Authenticated same-seed progress is {completed}/{SAME_SEED_TARGET}.",
            {"progress_sha256": sha256(SAME_PROGRESS), "completed_pairs": completed},
        )
        return
    try:
        if not (
            completed == SAME_SEED_TARGET
            and progress.get("target_pairs") == SAME_SEED_TARGET
            and isinstance(results, list)
            and len(results) == SAME_SEED_TARGET
        ):
            raise AuditError("complete same-seed progress has the wrong cardinality")
        analysis = load_json(SAME_ANALYSIS)
        strict_analysis = load_json(STRICT_SAME_ANALYSIS)
        pre_repair_progress = load_json(PRE_REPAIR_PROGRESS)
        policy = load_json(PLACEABILITY_POLICY)
        repair = load_json(POSTPROCESSING_REPAIR)
        decision = load_json(SAME_DECISION)
        selection = load_json(SELECTION)
        attestation = load_json(SELECTION_ATTESTATION)
        baseline = analysis.get("baseline_raw_score_ms")
        selected_r = decision.get("selected_replicates")
        margin = decision.get("takeover_margin")
        threshold = decision.get("baseline_mean_significantly_better_threshold_ms")
        options = analysis.get("aggregation_options")
        eligible = (
            [option for option in options if option.get("selection_eligible") is True]
            if isinstance(options, list)
            else []
        )
        if not (
            sha256(PLACEABILITY_POLICY) == PLACEABILITY_POLICY_SHA256
            and PLACEABILITY_POLICY.stat().st_mode & 0o777 == 0o400
            and sha256(POSTPROCESSING_REPAIR) == POSTPROCESSING_REPAIR_SHA256
            and POSTPROCESSING_REPAIR.stat().st_mode & 0o777 == 0o400
            and sha256(STRICT_SAME_ANALYSIS) == STRICT_SAME_ANALYSIS_SHA256
            and sha256(PRE_REPAIR_PROGRESS) == PRE_REPAIR_PROGRESS_SHA256
            and strict_analysis.get("decision_ready") is False
            and strict_analysis.get("progress_sha256")
            == PRE_REPAIR_PROGRESS_SHA256
            and pre_repair_progress.get("complete") is True
            and policy.get("authorized") is True
            and policy.get("strict_same_seed_analysis_sha256")
            == STRICT_SAME_ANALYSIS_SHA256
            and policy.get("launch_policy", {}).get("minimum_probability") == 0.94
            and policy.get("launch_policy", {}).get("selected_replicates") == 9
            and policy.get("independent_reference_gate_unchanged", {}).get(
                "required_ordering_passes"
            )
            == PRIOR_REQUIRED_PASSES
            and repair.get("passed") is True
            and repair.get("measurements_rerun") is False
            and repair.get("measurements_censored") is False
            and repair.get("strict_familywise_analysis_sha256")
            == STRICT_SAME_ANALYSIS_SHA256
            and repair.get("launch_analysis_sha256") == sha256(SAME_ANALYSIS)
            and repair.get("calibration_selection_sha256") == sha256(SELECTION)
            and analysis.get("kind")
            == "sim-latency-launch-compromise-same-seed-analysis"
            and analysis.get("decision_ready") is True
            and analysis.get("replicate_count") == SAME_SEED_TARGET
            and analysis.get("progress_sha256") == sha256(SAME_PROGRESS)
            and analysis.get("measurement_amendment_sha256") == AMENDMENT_SHA256
            and analysis.get("placeability_policy_amendment_sha256")
            == PLACEABILITY_POLICY_SHA256
            and analysis.get("strict_familywise_analysis_sha256")
            == STRICT_SAME_ANALYSIS_SHA256
            and len(eligible) == 1
            and eligible[0].get("replicates") == 9
            and math.isclose(
                eligible[0].get("noop_placeability_pass_rate"),
                0.94614,
                rel_tol=1e-12,
            )
            and decision.get("accepted") is True
            and decision.get("same_seed_analysis_sha256") == sha256(SAME_ANALYSIS)
            and selected_r == 9
            and isinstance(margin, (int, float))
            and margin == 0.161
            and isinstance(threshold, (int, float))
            and math.isfinite(threshold)
            and isinstance(baseline, dict)
            and isinstance(baseline.get("mean"), (int, float))
            and math.isclose(
                threshold,
                baseline["mean"] * (1 - margin),
                rel_tol=1e-12,
                abs_tol=1e-9,
            )
            and selection.get("accepted") is True
            and selection.get("calibration_decision_sha256") == sha256(SAME_DECISION)
            and attestation.get("accepted") is True
            and attestation.get("calibration_selection_sha256") == sha256(SELECTION)
        ):
            raise AuditError("same-seed analysis or selection chain failed authentication")
        check(
            checks,
            "same_seed_calibration",
            "pass",
            "Twelve uncensored pairs retained the strict familywise failure and selected R=9 under the authorized single-evaluation placeability rule.",
            {
                "progress_sha256": sha256(SAME_PROGRESS),
                "analysis_sha256": sha256(SAME_ANALYSIS),
                "decision_sha256": sha256(SAME_DECISION),
                "selection_sha256": sha256(SELECTION),
                "strict_analysis_sha256": STRICT_SAME_ANALYSIS_SHA256,
                "placeability_policy_sha256": PLACEABILITY_POLICY_SHA256,
                "postprocessing_repair_sha256": POSTPROCESSING_REPAIR_SHA256,
                "baseline_mean_ms": baseline["mean"],
                "baseline_cv": baseline.get("cv"),
                "r9_noop_placeability_probability": eligible[0].get(
                    "noop_placeability_pass_rate"
                ),
                "selected_replicates": selected_r,
                "takeover_margin": margin,
                "significantly_better_threshold_ms": threshold,
            },
        )
    except (AuditError, OSError, ValueError, TypeError) as exc:
        check(checks, "same_seed_calibration", "fail", str(exc))


def audit_independent_protocol(checks: list[dict[str, Any]]) -> None:
    try:
        amendment = load_json(AMENDMENT)
        placeability_policy = load_json(PLACEABILITY_POLICY)
        repair = load_json(POSTPROCESSING_REPAIR)
        correction = load_json(R1_CORRECTION)
        protocol = load_json(R1_PROTOCOL)
        launch = load_json(R1_LAUNCH)
        design = load_json(REFERENCE_V5_DESIGN)
        static_qualification = load_json(REFERENCE_V5_STATIC_QUALIFICATION)
        retired = load_json(REFERENCE_V5_RETIRED_COMMITMENTS)
        pre_pilot_retired = load_json(REFERENCE_V5_PRE_PILOT_RETIRED_COMMITMENTS)
        v4_rejection = load_json(REFERENCE_V4_REJECTION)
        if not (
            sha256(AMENDMENT) == AMENDMENT_SHA256
            and amendment.get("authorized") is True
            and amendment.get("same_seed_target") == SAME_SEED_TARGET
            and amendment.get("independent_seed_target")
            == PRIOR_INDEPENDENT_TARGET
            and amendment.get("reference_required_passes")
            == PRIOR_REQUIRED_PASSES
            and sha256(PLACEABILITY_POLICY) == PLACEABILITY_POLICY_SHA256
            and placeability_policy.get("authorized") is True
            and placeability_policy.get("launch_policy", {}).get(
                "selected_replicates"
            )
            == 9
            and placeability_policy.get("launch_policy", {}).get(
                "minimum_probability"
            )
            == 0.94
            and sha256(POSTPROCESSING_REPAIR) == POSTPROCESSING_REPAIR_SHA256
            and repair.get("passed") is True
            and repair.get("measurements_rerun") is False
            and repair.get("measurements_censored") is False
            and sha256(R1_CORRECTION) == R1_CORRECTION_SHA256
            and correction.get("authorized") is True
            and correction.get("correction", {}).get(
                "independent_reference_replicates_after"
            )
            == 1
            and correction.get("safety", {}).get("fresh_hidden_seed_material_created")
            is False
            and sha256(R1_PROTOCOL) == R1_PROTOCOL_SHA256
            and protocol.get("independent_reference_replicates") == 1
            and protocol.get("target_independent_seeds")
            == PRIOR_INDEPENDENT_TARGET
            and protocol.get("reference_required_passes")
            == PRIOR_REQUIRED_PASSES
            and protocol.get("all_seeds_precommitted_before_first_result") is True
            and launch.get("independent_reference_replicates") == 1
            and launch.get("fresh_hidden_seed_material_created") is False
            and sha256(REFERENCE_V5_DESIGN) == REFERENCE_V5_DESIGN_SHA256
            and REFERENCE_V5_DESIGN.stat().st_mode & 0o777 == 0o400
            and design.get("kind") == "sim-latency-reference-v5-design"
            and design.get("draft") is False
            and design.get("authorized") is True
            and design.get("ordering_metric") == ORDERING_METRIC
            and design.get("v4_hidden_campaign_rejection_sha256")
            == REFERENCE_V4_REJECTION_SHA256
            and sha256(REFERENCE_V5_STATIC_QUALIFICATION)
            == REFERENCE_V5_STATIC_QUALIFICATION_SHA256
            and static_qualification.get("accepted_for_full_scale_evaluator_pilot")
            is True
            and static_qualification.get("official_reference_set_accepted")
            is False
            and sha256(REFERENCE_V5_RETIRED_COMMITMENTS)
            == REFERENCE_V5_RETIRED_COMMITMENTS_SHA256
            and sha256(REFERENCE_V5_PRE_PILOT_RETIRED_COMMITMENTS)
            == REFERENCE_V5_PRE_PILOT_RETIRED_COMMITMENTS_SHA256
            and pre_pilot_retired.get("commitment_count") == 21
            and isinstance(pre_pilot_retired.get("commitments"), list)
            and len(pre_pilot_retired["commitments"])
            == len(set(pre_pilot_retired["commitments"]))
            == 21
            and retired.get("commitment_count") == 22
            and isinstance(retired.get("commitments"), list)
            and len(retired["commitments"]) == len(set(retired["commitments"])) == 22
            and retired.get("seed_material_included") is False
            and sha256(REFERENCE_V4_REJECTION) == REFERENCE_V4_REJECTION_SHA256
            and v4_rejection.get("accepted") is False
            and v4_rejection.get("completed_seeds") == 5
            and v4_rejection.get("all_precommitted_measurements_retained") is True
        ):
            raise AuditError("historical or reference-v5 protocol lineage changed")

        if not REFERENCE_V5_PILOT_DECISION.exists():
            check(
                checks,
                "measurement_protocol",
                "pending",
                "The authorized v5 design is frozen; its fresh pilot is still running.",
                {
                    "design_sha256": REFERENCE_V5_DESIGN_SHA256,
                    "target_independent_seeds": INDEPENDENT_TARGET,
                    "reference_required_passes": INDEPENDENT_REQUIRED_PASSES,
                },
            )
            return

        pilot = load_json(REFERENCE_V5_PILOT_DECISION)
        qualification = load_json(REFERENCE_V5_QUALIFICATION)
        if qualification.get("draft") is True:
            check(
                checks,
                "measurement_protocol",
                "pending",
                "The v5 pilot has not yet been promoted into an immutable hidden-campaign protocol.",
                {
                    "pilot_decision_sha256": sha256(REFERENCE_V5_PILOT_DECISION),
                    "target_independent_seeds": INDEPENDENT_TARGET,
                    "reference_required_passes": INDEPENDENT_REQUIRED_PASSES,
                },
            )
            return

        hidden_amendment = load_json(INDEPENDENT_MEASUREMENT_AMENDMENT)
        hidden_protocol = load_json(INDEPENDENT_PROTOCOL)
        pilot_reveal = load_json(REFERENCE_V5_PILOT_REVEAL)
        qualification_checks = qualification.get("checks")
        expected_patches = REFERENCE_PATCH_SHA256
        if not (
            REFERENCE_V5_PILOT_DECISION.stat().st_mode & 0o777 == 0o400
            and REFERENCE_V5_PILOT_REVEAL.stat().st_mode & 0o777 == 0o400
            and REFERENCE_V5_QUALIFICATION.stat().st_mode & 0o777 == 0o400
            and INDEPENDENT_MEASUREMENT_AMENDMENT.stat().st_mode & 0o777 == 0o400
            and INDEPENDENT_PROTOCOL.stat().st_mode & 0o777 == 0o400
            and REFERENCE_V5_RUNNER.stat().st_mode & 0o777 == 0o500
            and pilot.get("kind") == "sim-latency-reference-v5-pilot-decision"
            and pilot.get("accepted") is True
            and pilot.get("strict_ordering_passed") is True
            and pilot.get("ordering_metric") == ORDERING_METRIC
            and pilot.get("cleanup", {}).get("residual_competition_containers") == 0
            and pilot.get("cleanup", {}).get("residual_competition_networks") == 0
            and pilot.get("campaign_commitment_sha256")
            == sha256(
                REFERENCE_V5
                / "pilot-runtime/independent-references/campaign-commitment.json"
            )
            and pilot.get("seed_reveal_sha256")
            == sha256(REFERENCE_V5_PILOT_REVEAL)
            and pilot_reveal.get("seed_commitment")
            not in pre_pilot_retired.get("commitments", [])
            and sorted(retired.get("commitments", []))
            == sorted(
                pre_pilot_retired.get("commitments", [])
                + [pilot_reveal.get("seed_commitment")]
            )
            and qualification.get("kind")
            == "sim-latency-reference-v5-pilot-qualification"
            and qualification.get("accepted_for_hidden_five_seed_screen") is True
            and qualification.get("official_reference_set_accepted") is False
            and qualification.get("pilot_decision_sha256")
            == sha256(REFERENCE_V5_PILOT_DECISION)
            and qualification.get("pilot_seed_reveal_sha256")
            == sha256(REFERENCE_V5_PILOT_REVEAL)
            and qualification.get("strict_ordering_passed") is True
            and qualification.get("pilot_seed_reused") is False
            and isinstance(qualification_checks, list)
            and len(qualification_checks) == 8
            and all(item.get("passed") is True for item in qualification_checks)
            and hidden_amendment.get("kind")
            == "sim-latency-reference-v5-hidden-launch-measurement-amendment"
            and hidden_amendment.get("draft") is False
            and hidden_amendment.get("authorized") is True
            and hidden_amendment.get("same_seed_target") == SAME_SEED_TARGET
            and hidden_amendment.get("target_independent_seeds")
            == INDEPENDENT_TARGET
            and hidden_amendment.get("reference_required_passes")
            == INDEPENDENT_REQUIRED_PASSES
            and hidden_amendment.get("pilot_decision_sha256")
            == sha256(REFERENCE_V5_PILOT_DECISION)
            and hidden_amendment.get("qualification_sha256")
            == sha256(REFERENCE_V5_QUALIFICATION)
            and hidden_amendment.get("retired_seed_commitments_sha256")
            == REFERENCE_V5_RETIRED_COMMITMENTS_SHA256
            and hidden_amendment.get("ordering_metric") == ORDERING_METRIC
            and hidden_amendment.get("patch_sha256") == expected_patches
            and hidden_protocol.get("kind")
            == "sim-latency-reference-v5-hidden-launch-protocol"
            and hidden_protocol.get("draft") is False
            and hidden_protocol.get("authorized") is True
            and hidden_protocol.get("target_independent_seeds")
            == INDEPENDENT_TARGET
            and hidden_protocol.get("reference_required_passes")
            == INDEPENDENT_REQUIRED_PASSES
            and hidden_protocol.get("all_seeds_precommitted_before_first_result")
            is True
            and hidden_protocol.get("measurement_amendment_sha256")
            == sha256(INDEPENDENT_MEASUREMENT_AMENDMENT)
            and hidden_protocol.get("qualification_sha256")
            == sha256(REFERENCE_V5_QUALIFICATION)
            and hidden_protocol.get("retired_seed_commitments_sha256")
            == REFERENCE_V5_RETIRED_COMMITMENTS_SHA256
            and hidden_protocol.get("runner_sha256") == sha256(REFERENCE_V5_RUNNER)
            and hidden_protocol.get("ordering_metric") == ORDERING_METRIC
            and hidden_protocol.get("patch_sha256") == expected_patches
        ):
            raise AuditError("reference-v5 pilot or hidden protocol is unauthenticated")
        check(
            checks,
            "measurement_protocol",
            "pass",
            "The 12-run baseline and authorized v5 five-seed/four-pass R=1 screen are hash-bound, with placeability kept diagnostic-only for reference controls.",
            {
                "measurement_amendment_sha256": AMENDMENT_SHA256,
                "placeability_policy_sha256": PLACEABILITY_POLICY_SHA256,
                "postprocessing_repair_sha256": POSTPROCESSING_REPAIR_SHA256,
                "r1_correction_sha256": R1_CORRECTION_SHA256,
                "r1_protocol_sha256": R1_PROTOCOL_SHA256,
                "reference_v5_design_sha256": REFERENCE_V5_DESIGN_SHA256,
                "reference_v5_qualification_sha256": sha256(
                    REFERENCE_V5_QUALIFICATION
                ),
                "reference_v5_measurement_amendment_sha256": sha256(
                    INDEPENDENT_MEASUREMENT_AMENDMENT
                ),
                "reference_v5_protocol_sha256": sha256(INDEPENDENT_PROTOCOL),
                "target_independent_seeds": INDEPENDENT_TARGET,
                "reference_required_passes": INDEPENDENT_REQUIRED_PASSES,
                "reference_evaluation_jobs": 15,
            },
        )
    except (AuditError, OSError, ValueError) as exc:
        check(checks, "measurement_protocol", "fail", str(exc))


def audit_independent_results(checks: list[dict[str, Any]]) -> None:
    if not INDEPENDENT_PROGRESS.exists():
        check(
            checks,
            "independent_reference_separability",
            "pending",
            "The five hidden v5 seeds are correctly absent until pilot promotion completes.",
            {
                "completed_independent_seeds": 0,
                "target_independent_seeds": INDEPENDENT_TARGET,
            },
        )
        return
    try:
        progress = load_json(INDEPENDENT_PROGRESS)
        completed = progress.get("completed_independent_seeds")
        if progress.get("complete") is not True:
            valid_partial = (
                isinstance(completed, int)
                and 0 <= completed < INDEPENDENT_TARGET
                and progress.get("target_independent_seeds") == INDEPENDENT_TARGET
                and progress.get("replicates_per_reference") == 1
            )
            check(
                checks,
                "independent_reference_separability",
                "pending" if valid_partial else "fail",
                f"Independent reference progress is {completed}/{INDEPENDENT_TARGET} seeds.",
                {
                    "progress_sha256": sha256(INDEPENDENT_PROGRESS),
                    "completed_independent_seeds": completed,
                    "reference_ordering_passes": progress.get(
                        "reference_ordering_passes"
                    ),
                },
            )
            return
        commitment = load_json(INDEPENDENT_COMMITMENT)
        reveal = load_json(INDEPENDENT_REVEAL)
        attestation = load_json(INDEPENDENT_ATTESTATION)
        independent_decision = load_json(INDEPENDENT_DECISION)
        independent_analysis = load_json(INDEPENDENT_ANALYSIS)
        terminal_decision = load_json(INDEPENDENT_TERMINAL_DECISION)
        terminal_reveal = load_json(INDEPENDENT_TERMINAL_REVEAL)
        attestation_repair = load_json(INDEPENDENT_ATTESTATION_REPAIR)
        reveal_projection = independent_reveal_public_projection(reveal)
        repair_service_state = attestation_repair.get("original_service_state")
        repair_invalid_attempt = attestation_repair.get("invalid_first_repair")
        if not (
            completed == INDEPENDENT_TARGET
            and progress.get("target_independent_seeds") == INDEPENDENT_TARGET
            and progress.get("replicates_per_reference") == 1
            and progress.get("reference_ordering_passes", 0)
            >= INDEPENDENT_REQUIRED_PASSES
            and progress.get("separability_passed") is True
            and commitment.get("target_independent_seeds") == INDEPENDENT_TARGET
            and commitment.get("independent_reference_replicates") == 1
            and isinstance(commitment.get("seeds"), list)
            and len(commitment["seeds"]) == INDEPENDENT_TARGET
            and commitment.get("calibration_decision_sha256")
            == sha256(INDEPENDENT_DECISION)
            and commitment.get("measurement_amendment_sha256")
            == sha256(INDEPENDENT_MEASUREMENT_AMENDMENT)
            and commitment.get("independent_reference_r1_correction_sha256")
            == R1_CORRECTION_SHA256
            and independent_decision.get("decision_ready") is True
            and independent_decision.get("source_lock_sha256")
            == HISTORICAL_SOURCE_LOCK_SHA256
            and independent_decision.get("source_calibration_decision_sha256")
            == sha256(SAME_DECISION)
            and independent_decision.get("source_calibration_selection_sha256")
            == sha256(SELECTION)
            and independent_decision.get("same_seed_analysis_sha256")
            == sha256(INDEPENDENT_ANALYSIS)
            and independent_decision.get("replicates") == 9
            and independent_decision.get("takeover_margin") == 0.161
            and independent_decision.get("independent_seed_target")
            == INDEPENDENT_TARGET
            and independent_decision.get("reference_required_passes")
            == INDEPENDENT_REQUIRED_PASSES
            and independent_decision.get("reference_patch_sha256")
            == REFERENCE_PATCH_SHA256
            and independent_decision.get("measurement_amendment_sha256")
            == sha256(INDEPENDENT_MEASUREMENT_AMENDMENT)
            and independent_analysis.get("compatibility_projection", {}).get(
                "source_analysis_sha256"
            )
            == sha256(SAME_ANALYSIS)
            and independent_analysis.get("compatibility_projection", {}).get(
                "projection_only"
            )
            is True
            and INDEPENDENT_DECISION.stat().st_mode & 0o777 == 0o400
            and INDEPENDENT_ANALYSIS.stat().st_mode & 0o777 == 0o400
            and reveal.get("schema") == 1
            and reveal.get("kind") == "sim-latency-independent-seed-reveal"
            and reveal.get("replicates_per_reference") == 1
            and reveal.get("calibration_decision_sha256")
            == sha256(INDEPENDENT_DECISION)
            and isinstance(reveal.get("seeds"), list)
            and len(reveal["seeds"]) == INDEPENDENT_TARGET
            and reveal_projection == commitment.get("seeds")
            and reveal.get("seeds") == terminal_reveal.get("seeds")
            and INDEPENDENT_REVEAL.stat().st_mode & 0o777 == 0o400
            and attestation.get("accepted") is True
            and attestation.get("campaign_commitment_sha256")
            == sha256(INDEPENDENT_COMMITMENT)
            and attestation.get("campaign_progress_sha256")
            == sha256(INDEPENDENT_PROGRESS)
            and attestation.get("seed_reveal_sha256") == sha256(INDEPENDENT_REVEAL)
            and attestation.get("protocol_sha256") == sha256(INDEPENDENT_PROTOCOL)
            and attestation.get("measurement_amendment_sha256")
            == sha256(INDEPENDENT_MEASUREMENT_AMENDMENT)
            and attestation.get("independent_reference_r1_correction_sha256")
            == R1_CORRECTION_SHA256
            and attestation.get("terminal_decision_sha256")
            == sha256(INDEPENDENT_TERMINAL_DECISION)
            and attestation.get("target_independent_seeds")
            == INDEPENDENT_TARGET
            and attestation.get("reference_required_passes")
            == INDEPENDENT_REQUIRED_PASSES
            and attestation.get("reference_ordering_passes", 0)
            >= INDEPENDENT_REQUIRED_PASSES
            and attestation.get("ordering_metric") == ORDERING_METRIC
            and attestation.get("design_sha256") == REFERENCE_V5_DESIGN_SHA256
            and attestation.get("retired_seed_commitments_sha256")
            == REFERENCE_V5_RETIRED_COMMITMENTS_SHA256
            and terminal_decision.get("kind")
            == "sim-latency-reference-v5-hidden-launch-decision"
            and terminal_decision.get("accepted") is True
            and terminal_decision.get("campaign_exit_code") == 0
            and terminal_decision.get("completed_independent_seeds")
            == INDEPENDENT_TARGET
            and terminal_decision.get("reference_required_passes")
            == INDEPENDENT_REQUIRED_PASSES
            and terminal_decision.get("reference_ordering_passes", 0)
            >= INDEPENDENT_REQUIRED_PASSES
            and terminal_decision.get("ordering_metric") == ORDERING_METRIC
            and terminal_decision.get("campaign_commitment_sha256")
            == sha256(INDEPENDENT_COMMITMENT)
            and terminal_decision.get("seed_reveal_sha256")
            == sha256(INDEPENDENT_TERMINAL_REVEAL)
            and terminal_decision.get("protocol_sha256")
            == sha256(INDEPENDENT_PROTOCOL)
            and terminal_decision.get("measurement_amendment_sha256")
            == sha256(INDEPENDENT_MEASUREMENT_AMENDMENT)
            and terminal_decision.get("cleanup", {}).get(
                "residual_competition_containers"
            )
            == 0
            and terminal_decision.get("cleanup", {}).get(
                "residual_competition_networks"
            )
            == 0
            and terminal_reveal.get("kind")
            == "sim-latency-reference-v5-hidden-launch-seed-reveal"
            and terminal_reveal.get("campaign_commitment_sha256")
            == sha256(INDEPENDENT_COMMITMENT)
            and terminal_reveal.get("pilot_seed_reuse_forbidden") is True
            and terminal_reveal.get("all_prior_seed_reuse_forbidden") is True
            and terminal_reveal.get("retired_seed_commitments_sha256")
            == REFERENCE_V5_RETIRED_COMMITMENTS_SHA256
            and attestation_repair.get("schema") == 1
            and attestation_repair.get("kind")
            == "sim-latency-hidden-attestation-schema-postprocessing-repair"
            and attestation_repair.get("source_lock_sha256")
            == HISTORICAL_SOURCE_LOCK_SHA256
            and attestation_repair.get("original_runner_sha256")
            == sha256(REFERENCE_V5_RUNNER)
            and attestation_repair.get("repair_script_sha256")
            == sha256(INDEPENDENT_ATTESTATION_REPAIR_SCRIPT)
            and attestation_repair.get("terminal_decision_sha256")
            == sha256(INDEPENDENT_TERMINAL_DECISION)
            and attestation_repair.get("terminal_progress_sha256")
            == sha256(INDEPENDENT_PROGRESS)
            and attestation_repair.get("campaign_commitment_sha256")
            == sha256(INDEPENDENT_COMMITMENT)
            and attestation_repair.get("calibration_decision_sha256")
            == sha256(INDEPENDENT_DECISION)
            and attestation_repair.get("terminal_reveal_sha256")
            == sha256(INDEPENDENT_TERMINAL_REVEAL)
            and attestation_repair.get("campaign_reveal_sha256")
            == sha256(INDEPENDENT_REVEAL)
            and attestation_repair.get("attestation_sha256")
            == sha256(INDEPENDENT_ATTESTATION)
            and isinstance(repair_service_state, dict)
            and repair_service_state.get("active_state") == "failed"
            and repair_service_state.get("result") == "exit-code"
            and repair_service_state.get("exec_main_status") == 1
            and repair_service_state.get("restarts") == 0
            and attestation_repair.get("terminal_only_runner_exit_code") == 0
            and attestation_repair.get("attempt_directory_count_before") == 15
            and attestation_repair.get("attempt_directory_count_after") == 15
            and attestation_repair.get("seed_results_or_worker_evidence_changed")
            is False
            and attestation_repair.get("statistical_measurements_rerun") is False
            and attestation_repair.get("measurements_censored") is False
            and attestation_repair.get("original_measurement_artifacts_changed")
            is False
            and attestation_repair.get("campaign_reveal_reconstructed") is True
            and attestation_repair.get("reveal_documents_byte_identical") is False
            and attestation_repair.get("commitment_derivations_reverified") is True
            and attestation_repair.get("generator_seed_derivations_reverified")
            is True
            and attestation_repair.get("public_private_terminal_lineage_reverified")
            is True
            and attestation_repair.get("cleanup_before", {}).get(
                "residual_competition_containers"
            )
            == 0
            and attestation_repair.get("cleanup_before", {}).get(
                "residual_competition_networks"
            )
            == 0
            and attestation_repair.get("cleanup_after", {}).get(
                "residual_competition_containers"
            )
            == 0
            and attestation_repair.get("cleanup_after", {}).get(
                "residual_competition_networks"
            )
            == 0
            and isinstance(repair_invalid_attempt, dict)
            and repair_invalid_attempt.get("retained_private_for_forensics")
            is True
            and repair_invalid_attempt.get("mirrored_reveal_sha256")
            == sha256(INDEPENDENT_TERMINAL_REVEAL)
            and INDEPENDENT_ATTESTATION_REPAIR.stat().st_mode & 0o777 == 0o400
        ):
            raise AuditError("independent result, commitment, or reveal chain is invalid")
        (
            seed_results_sha256,
            seed_result_hashes,
            reference_placeability_counts,
            authenticated_manifest_entries,
            authenticated_manifest_bytes,
        ) = independent_seed_result_evidence(commitment)
        terminal_seed_entries = terminal_decision.get("seed_results")
        if not (
            isinstance(terminal_seed_entries, list)
            and len(terminal_seed_entries) == INDEPENDENT_TARGET
            and [entry.get("seed_index") for entry in terminal_seed_entries]
            == list(range(1, INDEPENDENT_TARGET + 1))
            and all(
                entry.get("seed_result_sha256")
                == seed_result_hashes[f"{entry['seed_index']:02d}"]
                for entry in terminal_seed_entries
            )
        ):
            raise AuditError("terminal per-seed result hashes are invalid")
        terminal_aggregate = hashlib.sha256(
            json.dumps(
                terminal_seed_entries,
                sort_keys=True,
                separators=(",", ":"),
                allow_nan=False,
            ).encode("utf-8")
        ).hexdigest()
        if not (
            terminal_decision.get("seed_result_aggregate_sha256")
            == terminal_aggregate
            and attestation.get("seed_result_aggregate_sha256")
            == terminal_aggregate
        ):
            raise AuditError("terminal seed-result aggregate is invalid")
        check(
            checks,
            "independent_reference_separability",
            "pass",
            "Five fresh precommitted independent seeds passed the authorized 4/5 reference-ordering launch screen.",
            {
                "progress_sha256": sha256(INDEPENDENT_PROGRESS),
                "commitment_sha256": sha256(INDEPENDENT_COMMITMENT),
                "reveal_sha256": sha256(INDEPENDENT_REVEAL),
                "attestation_sha256": sha256(INDEPENDENT_ATTESTATION),
                "terminal_decision_sha256": sha256(INDEPENDENT_TERMINAL_DECISION),
                "terminal_reveal_sha256": sha256(INDEPENDENT_TERMINAL_REVEAL),
                "attestation_repair_sha256": sha256(
                    INDEPENDENT_ATTESTATION_REPAIR
                ),
                "attestation_repair_script_sha256": sha256(
                    INDEPENDENT_ATTESTATION_REPAIR_SCRIPT
                ),
                "protocol_sha256": sha256(INDEPENDENT_PROTOCOL),
                "compatibility_decision_sha256": sha256(INDEPENDENT_DECISION),
                "source_calibration_selection_sha256": sha256(SELECTION),
                "independent_seed_results_sha256": seed_results_sha256,
                "terminal_seed_result_aggregate_sha256": terminal_aggregate,
                "independent_seed_result_hashes": seed_result_hashes,
                "reference_placeability_counts": reference_placeability_counts,
                "authenticated_manifest_entries": authenticated_manifest_entries,
                "authenticated_manifest_bytes": authenticated_manifest_bytes,
                "reference_ordering_passes": progress.get(
                    "reference_ordering_passes"
                ),
                "reference_required_passes": INDEPENDENT_REQUIRED_PASSES,
            },
        )
    except (AuditError, OSError, ValueError) as exc:
        check(checks, "independent_reference_separability", "fail", str(exc))


def audit_security_and_packages(checks: list[dict[str, Any]]) -> None:
    try:
        source_lock = load_json(SOURCE_LOCK)
        host = load_json(HOST_SELF_CHECK)
        pair = load_json(SAME / "attempt-20104/worker-result.json")
        control_boundary = load_json(CONTROL_BOUNDARY)
        pre_measurement = source_lock.get("pre_measurement_gates", {})
        boolean_pre_measurement = [
            value for value in pre_measurement.values() if isinstance(value, bool)
        ]
        security = pair.get("security")
        if not (
            len(boolean_pre_measurement) >= 10
            and all(boolean_pre_measurement)
            and pre_measurement.get("residual_competition_containers") == 0
            and host.get("resource_bomb_cleanup_verified") is True
            and host.get("default_deny_network") is True
            and host.get("checks", {}).get("docker_user_namespace") is True
            and security_evidence_authenticated(security)
            and sha256(CONTROL_BOUNDARY) == CONTROL_BOUNDARY_SHA256
            and sha256(CONTROL_BOUNDARY_VERIFIER)
            == CONTROL_BOUNDARY_VERIFIER_SHA256
            and control_boundary.get("passed") is True
            and control_boundary.get("evaluator_boundary", {}).get(
                "control_resources_present"
            )
            is False
            and control_boundary.get("control_plane", {}).get(
                "mounted_into_evaluator"
            )
            is False
            and command([str(CONTROL_BOUNDARY_VERIFIER)], SERVER)
            == "control-plane secret boundary: passed"
        ):
            raise AuditError("secure-evaluator evidence is incomplete")
        check(
            checks,
            "secure_evaluator",
            "pass",
            "The production-scale evaluator, local-only mounts, userns, default-deny network, and hostile-job cleanup are authenticated.",
            {
                "host_self_check_sha256": sha256(
                    HOST_SELF_CHECK
                ),
                "resource_bomb_sha256": source_lock.get("host", {}).get(
                    "resource_bomb_report_sha256"
                ),
                "worker_result_sha256": sha256(
                    SAME / "attempt-20104/worker-result.json"
                ),
                "control_plane_secret_boundary_sha256": CONTROL_BOUNDARY_SHA256,
                "security_boolean_count": sum(
                    value is True for value in security.values()
                ),
            },
        )
    except (AuditError, OSError, ValueError) as exc:
        check(checks, "secure_evaluator", "fail", str(exc))

    try:
        evidence = load_json(PARALLEL_EVIDENCE)
        control_release = load_json(CONTROL_SOURCE_RELEASE)
        passed = [item for item in evidence.get("checks", []) if item.get("exit_code") == 0]
        release_checks = control_release.get("checks")
        release_sources = control_release.get("source_sha256")
        if not (
            sha256(PARALLEL_EVIDENCE) == PARALLEL_EVIDENCE_SHA256
            and evidence.get("tracked_worktrees_clean") is True
            and len(passed) == 3
            and any(item.get("id") == "miner_package" and item.get("tests_run") == 17 for item in passed)
            and sha256(CONTROL_SOURCE_RELEASE) == CONTROL_SOURCE_RELEASE_SHA256
            and CONTROL_SOURCE_RELEASE.stat().st_mode & 0o777 == 0o400
            and control_release.get("kind")
            == "sim-latency-control-plane-source-release"
            and control_release.get("season_base_commit") == SERVER_COMMIT
            and control_release.get("control_plane_commit")
            == CONTROL_SOURCE_COMMIT
            and control_release.get("origin_control_plane_commit")
            == CONTROL_SOURCE_COMMIT
            and control_release.get("candidate_base_unchanged") is False
            and control_release.get("candidate_base_change_authorized") is True
            and control_release.get("evaluator_image_unchanged") is False
            and control_release.get("scorer_source_unchanged") is True
            and control_release.get("simulator_source_unchanged") is True
            and control_release.get("remediation_amendment_sha256")
            == REMEDIATION_AMENDMENT_SHA256
            and isinstance(release_checks, list)
            and {item.get("id") for item in release_checks}
            == {
                "deterministic_wrapped_connection_regression",
                "exact_image_regression",
                "evaluator_docker_smoke",
                "reference_patch_static_qualification",
                "remote_commit_verified",
            }
            and all(item.get("exit_code") == 0 for item in release_checks)
            and isinstance(release_sources, dict)
            and release_sources
            and all(
                sha256(
                    (
                        SERVER
                        if relative_path in {"db.go", "db_error_test.go"}
                        else EVIDENCE_WORKTREE
                    )
                    / relative_path
                )
                == digest
                for relative_path, digest in release_sources.items()
            )
            and command(["git", "rev-parse", "HEAD"], CONTROL_SOURCE_WORKTREE)
            == CONTROL_SOURCE_COMMIT
            and command(["git", "rev-parse", "@{upstream}"], CONTROL_SOURCE_WORKTREE)
            == CONTROL_SOURCE_COMMIT
            and not command(
                ["git", "status", "--porcelain", "--untracked-files=no"],
                CONTROL_SOURCE_WORKTREE,
            )
        ):
            raise AuditError("API/package conformance evidence changed")
        check(
            checks,
            "api_and_miner_package",
            "pass",
            "Competition package, dual OpenAPI conformance, miner package, and the pushed trusted-rebaseline release are authenticated.",
            {
                "parallel_readiness_evidence_sha256": PARALLEL_EVIDENCE_SHA256,
                "control_plane_source_release_sha256": CONTROL_SOURCE_RELEASE_SHA256,
                "control_plane_commit": CONTROL_SOURCE_COMMIT,
            },
        )
    except (AuditError, OSError, ValueError) as exc:
        check(checks, "api_and_miner_package", "fail", str(exc))


def audit_production_and_reports(checks: list[dict[str, Any]]) -> None:
    protocol_valid = False
    try:
        protocol = load_json(PRODUCTION_PROTOCOL)
        protocol_valid = (
            sha256(PRODUCTION_PROTOCOL) == PRODUCTION_PROTOCOL_SHA256
            and PRODUCTION_PROTOCOL.stat().st_mode & 0o777 == 0o400
            and protocol.get("kind")
            == "sim-latency-production-staging-protocol"
            and protocol.get("source_lock_sha256")
            == HISTORICAL_SOURCE_LOCK_SHA256
            and protocol.get("control_plane_commit")
            == HISTORICAL_CONTROL_SOURCE_COMMIT
            and protocol.get("control_plane_source_release_sha256")
            == HISTORICAL_CONTROL_SOURCE_RELEASE_SHA256
            and protocol.get("evaluator_image_digest")
            == HISTORICAL_EVALUATOR_IMAGE
            and protocol.get("measurement_dependencies", {}).get(
                "same_seed_pairs"
            )
            == SAME_SEED_TARGET
            and protocol.get("measurement_dependencies", {}).get(
                "independent_seeds"
            )
            == PRIOR_INDEPENDENT_TARGET
            and protocol.get("measurement_dependencies", {}).get(
                "required_reference_ordering_passes"
            )
            == PRIOR_REQUIRED_PASSES
            and protocol.get("final_evidence", {}).get(
                "boolean_only_attestation_forbidden"
            )
            is True
        )
        if not protocol_valid:
            raise AuditError("production staging protocol changed or is incomplete")
    except (AuditError, OSError, ValueError) as exc:
        check(checks, "production_staging", "fail", str(exc))

    if protocol_valid and not READINESS.exists():
        check(
            checks,
            "production_staging",
            "pending",
            "The hash-frozen post-calibration API/store integration and full staging round remain pending.",
            {"production_staging_protocol_sha256": PRODUCTION_PROTOCOL_SHA256},
        )
    elif protocol_valid:
        try:
            readiness = load_json(READINESS)
            remediation = load_json(REMEDIATION_AMENDMENT)
            staging_amendment = load_json(STAGING_REFERENCE_V5_AMENDMENT)
            commitment = load_json(INDEPENDENT_COMMITMENT)
            (
                seed_results_sha256,
                seed_result_hashes,
                reference_placeability_counts,
                _authenticated_manifest_entries,
                _authenticated_manifest_bytes,
            ) = independent_seed_result_evidence(commitment)
            records = {
                check_id: load_production_check(readiness, check_id)
                for check_id in PRODUCTION_ASSERTIONS
            }
            replacement = staging_amendment.get(
                "replacement_measurement_dependencies"
            )
            retained = staging_amendment.get("retained_invariants")
            if not (
                STAGING_REFERENCE_V5_AMENDMENT.stat().st_mode & 0o777 == 0o400
                and staging_amendment.get("kind")
                == "sim-latency-production-staging-reference-v5-amendment"
                and staging_amendment.get("draft") is False
                and staging_amendment.get("authorized") is True
                and staging_amendment.get("source_lock_sha256")
                == HISTORICAL_SOURCE_LOCK_SHA256
                and staging_amendment.get(
                    "original_production_staging_protocol_sha256"
                )
                == PRODUCTION_PROTOCOL_SHA256
                and staging_amendment.get("pilot_decision_sha256")
                == sha256(REFERENCE_V5_PILOT_DECISION)
                and staging_amendment.get("reference_v5_qualification_sha256")
                == sha256(REFERENCE_V5_QUALIFICATION)
                and staging_amendment.get("hidden_campaign_attestation_sha256")
                == sha256(INDEPENDENT_ATTESTATION)
                and staging_amendment.get("hidden_campaign_decision_sha256")
                == sha256(INDEPENDENT_TERMINAL_DECISION)
                and staging_amendment.get("hidden_campaign_protocol_sha256")
                == sha256(INDEPENDENT_PROTOCOL)
                and staging_amendment.get("hidden_attestation_repair_sha256")
                == sha256(INDEPENDENT_ATTESTATION_REPAIR)
                and staging_amendment.get(
                    "hidden_attestation_repair_script_sha256"
                )
                == sha256(INDEPENDENT_ATTESTATION_REPAIR_SCRIPT)
                and isinstance(replacement, dict)
                and replacement.get("same_seed_pairs") == SAME_SEED_TARGET
                and replacement.get("independent_seeds") == INDEPENDENT_TARGET
                and replacement.get("required_reference_ordering_passes")
                == INDEPENDENT_REQUIRED_PASSES
                and replacement.get("selected_competition_replicates") == 9
                and replacement.get("takeover_margin") == 0.161
                and isinstance(retained, dict)
                and retained
                and all(value is True for value in retained.values())
                and sha256(REMEDIATION_AMENDMENT)
                == REMEDIATION_AMENDMENT_SHA256
                and REMEDIATION_AMENDMENT.stat().st_mode & 0o777 == 0o400
                and remediation.get("authorized") is True
                and remediation.get("historical_calibration", {}).get(
                    "source_lock_sha256"
                )
                == HISTORICAL_SOURCE_LOCK_SHA256
                and remediation.get("replacement", {}).get(
                    "source_lock_sha256"
                )
                == SOURCE_LOCK_SHA256
                and remediation.get("replacement", {}).get(
                    "evaluator_image_digest"
                )
                == EVALUATOR_IMAGE
                and readiness.get("schema") == 1
                and readiness.get("kind") == "sim-latency-production-readiness-final"
                and readiness.get("source_lock_sha256") == SOURCE_LOCK_SHA256
                and readiness.get("historical_calibration_source_lock_sha256")
                == HISTORICAL_SOURCE_LOCK_SHA256
                and readiness.get("production_staging_protocol_sha256")
                == PRODUCTION_PROTOCOL_SHA256
                and readiness.get(
                    "production_staging_reference_v5_amendment_sha256"
                )
                == sha256(STAGING_REFERENCE_V5_AMENDMENT)
                and readiness.get(
                    "production_release_self_check_contract_amendment_sha256"
                )
                == RELEASE_SELF_CHECK_AMENDMENT_SHA256
                and readiness.get(
                    "production_staging_attempt_06_remediation_amendment_sha256"
                )
                == REMEDIATION_AMENDMENT_SHA256
                and readiness.get("control_plane_commit")
                == CONTROL_SOURCE_COMMIT
                and readiness.get("control_plane_source_release_sha256")
                == CONTROL_SOURCE_RELEASE_SHA256
                and readiness.get("evaluator_image_digest")
                == EVALUATOR_IMAGE
                and readiness.get("host_qualification_sha256")
                == HOST_QUALIFICATION_SHA256
                and readiness.get("same_seed_selection_sha256")
                == sha256(SELECTION)
                and readiness.get("placeability_policy_amendment_sha256")
                == PLACEABILITY_POLICY_SHA256
                and readiness.get("same_seed_postprocessing_repair_sha256")
                == POSTPROCESSING_REPAIR_SHA256
                and readiness.get("strict_same_seed_analysis_sha256")
                == STRICT_SAME_ANALYSIS_SHA256
                and readiness.get("pre_repair_same_seed_progress_sha256")
                == PRE_REPAIR_PROGRESS_SHA256
                and readiness.get("independent_attestation_sha256")
                == sha256(INDEPENDENT_ATTESTATION)
                and readiness.get("independent_attestation_repair_sha256")
                == sha256(INDEPENDENT_ATTESTATION_REPAIR)
                and readiness.get(
                    "independent_attestation_repair_script_sha256"
                )
                == sha256(INDEPENDENT_ATTESTATION_REPAIR_SCRIPT)
                and readiness.get("independent_calibration_decision_sha256")
                == sha256(INDEPENDENT_DECISION)
                and readiness.get("independent_terminal_decision_sha256")
                == sha256(INDEPENDENT_TERMINAL_DECISION)
                and readiness.get("independent_protocol_sha256")
                == sha256(INDEPENDENT_PROTOCOL)
                and readiness.get("reference_v5_qualification_sha256")
                == sha256(REFERENCE_V5_QUALIFICATION)
                and readiness.get("independent_seed_commitment_sha256")
                == sha256(INDEPENDENT_COMMITMENT)
                and readiness.get("independent_seed_results_sha256")
                == seed_results_sha256
                and READINESS.stat().st_mode & 0o777 == 0o400
            ):
                raise AuditError("production-readiness evidence is not complete")
            check(
                checks,
                "production_staging",
                "pass",
                "The API/store path, monitoring/recovery, retention, and full staging round pass.",
                {
                    "production_readiness_sha256": sha256(READINESS),
                    "production_staging_protocol_sha256": PRODUCTION_PROTOCOL_SHA256,
                    "production_staging_reference_v5_amendment_sha256": sha256(
                        STAGING_REFERENCE_V5_AMENDMENT
                    ),
                    "independent_seed_results_sha256": seed_results_sha256,
                    "independent_seed_result_hashes": seed_result_hashes,
                    "reference_placeability_counts": (
                        reference_placeability_counts
                    ),
                    "content_addressed_check_records": records,
                },
            )
        except (AuditError, OSError, ValueError) as exc:
            check(checks, "production_staging", "fail", str(exc))

    if not CALIBRATION_MD.exists():
        check(
            checks,
            "calibration_document",
            "pending",
            "APEX-CALIBRATION.md is created only after measurement completion.",
        )
    else:
        text = CALIBRATION_MD.read_text(encoding="utf-8")
        required_terms = (
            "baseline",
            "takeover",
            "independent",
            "reference",
            "resource",
            "timeout",
            "12",
            "11/12",
            "4/5",
            "familywise",
            "false-rejection",
            "not confidence-equivalent",
            "8/12",
            "r=9",
            "apex-season-1",
            "public patch-authoring base",
        )
        superseded_template = (
            "not yet qualified" in text.lower()
            or "not set" in text.lower()
            or "not run" in text.lower()
        )
        state = (
            "pass"
            if not superseded_template
            and SOURCE_LOCK_SHA256 in text
            and HISTORICAL_SOURCE_LOCK_SHA256 in text
            and REMEDIATION_AMENDMENT_SHA256 in text
            and SEASON_BASE_EQUIVALENCE_SHA256 in text
            and SERVER_COMMIT in text
            and all(term in text.lower() for term in required_terms)
            else "pending"
        )
        check(
            checks,
            "calibration_document",
            state,
            "APEX-CALIBRATION.md contains the required sizing and statistical disclosures."
            if state == "pass"
            else "The existing APEX-CALIBRATION.md is the explicit pre-calibration template and must be replaced after the campaigns pass.",
            {"sha256": sha256(CALIBRATION_MD)},
        )

    if not FINAL_REPORT.exists():
        check(
            checks,
            "final_html_report",
            "pending",
            "finalize-report.html is created only after finalization.",
        )
    else:
        try:
            text = FINAL_REPORT.read_text(encoding="utf-8")
            parser = FinalReportParser()
            parser.feed(text)
            preview_text = FINAL_PREVIEW.read_text(encoding="utf-8")
            preview_parser = FinalReportParser()
            preview_parser.feed(preview_text)
            evidence = load_json(FINAL_REPORT_EVIDENCE)
            commitment = load_json(INDEPENDENT_COMMITMENT)
            (
                seed_results_sha256,
                seed_result_hashes,
                reference_placeability_counts,
                _authenticated_manifest_entries,
                _authenticated_manifest_bytes,
            ) = independent_seed_result_evidence(commitment)
            report_sha = sha256(FINAL_REPORT)
            preview_sha = sha256(FINAL_PREVIEW)
            visual_count = sum(parser.section_visuals)
            if not (
                len(parser.section_visuals) == 4
                and all(count >= 1 for count in parser.section_visuals)
                and parser.section_depth == 0
                and parser.baseline_ids
                and parser.baseline_ids == parser.threshold_line_ids
                and parser.baseline_ids == parser.threshold_label_ids
                and FINAL_REPORT.stat().st_mode & 0o777 == 0o444
                and len(preview_parser.section_visuals) == 4
                and all(count >= 1 for count in preview_parser.section_visuals)
                and preview_parser.section_depth == 0
                and preview_parser.baseline_ids
                and preview_parser.baseline_ids
                == preview_parser.threshold_line_ids
                and preview_parser.baseline_ids
                == preview_parser.threshold_label_ids
                and preview_parser.baseline_ids == parser.baseline_ids
                and FINAL_PREVIEW.stat().st_mode & 0o777 == 0o444
                and "preview-only" not in text.lower()
                and "pending" not in text.lower()
                and "shareable final preview" in preview_text.lower()
                and "launch-ready preview" in preview_text.lower()
                and "preview-only" not in preview_text.lower()
                and "pending" not in preview_text.lower()
                and "original familywise placeability rule produced no eligible"
                in text.lower()
                and "94.614%" in text
                and "86.612%" in text
                and "placeability is a separate diagnostic" in text.lower()
                and "apex-season-1" in text
                and SERVER_COMMIT in text
                and SERVER_COMMIT in preview_text
                and EVALUATOR_IMAGE.removeprefix("sha256:")[:12] in preview_text
                and evidence.get("schema") == 1
                and evidence.get("kind") == "sim-latency-finalize-report-evidence"
                and evidence.get("source_lock_sha256") == SOURCE_LOCK_SHA256
                and evidence.get(
                    "historical_calibration_source_lock_sha256"
                )
                == HISTORICAL_SOURCE_LOCK_SHA256
                and evidence.get(
                    "production_staging_attempt_06_remediation_amendment_sha256"
                )
                == REMEDIATION_AMENDMENT_SHA256
                and evidence.get("season_base_equivalence_sha256")
                == SEASON_BASE_EQUIVALENCE_SHA256
                and evidence.get("report_sha256") == report_sha
                and evidence.get("preview_sha256") == preview_sha
                and evidence.get("same_seed_selection_sha256")
                == sha256(SELECTION)
                and evidence.get("strict_same_seed_analysis_sha256")
                == STRICT_SAME_ANALYSIS_SHA256
                and evidence.get("pre_repair_same_seed_progress_sha256")
                == PRE_REPAIR_PROGRESS_SHA256
                and evidence.get("placeability_policy_amendment_sha256")
                == PLACEABILITY_POLICY_SHA256
                and evidence.get("same_seed_postprocessing_repair_sha256")
                == POSTPROCESSING_REPAIR_SHA256
                and evidence.get("independent_attestation_sha256")
                == sha256(INDEPENDENT_ATTESTATION)
                and evidence.get("independent_calibration_decision_sha256")
                == sha256(INDEPENDENT_DECISION)
                and evidence.get("independent_terminal_decision_sha256")
                == sha256(INDEPENDENT_TERMINAL_DECISION)
                and evidence.get("independent_protocol_sha256")
                == sha256(INDEPENDENT_PROTOCOL)
                and evidence.get("reference_v5_qualification_sha256")
                == sha256(REFERENCE_V5_QUALIFICATION)
                and evidence.get(
                    "production_staging_reference_v5_amendment_sha256"
                )
                == sha256(STAGING_REFERENCE_V5_AMENDMENT)
                and evidence.get("independent_seed_commitment_sha256")
                == sha256(INDEPENDENT_COMMITMENT)
                and evidence.get("independent_seed_results_sha256")
                == seed_results_sha256
                and evidence.get("independent_seed_result_hashes")
                == seed_result_hashes
                and evidence.get("reference_placeability_counts")
                == reference_placeability_counts
                and evidence.get("production_readiness_sha256")
                == sha256(READINESS)
                and evidence.get("sections") == 4
                and evidence.get("section_svg_counts")
                == parser.section_visuals
                and set(evidence.get("baseline_ids", []))
                == parser.baseline_ids
                and evidence.get("all_baselines_have_threshold_lines") is True
                and evidence.get("preview_sections") == 4
                and evidence.get("preview_section_svg_counts")
                == preview_parser.section_visuals
                and set(evidence.get("preview_baseline_ids", []))
                == preview_parser.baseline_ids
                and evidence.get("preview_all_baselines_have_threshold_lines")
                is True
                and FINAL_REPORT_EVIDENCE.stat().st_mode & 0o777 == 0o400
            ):
                raise AuditError(
                    "final report structure, threshold mapping, or evidence chain is incomplete"
                )
            check(
                checks,
                "final_html_report",
                "pass",
                "The authenticated final report and shareable preview each have four sections, a visual in each, and a mapped threshold line and label for every baseline.",
                {
                    "sha256": report_sha,
                    "preview_sha256": preview_sha,
                    "evidence_sha256": sha256(FINAL_REPORT_EVIDENCE),
                    "sections": len(parser.section_visuals),
                    "section_svg_counts": parser.section_visuals,
                    "preview_sections": len(preview_parser.section_visuals),
                    "preview_section_svg_counts": preview_parser.section_visuals,
                    "baseline_ids": sorted(parser.baseline_ids),
                    "independent_seed_results_sha256": seed_results_sha256,
                    "reference_placeability_counts": (
                        reference_placeability_counts
                    ),
                },
            )
        except (AuditError, OSError, TypeError, ValueError) as exc:
            check(checks, "final_html_report", "fail", str(exc))

    check(
        checks,
        "external_apex_handoff",
        "pending",
        "Registry activation, partner acceptance, credentials, and organizational on-call ownership are external launch gates.",
        launch_required=False,
    )


def audit() -> dict[str, Any]:
    checks: list[dict[str, Any]] = []
    audit_source(checks)
    audit_frontier(checks)
    audit_same_seed(checks)
    audit_independent_protocol(checks)
    audit_independent_results(checks)
    audit_security_and_packages(checks)
    audit_production_and_reports(checks)
    required = [item for item in checks if item["launch_required"]]
    return {
        "schema": 1,
        "kind": "sim-latency-finalization-audit",
        "source_lock_sha256": SOURCE_LOCK_SHA256,
        "historical_calibration_source_lock_sha256": (
            HISTORICAL_SOURCE_LOCK_SHA256
        ),
        "production_staging_attempt_06_remediation_amendment_sha256": (
            REMEDIATION_AMENDMENT_SHA256
        ),
        "local_finalization_complete": all(item["state"] == "pass" for item in required),
        "required_passes": sum(item["state"] == "pass" for item in required),
        "required_pending": sum(item["state"] == "pending" for item in required),
        "required_failures": sum(item["state"] == "fail" for item in required),
        "external_handoff_complete": all(
            item["state"] == "pass"
            for item in checks
            if item["id"] == "external_apex_handoff"
        ),
        "checks": checks,
    }


def expect_manifest_failure(
    label: str, attempt_directory: Path, manifest: dict[str, Any]
) -> None:
    try:
        authenticate_evidence_manifest(attempt_directory, manifest)
    except AuditError:
        return
    raise AuditError(f"manifest self-test accepted {label}")


def self_test() -> None:
    security = {
        **{key: True for key in SECURITY_BOOLEAN_IDS},
        **{key: f"test-{key}" for key in SECURITY_ID_IDS},
    }
    if not security_evidence_authenticated(security):
        raise AuditError("security evidence self-test rejected valid evidence")
    invalid_security: dict[str, dict[str, Any]] = {}
    missing = security.copy()
    missing.pop("cleanup_complete")
    invalid_security["missing flag"] = missing
    false_flag = security.copy()
    false_flag["cleanup_complete"] = False
    invalid_security["false flag"] = false_flag
    extra = security.copy()
    extra["unfrozen_flag"] = True
    invalid_security["extra flag"] = extra
    empty_identifier = security.copy()
    empty_identifier["cgroup_id"] = ""
    invalid_security["empty identifier"] = empty_identifier
    if any(
        security_evidence_authenticated(value)
        for value in invalid_security.values()
    ):
        raise AuditError("security evidence self-test accepted invalid evidence")
    payload = b"authenticated evidence\n"
    payload_sha256 = hashlib.sha256(payload).hexdigest()
    with tempfile.TemporaryDirectory(
        prefix="sim-latency-finalization-audit-"
    ) as directory:
        attempt = Path(directory) / "attempt-1"
        evidence = attempt / "evidence"
        evidence.mkdir(parents=True)
        artifact = evidence / "result.json"
        artifact.write_bytes(payload)
        artifact.chmod(0o400)
        entry = {
            "path": "evidence/result.json",
            "sha256": payload_sha256,
            "bytes": len(payload),
        }
        manifest = {"artifacts": [entry]}
        if authenticate_evidence_manifest(attempt, manifest) != (
            1,
            len(payload),
        ):
            raise AuditError("manifest self-test count mismatch")
        expect_manifest_failure(
            "duplicate path", attempt, {"artifacts": [entry, entry.copy()]}
        )
        expect_manifest_failure(
            "parent traversal",
            attempt,
            {"artifacts": [{**entry, "path": "../result.json"}]},
        )
        expect_manifest_failure(
            "wrong byte count",
            attempt,
            {"artifacts": [{**entry, "bytes": len(payload) + 1}]},
        )
        artifact.chmod(0o600)
        artifact.write_bytes(b"tampered evidence\n")
        artifact.chmod(0o400)
        expect_manifest_failure("tampered content", attempt, manifest)
        artifact.chmod(0o600)
        artifact.write_bytes(payload)
        artifact.chmod(0o400)
        symlink = evidence / "linked.json"
        symlink.symlink_to(artifact)
        expect_manifest_failure(
            "symlink artifact",
            attempt,
            {
                "artifacts": [
                    {
                        **entry,
                        "path": "evidence/linked.json",
                    }
                ]
            },
        )
        linked_directory = attempt / "linked-evidence"
        linked_directory.symlink_to(evidence, target_is_directory=True)
        expect_manifest_failure(
            "symlink parent directory",
            attempt,
            {
                "artifacts": [
                    {
                        **entry,
                        "path": "linked-evidence/result.json",
                    }
                ]
            },
        )
    print("finalization audit manifest self-test: passed")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--require-complete", action="store_true")
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    if args.require_complete and args.self_test:
        raise AuditError("diagnostic modes are mutually exclusive")
    if args.self_test:
        self_test()
        return 0
    result = audit()
    json.dump(result, sys.stdout, indent=2, sort_keys=True, allow_nan=False)
    sys.stdout.write("\n")
    return int(args.require_complete and not result["local_finalization_complete"])


if __name__ == "__main__":
    raise SystemExit(main())
