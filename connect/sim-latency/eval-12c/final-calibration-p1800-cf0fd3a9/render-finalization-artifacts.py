#!/usr/bin/env python3
"""Render terminal calibration documentation from authenticated evidence."""

from __future__ import annotations

import argparse
import hashlib
import html
import json
import math
import os
import statistics
import tempfile
import time
from html.parser import HTMLParser
from pathlib import Path
from typing import Any


SERVER = Path("/home/by/urnetwork/server")
ROOT = (
    Path("/home/by/urnetwork/server-finalization-evidence")
    / "connect/sim-latency/eval-12c/"
    "final-calibration-p1800-cf0fd3a9"
)
HISTORICAL_ROOT = (
    SERVER
    / "connect/sim-latency/eval-12c/"
    "final-calibration-p1800-cf0fd3a9"
)
ROUND = HISTORICAL_ROOT / "post-frontier/p1800-c200-r80-q2"
REFERENCE_V5 = HISTORICAL_ROOT / "reference-requalification-v5"
INDEPENDENT_ROOT = REFERENCE_V5 / "hidden-launch-runtime"
INDEPENDENT = INDEPENDENT_ROOT / "independent-references"

SOURCE_LOCK = ROOT / "source-lock.json"
SEASON_BASE_EQUIVALENCE = HISTORICAL_ROOT / "season-base-equivalence.json"
FRONTIER = HISTORICAL_ROOT / "exact-frontier/frontier-decision.json"
POINT = HISTORICAL_ROOT / "exact-frontier/p1800-c200-r80-q2/point-summary.json"
FROZEN_ROUND = ROUND / "frozen-round.json"
SAME_PROGRESS = ROUND / "same-seed/progress.json"
SAME_ANALYSIS = ROUND / "same-seed-analysis.json"
STRICT_SAME_ANALYSIS = ROUND / "same-seed-analysis-familywise.json"
SAME_DECISION = ROUND / "calibration-decision.json"
PRE_REPAIR_PROGRESS = (
    ROUND / "same-seed/progress-before-postprocessing-repair.json"
)
SELECTION = HISTORICAL_ROOT / "post-frontier/final-calibration-selection.json"
SELECTION_ATTESTATION = (
    HISTORICAL_ROOT / "post-frontier/launch-compromise-selection-attestation.json"
)
PLACEABILITY_POLICY = (
    HISTORICAL_ROOT / "launch-readiness-placeability-policy-amendment.json"
)
POSTPROCESSING_REPAIR = HISTORICAL_ROOT / "same-seed-postprocessing-repair.json"
INDEPENDENT_PROGRESS = INDEPENDENT / "progress.json"
INDEPENDENT_ATTESTATION = INDEPENDENT_ROOT / "independent-campaign-attestation.json"
INDEPENDENT_DECISION = INDEPENDENT_ROOT / "calibration-decision.json"
INDEPENDENT_ANALYSIS = INDEPENDENT_ROOT / "same-seed-analysis.json"
INDEPENDENT_COMMITMENT = INDEPENDENT / "campaign-commitment.json"
INDEPENDENT_TERMINAL_DECISION = REFERENCE_V5 / "hidden-launch-decision.json"
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
REFERENCE_V5_QUALIFICATION = REFERENCE_V5 / "qualification.json"
STAGING_REFERENCE_V5_AMENDMENT = (
    HISTORICAL_ROOT / "production-staging-reference-v5-amendment.json"
)
READINESS = ROOT / "production-readiness-final.json"
REMEDIATION_AMENDMENT = (
    ROOT / "production-staging-attempt-06-remediation-amendment.json"
)

REPORT = SERVER / "finalize-report.html"
PREVIEW = SERVER / "final-preview.html"
REPORT_EVIDENCE = ROOT / "finalize-report-evidence.json"
CALIBRATION_DOCUMENT = SERVER / "connect/sim-latency/APEX-CALIBRATION.md"

SOURCE_LOCK_SHA256 = (
    "94c25024a92b5fcb5fa8bf324ff8022fde1074fd62bc210fc0ad5efbba0e4022"
)
HISTORICAL_SOURCE_LOCK_SHA256 = (
    "0cf71458833f3b1ae96a663357c583eba3a9c25a19d6c795c8549e4154141838"
)
SEASON_BASE_EQUIVALENCE_SHA256 = (
    "6bce6a80cecfee0297bcc11afbaa390576d8f542980d8797e4da33046daa07b3"
)
CALIBRATION_TEMPLATE_SHA256 = (
    "ff4883f7b9d0776ebe0e91d33e91a25d1f8a5bbb616776a135e1e1c06e8cc7cc"
)
PREVIEW_TEMPLATE_SHA256 = (
    "2011ec4cc129819d24c1f4726a0f5fbfa268f22d633b86c24cba58bf7246a027"
)
BASE_SHA = "46515d82fe98ff666c61b2b5bb1d34a89cf4dad8"
HISTORICAL_BASE_SHA = "5ca3d5242f4a7d40efe4415635608023b05a0956"
PUBLIC_AUTHORING_TAG = "apex-season-1"
PUBLIC_AUTHORING_COMMIT = "eb697281cbe0a19a27d7771fe69fb24c2c3dab8c"
EDITABLE_BLOB = "66e2d39956b958749dfdfd00f408d4c05f874833"
EVALUATOR_IMAGE = (
    "sha256:2abcf145c0f914899debbd2fd52e57a16cf20072165c8d13f04a0ba487198a4c"
)
SIMULATOR_SHA256 = (
    "bc843ce2b9cdcc41459362c7a682b08e7a12a8ac896443fe1e8aad94d4b17997"
)
HISTORICAL_EVALUATOR_IMAGE = (
    "sha256:cf0fd3a9e73385729ee8dcd8da7ea53eb59d5f372b9ff36789ec923056222038"
)
HISTORICAL_SIMULATOR_SHA256 = (
    "247a4d2998699eb439ade7987588cf886be707bde458a07ed1fb6a4fd84c102d"
)
HOST_QUALIFICATION_SHA256 = (
    "acf226db6b8e50d67f8957cddb3903d5d4e9e82566935d61d270ccb5b03463a3"
)
REMEDIATION_AMENDMENT_SHA256 = (
    "7971eeeac22c73781c0de1ce34c5296f79b2f223afbfe67d4a7b3fd2642de65d"
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
MEASUREMENT_AMENDMENT_SHA256 = (
    "3bd163e339cc7dc8e23757dd23ea238607f7eb6eaecc1959acd412661b9a770f"
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
LAUNCH_REPLICATES = 9
LAUNCH_PLACEABILITY_TARGET = 0.94
LAUNCH_PLACEABILITY_OBSERVED = 0.94614
LAUNCH_TAKEOVER_MARGIN = 0.161


class RenderError(RuntimeError):
    pass


class ReportShapeParser(HTMLParser):
    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.section_depth = 0
        self.section_visuals: list[int] = []
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
        if tag == "line" and threshold_for:
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


def require(condition: bool, message: str) -> None:
    if not condition:
        raise RenderError(message)


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


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def sha256_text(value: str) -> str:
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def independent_seed_results_digest() -> tuple[str, dict[str, str]]:
    digest = hashlib.sha256()
    file_hashes: dict[str, str] = {}
    for index in range(1, INDEPENDENT_TARGET + 1):
        path = INDEPENDENT / f"seed-{index:02d}/seed-result.json"
        result_sha256 = sha256(path)
        file_hashes[f"{index:02d}"] = result_sha256
        relative = path.relative_to(HISTORICAL_ROOT).as_posix()
        digest.update(
            f"{index:02d}\t{relative}\t{result_sha256}\n".encode("utf-8")
        )
    return digest.hexdigest(), file_hashes


def load_object(path: Path) -> dict[str, Any]:
    require(path.is_file() and not path.is_symlink(), f"unsafe input: {path}")
    value = json.loads(path.read_text(encoding="utf-8"))
    require(isinstance(value, dict), f"expected JSON object: {path}")
    return value


def finite_number(value: Any, label: str) -> float:
    require(
        isinstance(value, (int, float))
        and not isinstance(value, bool)
        and math.isfinite(value),
        f"invalid number: {label}",
    )
    return float(value)


def exact_mode(path: Path, mode: int) -> None:
    require(path.stat().st_mode & 0o777 == mode, f"unexpected mode: {path}")


def compact_sha(value: str, length: int = 12) -> str:
    normalized = value.removeprefix("sha256:")
    return normalized[:length] + "…"


def fmt_ms(value: float) -> str:
    return f"{value:,.3f} ms"


def fmt_pct(value: float) -> str:
    return f"{100 * value:.3f}%"


def esc(value: Any) -> str:
    return html.escape(str(value), quote=True)


def validate_terminal_inputs() -> dict[str, Any]:
    require(sha256(SOURCE_LOCK) == SOURCE_LOCK_SHA256, "source lock changed")
    for path, expected_hash in (
        (SEASON_BASE_EQUIVALENCE, SEASON_BASE_EQUIVALENCE_SHA256),
        (PLACEABILITY_POLICY, PLACEABILITY_POLICY_SHA256),
        (POSTPROCESSING_REPAIR, POSTPROCESSING_REPAIR_SHA256),
        (STRICT_SAME_ANALYSIS, STRICT_SAME_ANALYSIS_SHA256),
        (PRE_REPAIR_PROGRESS, PRE_REPAIR_PROGRESS_SHA256),
        (REMEDIATION_AMENDMENT, REMEDIATION_AMENDMENT_SHA256),
    ):
        exact_mode(path, 0o400)
        require(sha256(path) == expected_hash, f"authenticated input changed: {path}")
    source_lock = load_object(SOURCE_LOCK)
    season_base = load_object(SEASON_BASE_EQUIVALENCE)
    frontier = load_object(FRONTIER)
    point = load_object(POINT)
    frozen = load_object(FROZEN_ROUND)
    progress = load_object(SAME_PROGRESS)
    analysis = load_object(SAME_ANALYSIS)
    strict_analysis = load_object(STRICT_SAME_ANALYSIS)
    pre_repair_progress = load_object(PRE_REPAIR_PROGRESS)
    selection = load_object(SELECTION)
    selection_attestation = load_object(SELECTION_ATTESTATION)
    placeability_policy = load_object(PLACEABILITY_POLICY)
    postprocessing_repair = load_object(POSTPROCESSING_REPAIR)
    independent_progress = load_object(INDEPENDENT_PROGRESS)
    independent_attestation = load_object(INDEPENDENT_ATTESTATION)
    independent_decision = load_object(INDEPENDENT_DECISION)
    independent_analysis = load_object(INDEPENDENT_ANALYSIS)
    independent_commitment = load_object(INDEPENDENT_COMMITMENT)
    independent_terminal_decision = load_object(INDEPENDENT_TERMINAL_DECISION)
    independent_attestation_repair = load_object(INDEPENDENT_ATTESTATION_REPAIR)
    independent_protocol = load_object(INDEPENDENT_PROTOCOL)
    reference_v5_qualification = load_object(REFERENCE_V5_QUALIFICATION)
    staging_reference_v5_amendment = load_object(STAGING_REFERENCE_V5_AMENDMENT)
    remediation = load_object(REMEDIATION_AMENDMENT)
    readiness = load_object(READINESS)

    require(
        source_lock.get("repositories", {}).get("server") == BASE_SHA
        and source_lock.get("evaluator", {}).get("image_id") == EVALUATOR_IMAGE
        and source_lock.get("evaluator", {}).get("simulator_sha256")
        == SIMULATOR_SHA256,
        "source-lock evaluator identity",
    )
    require(
        remediation.get("kind")
        == "sim-latency-production-staging-attempt-06-remediation-amendment"
        and remediation.get("authorized") is True
        and remediation.get("historical_calibration", {}).get(
            "source_lock_sha256"
        )
        == HISTORICAL_SOURCE_LOCK_SHA256
        and remediation.get("replacement", {}).get("source_lock_sha256")
        == SOURCE_LOCK_SHA256
        and remediation.get("replacement", {}).get("evaluator_image_digest")
        == EVALUATOR_IMAGE
        and remediation.get("replacement", {}).get("simulator_sha256")
        == SIMULATOR_SHA256
        and remediation.get("replacement", {}).get(
            "host_qualification_sha256"
        )
        == HOST_QUALIFICATION_SHA256
        and remediation.get("required_measurement_bridge", {}).get(
            "replicates_per_side"
        )
        == LAUNCH_REPLICATES
        and remediation.get("required_measurement_bridge", {}).get(
            "all_eighteen_replicates_must_be_clean"
        )
        is True,
        "attempt-06 remediation lineage",
    )
    authoring = season_base.get("public_authoring_base")
    evaluator = season_base.get("authoritative_evaluator")
    policy = season_base.get("patch_policy")
    editable_blobs = season_base.get("editable_blobs")
    editable = (
        editable_blobs.get("connect/resident_contract_manager.go")
        if isinstance(editable_blobs, dict)
        else None
    )
    require(
        season_base.get("kind") == "sim-latency-season-base-equivalence"
        and isinstance(authoring, dict)
        and authoring.get("tag") == PUBLIC_AUTHORING_TAG
        and authoring.get("commit") == PUBLIC_AUTHORING_COMMIT
        and authoring.get("remote_tag_matches_local") is True
        and isinstance(evaluator, dict)
        and evaluator.get("commit") == HISTORICAL_BASE_SHA
        and evaluator.get("source_lock_sha256")
        == HISTORICAL_SOURCE_LOCK_SHA256
        and evaluator.get("image_digest") == HISTORICAL_EVALUATOR_IMAGE
        and isinstance(policy, dict)
        and policy.get("allowed_paths")
        == ["connect/resident_contract_manager.go"]
        and isinstance(editable, dict)
        and editable.get("identical") is True
        and editable.get("public_authoring_base_blob") == EDITABLE_BLOB
        and editable.get("authoritative_evaluator_blob") == EDITABLE_BLOB
        and season_base.get("all_allowed_path_blobs_identical") is True
        and season_base.get("local_reproduction_uses_evaluator_image") is True
        and season_base.get("seed_material_included") is False,
        "public authoring base equivalence",
    )
    require(
        frontier.get("accepted") is True
        and frontier.get("selected_point_id") == "p1800-c200-r80-q2"
        and frontier.get("source_lock_sha256")
        == HISTORICAL_SOURCE_LOCK_SHA256
        and frontier.get("evaluator_image_digest")
        == HISTORICAL_EVALUATOR_IMAGE,
        "frontier selection",
    )
    rejected = frontier.get("rejected_upper_bound")
    require(
        isinstance(rejected, dict)
        and rejected.get("point_id") == "p2700-c300-r120-q2"
        and finite_number(rejected.get("minimum_success_rate"), "upper success")
        < 0.97,
        "frontier upper bound",
    )
    require(
        point.get("accepted") is True
        and point.get("impairment_modes_completed") is True
        and point.get("source_lock_sha256")
        == HISTORICAL_SOURCE_LOCK_SHA256,
        "selected frontier evidence",
    )
    require(
        frozen.get("provider_count") == 1800
        and frozen.get("client_pool_size") == 200
        and frozen.get("arrivals_per_minute") == 80
        and frozen.get("quality_window_size") == 2
        and frozen.get("exchange_hosts") == 4
        and frozen.get("fleet_shards") == 4
        and frozen.get("duration_ms") == 180000,
        "frozen workload",
    )

    results = progress.get("results")
    require(
        progress.get("complete") is True
        and progress.get("completed_pairs") == SAME_SEED_TARGET
        and progress.get("target_pairs") == SAME_SEED_TARGET
        and isinstance(results, list)
        and len(results) == SAME_SEED_TARGET,
        "same-seed campaign is not terminal",
    )
    active_measurements = dict(progress)
    retained_measurements = dict(pre_repair_progress)
    active_measurements.pop("generated_at", None)
    retained_measurements.pop("generated_at", None)
    require(
        active_measurements == retained_measurements,
        "post-processing repair changed same-seed measurements",
    )
    strict_policy = placeability_policy.get("strict_policy")
    launch_policy = placeability_policy.get("launch_policy")
    retention = placeability_policy.get("retention")
    require(
        placeability_policy.get("kind")
        == "sim-latency-launch-readiness-placeability-policy-amendment"
        and placeability_policy.get("authorized") is True
        and placeability_policy.get("source_lock_sha256")
        == HISTORICAL_SOURCE_LOCK_SHA256
        and placeability_policy.get("same_seed_progress_sha256")
        == PRE_REPAIR_PROGRESS_SHA256
        and placeability_policy.get("strict_same_seed_analysis_sha256")
        == STRICT_SAME_ANALYSIS_SHA256
        and isinstance(strict_policy, dict)
        and strict_policy.get("passed") is False
        and strict_policy.get("eligible_replicate_counts") == []
        and isinstance(launch_policy, dict)
        and launch_policy.get("selected_replicates") == LAUNCH_REPLICATES
        and math.isclose(
            finite_number(
                launch_policy.get("minimum_probability"),
                "launch placeability target",
            ),
            LAUNCH_PLACEABILITY_TARGET,
            rel_tol=1e-12,
        )
        and math.isclose(
            finite_number(
                launch_policy.get("observed_bootstrap_probability"),
                "launch placeability observed",
            ),
            LAUNCH_PLACEABILITY_OBSERVED,
            rel_tol=1e-12,
        )
        and math.isclose(
            finite_number(
                launch_policy.get("takeover_margin"),
                "launch takeover margin",
            ),
            LAUNCH_TAKEOVER_MARGIN,
            rel_tol=1e-12,
        )
        and isinstance(retention, dict)
        and retention.get("all_twelve_same_seed_pairs_retained") is True
        and retention.get("strict_familywise_analysis_retained") is True
        and retention.get("measurement_rerun_required") is False
        and retention.get("nonplaceable_results_censored") is False,
        "placeability policy amendment",
    )
    require(
        strict_analysis.get("kind")
        == "sim-latency-launch-compromise-same-seed-analysis"
        and strict_analysis.get("decision_ready") is False
        and strict_analysis.get("replicate_count") == SAME_SEED_TARGET
        and strict_analysis.get("recommended_replicates") is None
        and strict_analysis.get("recommended_takeover_margin") is None
        and strict_analysis.get("progress_sha256")
        == PRE_REPAIR_PROGRESS_SHA256
        and strict_analysis.get("noop_placeable_pairs") == 8,
        "retained strict familywise analysis",
    )
    require(
        analysis.get("kind")
        == "sim-latency-launch-compromise-same-seed-analysis"
        and analysis.get("decision_ready") is True
        and analysis.get("replicate_count") == SAME_SEED_TARGET
        and analysis.get("progress_sha256") == sha256(SAME_PROGRESS),
        "same-seed analysis",
    )
    require(
        selection.get("accepted") is True
        and selection.get("source_lock_sha256")
        == HISTORICAL_SOURCE_LOCK_SHA256
        and selection.get("same_seed_pairs") == SAME_SEED_TARGET
        and selection.get("independent_seed_target") == PRIOR_INDEPENDENT_TARGET
        and selection.get("reference_required_passes") == PRIOR_REQUIRED_PASSES
        and selection.get("same_seed_progress_sha256") == sha256(SAME_PROGRESS)
        and selection.get("same_seed_analysis_sha256") == sha256(SAME_ANALYSIS),
        "final calibration selection",
    )
    require(
        selection_attestation.get("accepted") is True
        and selection_attestation.get("calibration_selection_sha256")
        == sha256(SELECTION)
        and selection_attestation.get("same_seed_analysis_sha256")
        == sha256(SAME_ANALYSIS),
        "same-seed selection attestation",
    )
    require(
        postprocessing_repair.get("kind")
        == "sim-latency-same-seed-postprocessing-repair"
        and postprocessing_repair.get("passed") is True
        and postprocessing_repair.get("source_lock_sha256")
        == HISTORICAL_SOURCE_LOCK_SHA256
        and postprocessing_repair.get("placeability_policy_amendment_sha256")
        == PLACEABILITY_POLICY_SHA256
        and postprocessing_repair.get("strict_familywise_analysis_sha256")
        == STRICT_SAME_ANALYSIS_SHA256
        and postprocessing_repair.get("retained_pre_repair_progress_sha256")
        == PRE_REPAIR_PROGRESS_SHA256
        and postprocessing_repair.get("terminal_progress_sha256")
        == sha256(SAME_PROGRESS)
        and postprocessing_repair.get("launch_analysis_sha256")
        == sha256(SAME_ANALYSIS)
        and postprocessing_repair.get("calibration_selection_sha256")
        == sha256(SELECTION)
        and postprocessing_repair.get("selection_attestation_sha256")
        == sha256(SELECTION_ATTESTATION)
        and postprocessing_repair.get("selected_replicates")
        == LAUNCH_REPLICATES
        and math.isclose(
            finite_number(
                postprocessing_repair.get("takeover_margin"),
                "repaired takeover margin",
            ),
            LAUNCH_TAKEOVER_MARGIN,
            rel_tol=1e-12,
        )
        and postprocessing_repair.get("measurements_rerun") is False
        and postprocessing_repair.get("measurements_censored") is False
        and postprocessing_repair.get("strict_analysis_retained") is True,
        "same-seed post-processing repair",
    )

    replicate_count = selection.get("replicate_count")
    margin = finite_number(selection.get("takeover_margin"), "takeover margin")
    baseline_summary = analysis.get("baseline_raw_score_ms")
    noop_summary = analysis.get("noop_raw_score_ms")
    require(
        replicate_count == LAUNCH_REPLICATES
        and analysis.get("recommended_replicates") == replicate_count
        and math.isclose(margin, LAUNCH_TAKEOVER_MARGIN, rel_tol=1e-12)
        and analysis.get("recommended_takeover_margin") == margin
        and analysis.get("noop_placeable_pairs") == 8
        and analysis.get("baseline_raw_score_ms")
        == strict_analysis.get("baseline_raw_score_ms")
        and analysis.get("noop_raw_score_ms")
        == strict_analysis.get("noop_raw_score_ms")
        and analysis.get("noop_over_baseline_ratio")
        == strict_analysis.get("noop_over_baseline_ratio")
        and isinstance(baseline_summary, dict)
        and isinstance(noop_summary, dict),
        "selected aggregation",
    )
    aggregation_options = analysis.get("aggregation_options")
    strict_options = strict_analysis.get("aggregation_options")
    require(
        isinstance(aggregation_options, list)
        and isinstance(strict_options, list)
        and [option.get("replicates") for option in aggregation_options]
        == [1, 3, 5, 7, 9]
        and [option.get("replicates") for option in strict_options]
        == [1, 3, 5, 7, 9],
        "aggregation option set",
    )
    selected_option = next(
        option
        for option in aggregation_options
        if option.get("replicates") == replicate_count
    )
    require(
        selected_option.get("selection_eligible") is True
        and selected_option.get("quality_gate_eligible") is True
        and selected_option.get("strict_familywise_quality_gate_eligible")
        is False
        and math.isclose(
            finite_number(
                selected_option.get(
                    "launch_single_evaluation_placeability_probability"
                ),
                "selected per-evaluation placeability",
            ),
            LAUNCH_PLACEABILITY_OBSERVED,
            rel_tol=1e-12,
        )
        and all(
            option.get("selection_eligible") is False
            for option in aggregation_options
            if option.get("replicates") != replicate_count
        )
        and all(
            option.get("selection_eligible") is False
            for option in strict_options
        ),
        "launch and strict placeability decisions",
    )
    baseline_mean = finite_number(baseline_summary.get("mean"), "baseline mean")
    threshold = finite_number(
        selection.get("baseline_mean_significantly_better_threshold_ms"),
        "baseline threshold",
    )
    require(
        math.isclose(threshold, baseline_mean * (1 - margin), rel_tol=1e-12),
        "baseline threshold does not match margin",
    )
    require(
        math.isclose(
            threshold,
            finite_number(
                launch_policy.get(
                    "baseline_mean_significantly_better_threshold_ms"
                ),
                "policy baseline threshold",
            ),
            rel_tol=1e-12,
        )
        and math.isclose(
            finite_number(
                launch_policy.get("estimated_false_rejection_probability"),
                "launch false rejection probability",
            ),
            1 - LAUNCH_PLACEABILITY_OBSERVED,
            rel_tol=1e-12,
        ),
        "launch policy arithmetic",
    )

    require(
        independent_progress.get("complete") is True
        and independent_progress.get("completed_independent_seeds")
        == INDEPENDENT_TARGET
        and independent_progress.get("target_independent_seeds")
        == INDEPENDENT_TARGET
        and independent_progress.get("replicates_per_reference") == 1
        and independent_progress.get("reference_ordering_passes", 0)
        >= INDEPENDENT_REQUIRED_PASSES
        and independent_progress.get("separability_passed") is True,
        "independent reference campaign",
    )
    exact_mode(INDEPENDENT_ATTESTATION_REPAIR, 0o400)
    require(
        independent_attestation_repair.get("kind")
        == "sim-latency-hidden-attestation-schema-postprocessing-repair"
        and independent_attestation_repair.get("source_lock_sha256")
        == HISTORICAL_SOURCE_LOCK_SHA256
        and independent_attestation_repair.get("repair_script_sha256")
        == sha256(INDEPENDENT_ATTESTATION_REPAIR_SCRIPT)
        and independent_attestation_repair.get("campaign_commitment_sha256")
        == sha256(INDEPENDENT_COMMITMENT)
        and independent_attestation_repair.get("terminal_decision_sha256")
        == sha256(INDEPENDENT_TERMINAL_DECISION)
        and independent_attestation_repair.get("attestation_sha256")
        == sha256(INDEPENDENT_ATTESTATION)
        and independent_attestation_repair.get("statistical_measurements_rerun")
        is False
        and independent_attestation_repair.get("measurements_censored") is False
        and independent_attestation_repair.get(
            "original_measurement_artifacts_changed"
        )
        is False,
        "independent attestation repair",
    )
    require(
        independent_attestation.get("accepted") is True
        and independent_attestation.get("target_independent_seeds")
        == INDEPENDENT_TARGET
        and independent_attestation.get("reference_required_passes")
        == INDEPENDENT_REQUIRED_PASSES
        and independent_attestation.get("campaign_progress_sha256")
        == sha256(INDEPENDENT_PROGRESS)
        and independent_attestation.get("campaign_commitment_sha256")
        == sha256(INDEPENDENT_COMMITMENT)
        and independent_attestation.get("terminal_decision_sha256")
        == sha256(INDEPENDENT_TERMINAL_DECISION)
        and independent_attestation.get("ordering_metric") == ORDERING_METRIC
        and independent_attestation.get(
            "one_designated_same_round_baseline_per_seed"
        )
        is True
        and independent_attestation.get("placeability_is_diagnostic_only") is True
        and independent_attestation.get("protocol_sha256")
        == sha256(INDEPENDENT_PROTOCOL)
        and independent_attestation.get("measurement_amendment_sha256")
        == sha256(INDEPENDENT_MEASUREMENT_AMENDMENT)
        and independent_attestation.get(
            "independent_reference_r1_correction_sha256"
        )
        == R1_CORRECTION_SHA256,
        "independent campaign attestation",
    )
    require(
        independent_commitment.get("target_independent_seeds")
        == INDEPENDENT_TARGET
        and independent_commitment.get("independent_reference_replicates") == 1
        and independent_commitment.get("calibration_decision_sha256")
        == sha256(INDEPENDENT_DECISION)
        and independent_commitment.get("measurement_amendment_sha256")
        == sha256(INDEPENDENT_MEASUREMENT_AMENDMENT)
        and independent_commitment.get(
            "independent_reference_r1_correction_sha256"
        )
        == R1_CORRECTION_SHA256
        and isinstance(independent_commitment.get("seeds"), list)
        and len(independent_commitment["seeds"]) == INDEPENDENT_TARGET,
        "independent seed commitment",
    )
    exact_mode(INDEPENDENT_DECISION, 0o400)
    exact_mode(INDEPENDENT_ANALYSIS, 0o400)
    require(
        independent_decision.get("decision_ready") is True
        and independent_decision.get("source_lock_sha256")
        == HISTORICAL_SOURCE_LOCK_SHA256
        and independent_decision.get("source_calibration_decision_sha256")
        == sha256(SAME_DECISION)
        and independent_decision.get("source_calibration_selection_sha256")
        == sha256(SELECTION)
        and independent_decision.get("same_seed_analysis_sha256")
        == sha256(INDEPENDENT_ANALYSIS)
        and independent_decision.get("replicates") == LAUNCH_REPLICATES
        and math.isclose(
            finite_number(
                independent_decision.get("takeover_margin"),
                "independent decision margin",
            ),
            LAUNCH_TAKEOVER_MARGIN,
            rel_tol=1e-12,
        )
        and independent_decision.get("independent_seed_target")
        == INDEPENDENT_TARGET
        and independent_decision.get("reference_required_passes")
        == INDEPENDENT_REQUIRED_PASSES
        and independent_analysis.get("compatibility_projection", {}).get(
            "source_analysis_sha256"
        )
        == sha256(SAME_ANALYSIS)
        and independent_analysis.get("compatibility_projection", {}).get(
            "projection_only"
        )
        is True,
        "independent calibration projection",
    )
    replacement = staging_reference_v5_amendment.get(
        "replacement_measurement_dependencies"
    )
    require(
        independent_terminal_decision.get("kind")
        == "sim-latency-reference-v5-hidden-launch-decision"
        and independent_terminal_decision.get("accepted") is True
        and independent_terminal_decision.get("campaign_exit_code") == 0
        and independent_terminal_decision.get("completed_independent_seeds")
        == INDEPENDENT_TARGET
        and independent_terminal_decision.get("reference_required_passes")
        == INDEPENDENT_REQUIRED_PASSES
        and independent_terminal_decision.get("reference_ordering_passes", 0)
        >= INDEPENDENT_REQUIRED_PASSES
        and independent_terminal_decision.get("ordering_metric") == ORDERING_METRIC
        and independent_terminal_decision.get("campaign_commitment_sha256")
        == sha256(INDEPENDENT_COMMITMENT)
        and independent_terminal_decision.get("protocol_sha256")
        == sha256(INDEPENDENT_PROTOCOL)
        and independent_terminal_decision.get("measurement_amendment_sha256")
        == sha256(INDEPENDENT_MEASUREMENT_AMENDMENT)
        and independent_terminal_decision.get("cleanup", {}).get(
            "residual_competition_containers"
        )
        == 0
        and independent_terminal_decision.get("cleanup", {}).get(
            "residual_competition_networks"
        )
        == 0,
        "independent terminal decision",
    )
    require(
        independent_protocol.get("kind")
        == "sim-latency-reference-v5-hidden-launch-protocol"
        and independent_protocol.get("draft") is False
        and independent_protocol.get("authorized") is True
        and independent_protocol.get("target_independent_seeds")
        == INDEPENDENT_TARGET
        and independent_protocol.get("reference_required_passes")
        == INDEPENDENT_REQUIRED_PASSES
        and independent_protocol.get("ordering_metric") == ORDERING_METRIC
        and independent_protocol.get("patch_sha256") == REFERENCE_PATCH_SHA256
        and reference_v5_qualification.get("kind")
        == "sim-latency-reference-v5-pilot-qualification"
        and reference_v5_qualification.get("draft") is False
        and reference_v5_qualification.get(
            "accepted_for_hidden_five_seed_screen"
        )
        is True,
        "reference-v5 protocol and qualification",
    )
    require(
        staging_reference_v5_amendment.get("kind")
        == "sim-latency-production-staging-reference-v5-amendment"
        and staging_reference_v5_amendment.get("draft") is False
        and staging_reference_v5_amendment.get("authorized") is True
        and staging_reference_v5_amendment.get(
            "original_production_staging_protocol_sha256"
        )
        == "6fc4a809779bf6e694ef3afa71522fa50d0512c56177b42da4249738a37dc7af"
        and staging_reference_v5_amendment.get(
            "hidden_campaign_attestation_sha256"
        )
        == sha256(INDEPENDENT_ATTESTATION)
        and staging_reference_v5_amendment.get("hidden_campaign_decision_sha256")
        == sha256(INDEPENDENT_TERMINAL_DECISION)
        and staging_reference_v5_amendment.get("hidden_campaign_protocol_sha256")
        == sha256(INDEPENDENT_PROTOCOL)
        and staging_reference_v5_amendment.get("hidden_attestation_repair_sha256")
        == sha256(INDEPENDENT_ATTESTATION_REPAIR)
        and staging_reference_v5_amendment.get(
            "hidden_attestation_repair_script_sha256"
        )
        == sha256(INDEPENDENT_ATTESTATION_REPAIR_SCRIPT)
        and staging_reference_v5_amendment.get(
            "reference_v5_qualification_sha256"
        )
        == sha256(REFERENCE_V5_QUALIFICATION)
        and isinstance(replacement, dict)
        and replacement.get("same_seed_pairs") == SAME_SEED_TARGET
        and replacement.get("independent_seeds") == INDEPENDENT_TARGET
        and replacement.get("required_reference_ordering_passes")
        == INDEPENDENT_REQUIRED_PASSES,
        "production staging reference-v5 amendment",
    )
    commitment_entries = independent_commitment["seeds"]
    require(
        all(isinstance(entry, dict) for entry in commitment_entries)
        and [entry.get("seed_index") for entry in commitment_entries]
        == list(range(1, INDEPENDENT_TARGET + 1)),
        "independent seed commitment order",
    )
    commitments = {
        int(entry["seed_index"]): entry for entry in commitment_entries
    }
    seed_results_sha256, seed_result_hashes = (
        independent_seed_results_digest()
    )
    reference_placeability_counts = {
        reference: 0 for reference in ("better", "noop", "worse")
    }
    expected_gates = {
        "G1_success",
        "G2_volume",
        "G3_path_integrity",
        "G4_matchmaking",
        "G5_stability",
        "G6_resources",
    }
    seed_results: list[dict[str, Any]] = []
    for index in range(1, INDEPENDENT_TARGET + 1):
        path = INDEPENDENT / f"seed-{index:02d}/seed-result.json"
        exact_mode(path, 0o400)
        result = load_object(path)
        references = result.get("references")
        commitment = commitments[index]
        reference_order = result.get("reference_order")
        designated = result.get("designated_baseline")
        require(
            result.get("schema") == 1
            and result.get("kind") == "sim-latency-independent-seed-result"
            and result.get("seed_index") == index
            and result.get("replicates_per_reference") == 1
            and result.get("round_id") == commitment.get("round_id")
            and result.get("seed_commitment")
            == commitment.get("seed_commitment")
            and result.get("providers_sha256")
            == commitment.get("providers_sha256")
            and result.get("calibration_decision_sha256")
            == sha256(INDEPENDENT_DECISION)
            and isinstance(reference_order, list)
            and len(reference_order) == 3
            and set(reference_order) == {"better", "noop", "worse"}
            and isinstance(designated, dict)
            and designated.get("reference") == reference_order[0]
            and isinstance(references, dict)
            and set(references) == {"better", "noop", "worse"},
            f"independent seed {index}",
        )
        ratios: dict[str, float] = {}
        baseline_raw_by_reference: dict[str, float] = {}
        for reference in ("better", "noop", "worse"):
            record = references[reference]
            require(
                isinstance(record, dict),
                f"seed {index} {reference} record",
            )
            relative = record.get("attempt_directory")
            require(
                isinstance(relative, str),
                f"seed {index} {reference} attempt path",
            )
            parts = Path(relative).parts
            require(
                len(parts) == 3
                and parts[0] == f"seed-{index:02d}"
                and parts[1] == f"reference-{reference}"
                and parts[2] in {"attempt-1", "attempt-2", "attempt-3"},
                f"seed {index} {reference} unsafe attempt path",
            )
            attempt_directory = INDEPENDENT
            for part in parts:
                attempt_directory /= part
                require(
                    not attempt_directory.is_symlink(),
                    f"seed {index} {reference} symlinked attempt path",
                )
            require(
                attempt_directory.is_dir(),
                f"seed {index} {reference} missing attempt directory",
            )
            worker_path = attempt_directory / "worker-result.json"
            manifest_path = attempt_directory / "evidence-manifest.json"
            baseline_path = attempt_directory / "baseline.json"
            for artifact in (worker_path, manifest_path, baseline_path):
                exact_mode(artifact, 0o400)
            require(
                record.get("worker_result_sha256") == sha256(worker_path)
                and record.get("evidence_manifest_sha256")
                == sha256(manifest_path)
                and record.get("patch_sha256")
                == REFERENCE_PATCH_SHA256[reference],
                f"seed {index} {reference} evidence hashes",
            )
            worker = load_object(worker_path)
            manifest = load_object(manifest_path)
            baseline = load_object(baseline_path)
            score = worker.get("score")
            security = worker.get("security")
            require(
                worker.get("schema") == 1
                and worker.get("eval_error") is None
                and isinstance(score, dict)
                and security_evidence_authenticated(security)
                and manifest.get("schema") == 1
                and manifest.get("kind") == "sim-latency-evidence-manifest"
                and manifest.get("job_id") == worker.get("job_id")
                and manifest.get("round_id") == result.get("round_id"),
                f"seed {index} {reference} worker security",
            )
            gates = score.get("gates")
            require(
                isinstance(gates, dict)
                and set(gates) == expected_gates
                and all(
                    isinstance(gate, dict)
                    and isinstance(gate.get("passed"), bool)
                    for gate in gates.values()
                ),
                f"seed {index} {reference} gates",
            )
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
            require(
                isinstance(placeable, bool)
                and isinstance(takeover_eligible, bool)
                and placeable == all(gate_passes.values())
                and record.get("placeable") is placeable
                and record.get("takeover_eligible") is takeover_eligible
                and record.get("gate_passes") == gate_passes
                and record.get("failed_gate_ids") == failed_gate_ids,
                f"seed {index} {reference} gate projection",
            )
            baseline_replicates = baseline.get("replicates")
            require(
                baseline.get("score_schema") == 1
                and baseline.get("kind") == "sim-latency-score-baseline"
                and baseline.get("round_id") == result.get("round_id")
                and baseline.get("config_sha256")
                == result.get("providers_sha256")
                and isinstance(baseline_replicates, list)
                and len(baseline_replicates) == 1
                and isinstance(baseline_replicates[0], dict),
                f"seed {index} {reference} baseline",
            )
            baseline_raw = finite_number(
                baseline_replicates[0].get("raw_score"),
                f"seed {index} {reference} baseline raw",
            )
            candidate_raw = finite_number(
                score.get("raw_score"),
                f"seed {index} {reference} candidate raw",
            )
            normalized = finite_number(
                score.get("normalized_score"),
                f"seed {index} {reference} normalized score",
            )
            ratio = finite_number(
                record.get("paired_ratio"),
                f"seed {index} {reference} ratio",
            )
            require(
                math.isclose(
                    finite_number(
                        record.get("baseline_raw_score_ms"),
                        f"seed {index} {reference} recorded baseline",
                    ),
                    baseline_raw,
                    rel_tol=1e-12,
                )
                and math.isclose(
                    finite_number(
                        record.get("candidate_raw_score_ms"),
                        f"seed {index} {reference} recorded candidate",
                    ),
                    candidate_raw,
                    rel_tol=1e-12,
                )
                and math.isclose(
                    finite_number(
                        record.get("normalized_score"),
                        f"seed {index} {reference} recorded normalized",
                    ),
                    normalized,
                    rel_tol=1e-12,
                )
                and math.isclose(
                    ratio, candidate_raw / baseline_raw, rel_tol=1e-12
                ),
                f"seed {index} {reference} score projection",
            )
            ratios[reference] = candidate_raw
            baseline_raw_by_reference[reference] = baseline_raw
            reference_placeability_counts[reference] += int(placeable)
        designated_reference = designated["reference"]
        require(
            math.isclose(
                finite_number(
                    designated.get("raw_score_ms"),
                    f"seed {index} designated baseline",
                ),
                baseline_raw_by_reference[designated_reference],
                rel_tol=1e-12,
            ),
            f"seed {index} designated baseline projection",
        )
        designated_raw = baseline_raw_by_reference[designated_reference]
        common_baseline_ratios = {
            reference: candidate_raw / designated_raw
            for reference, candidate_raw in ratios.items()
        }
        require(
            result.get("ordering_metric") == ORDERING_METRIC
            and result.get("ordering_passed")
            == (
                common_baseline_ratios["better"]
                < common_baseline_ratios["noop"]
                < common_baseline_ratios["worse"]
            ),
            f"seed {index} ordering",
        )
        seed_results.append(result)

    checks = readiness.get("checks")
    require(
        readiness.get("kind") == "sim-latency-production-readiness-final"
        and readiness.get("source_lock_sha256") == SOURCE_LOCK_SHA256
        and readiness.get("historical_calibration_source_lock_sha256")
        == HISTORICAL_SOURCE_LOCK_SHA256
        and readiness.get("evaluator_image_digest") == EVALUATOR_IMAGE
        and readiness.get("host_qualification_sha256")
        == HOST_QUALIFICATION_SHA256
        and readiness.get("same_seed_selection_sha256") == sha256(SELECTION)
        and readiness.get("placeability_policy_amendment_sha256")
        == PLACEABILITY_POLICY_SHA256
        and readiness.get("same_seed_postprocessing_repair_sha256")
        == POSTPROCESSING_REPAIR_SHA256
        and readiness.get("strict_same_seed_analysis_sha256")
        == STRICT_SAME_ANALYSIS_SHA256
        and readiness.get("pre_repair_same_seed_progress_sha256")
        == PRE_REPAIR_PROGRESS_SHA256
        and readiness.get(
            "production_staging_reference_v5_amendment_sha256"
        )
        == sha256(STAGING_REFERENCE_V5_AMENDMENT)
        and readiness.get(
            "production_staging_attempt_06_remediation_amendment_sha256"
        )
        == REMEDIATION_AMENDMENT_SHA256
        and readiness.get("independent_attestation_sha256")
        == sha256(INDEPENDENT_ATTESTATION)
        and readiness.get("independent_attestation_repair_sha256")
        == sha256(INDEPENDENT_ATTESTATION_REPAIR)
        and readiness.get("independent_attestation_repair_script_sha256")
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
        and isinstance(checks, dict)
        and len(checks) == 7
        and all(
            isinstance(check, dict)
            and check.get("passed") is True
            and isinstance(check.get("evidence_sha256"), str)
            for check in checks.values()
        ),
        "production readiness",
    )

    baseline_scores = [
        finite_number(result.get("baseline_raw_score"), "baseline result")
        for result in results
    ]
    noop_scores = [
        finite_number(result.get("candidate_raw_score"), "no-op result")
        for result in results
    ]
    require(
        math.isclose(
            statistics.fmean(baseline_scores), baseline_mean, rel_tol=1e-12
        )
        and math.isclose(
            statistics.fmean(noop_scores),
            finite_number(noop_summary.get("mean"), "no-op mean"),
            rel_tol=1e-12,
        ),
        "same-seed summary does not reproduce raw results",
    )

    return {
        "source_lock": source_lock,
        "season_base": season_base,
        "frontier": frontier,
        "point": point,
        "frozen": frozen,
        "progress": progress,
        "analysis": analysis,
        "strict_analysis": strict_analysis,
        "placeability_policy": placeability_policy,
        "postprocessing_repair": postprocessing_repair,
        "selection": selection,
        "independent_progress": independent_progress,
        "independent_attestation": independent_attestation,
        "seed_results": seed_results,
        "independent_seed_results_sha256": seed_results_sha256,
        "independent_seed_result_hashes": seed_result_hashes,
        "reference_placeability_counts": reference_placeability_counts,
        "readiness": readiness,
        "readiness_sha256": sha256(READINESS),
        "baseline_scores": baseline_scores,
        "noop_scores": noop_scores,
        "baseline_mean": baseline_mean,
        "noop_mean": finite_number(noop_summary.get("mean"), "no-op mean"),
        "threshold": threshold,
        "replicate_count": replicate_count,
        "margin": margin,
    }


def chart_coordinates(
    values: list[float],
    *,
    left: float,
    right: float,
    top: float,
    bottom: float,
    minimum: float,
    maximum: float,
) -> list[tuple[float, float]]:
    require(len(values) >= 2 and maximum > minimum, "invalid chart domain")
    x_step = (right - left) / (len(values) - 1)
    return [
        (
            left + index * x_step,
            bottom - (value - minimum) / (maximum - minimum) * (bottom - top),
        )
        for index, value in enumerate(values)
    ]


def point_string(points: list[tuple[float, float]]) -> str:
    return " ".join(f"{x:.1f},{y:.1f}" for x, y in points)


def same_seed_svg(data: dict[str, Any]) -> str:
    baseline = data["baseline_scores"]
    noop = data["noop_scores"]
    threshold = data["threshold"]
    baseline_mean = data["baseline_mean"]
    values = baseline + noop + [threshold, baseline_mean]
    span = max(values) - min(values)
    padding = max(1000.0, span * 0.12)
    minimum = min(values) - padding
    maximum = max(values) + padding
    baseline_points = chart_coordinates(
        baseline,
        left=70,
        right=850,
        top=48,
        bottom=275,
        minimum=minimum,
        maximum=maximum,
    )
    noop_points = chart_coordinates(
        noop,
        left=70,
        right=850,
        top=48,
        bottom=275,
        minimum=minimum,
        maximum=maximum,
    )

    def y(value: float) -> float:
        return 275 - (value - minimum) / (maximum - minimum) * 227

    dots = []
    labels = []
    for index, ((base_x, base_y), (noop_x, noop_y)) in enumerate(
        zip(baseline_points, noop_points, strict=True), start=1
    ):
        dots.append(
            f'<circle class="baseline-dot" cx="{base_x:.1f}" cy="{base_y:.1f}" r="4"/>'
        )
        dots.append(
            f'<circle class="noop-dot" cx="{noop_x:.1f}" cy="{noop_y:.1f}" r="4"/>'
        )
        labels.append(
            f'<text class="axis-label" x="{base_x:.1f}" y="302" text-anchor="middle">{index}</text>'
        )
    return f"""
      <svg viewBox="0 0 900 330" role="img" aria-label="Twelve same-seed baseline and no-op measurements with the significant-better threshold">
        <line class="axis" x1="70" y1="275" x2="850" y2="275"/>
        <line class="axis" x1="70" y1="48" x2="70" y2="275"/>
        <polyline class="baseline-series" data-baseline-id="same-seed-score" points="{point_string(baseline_points)}"/>
        <polyline class="noop-series" points="{point_string(noop_points)}"/>
        <line class="baseline-mean" data-baseline-id="same-seed-score" x1="70" y1="{y(baseline_mean):.1f}" x2="850" y2="{y(baseline_mean):.1f}"/>
        <text class="baseline-label" x="846" y="{y(baseline_mean) - 7:.1f}" text-anchor="end">baseline mean {fmt_ms(baseline_mean)}</text>
        <line class="threshold" data-threshold-for="same-seed-score" x1="70" y1="{y(threshold):.1f}" x2="850" y2="{y(threshold):.1f}"/>
        <text class="threshold-label" data-threshold-label-for="same-seed-score" x="846" y="{y(threshold) - 7:.1f}" text-anchor="end">significant-better threshold ≤ {fmt_ms(threshold)}</text>
        {''.join(dots)}
        {''.join(labels)}
        <text class="axis-title" x="460" y="324" text-anchor="middle">authenticated same-seed pair</text>
        <g transform="translate(86,66)"><circle class="baseline-dot" cx="0" cy="0" r="4"/><text class="legend" x="10" y="4">baseline</text><circle class="noop-dot" cx="92" cy="0" r="4"/><text class="legend" x="102" y="4">no-op</text></g>
      </svg>"""


def environment_svg(data: dict[str, Any]) -> str:
    selected_success = finite_number(
        data["point"].get("minimum_success_rate"), "selected success"
    )
    rejected_success = finite_number(
        data["frontier"]["rejected_upper_bound"].get("minimum_success_rate"),
        "rejected success",
    )
    return f"""
      <svg viewBox="0 0 900 330" role="img" aria-label="Frozen evaluation environment, workload, and measured frontier">
        <rect class="host-box" x="34" y="42" width="510" height="232" rx="18"/>
        <text class="visual-title" x="58" y="72">authoritative single host · 12 physical CPUs · 128 GiB</text>
        <rect class="eval-box" x="58" y="94" width="308" height="82" rx="12"/>
        <text class="box-title" x="78" y="123">EVALUATION BOUNDARY</text>
        <text class="box-value" x="78" y="151">10 cores · 72 GiB runner</text>
        <rect class="manage-box" x="382" y="94" width="138" height="82" rx="12"/>
        <text class="box-title" x="397" y="123">MANAGEMENT</text>
        <text class="box-value" x="397" y="151">2 cores · ≥24 GiB</text>
        <rect class="service-box" x="58" y="194" width="140" height="54" rx="10"/><text class="box-value" x="78" y="226">PostgreSQL · 16 GiB</text>
        <rect class="service-box" x="214" y="194" width="140" height="54" rx="10"/><text class="box-value" x="239" y="226">Redis · 8 GiB</text>
        <rect class="service-box" x="370" y="194" width="150" height="54" rx="10"/><text class="box-value" x="395" y="218">offline image</text><text class="small" x="395" y="237">local-only RO mounts</text>
        <line class="flow" x1="544" y1="158" x2="600" y2="158"/>
        <rect class="frontier-selected" x="616" y="57" width="248" height="92" rx="14"/>
        <text class="box-title" x="638" y="85">FROZEN p1800</text>
        <text class="box-value large" x="638" y="115">1,800 providers · 200 clients</text>
        <text class="small" x="638" y="136">80/min · q2 · 4 hosts · 4 shards · {100 * selected_success:.3f}% floor</text>
        <rect class="frontier-rejected" x="616" y="171" width="248" height="92" rx="14"/>
        <text class="box-title" x="638" y="199">UPPER BOUND p2700</text>
        <text class="box-value large" x="638" y="229">2,700 providers · 300 clients</text>
        <text class="small rejected" x="638" y="250">{100 * rejected_success:.4f}% &lt; 97% quality floor</text>
        <text class="axis-label" x="34" y="306">SMT/turbo off · performance governor · fixed IRQ affinity · management-controlled cleanup</text>
      </svg>"""


def independent_svg(data: dict[str, Any]) -> str:
    references = ("better", "noop", "worse")
    ratio_series = {
        reference: [
            finite_number(
                result["references"][reference]["candidate_raw_score_ms"],
                f"{reference} ratio",
            )
            / finite_number(
                result["designated_baseline"]["raw_score_ms"],
                "designated baseline ratio denominator",
            )
            for result in data["seed_results"]
        ]
        for reference in references
    }
    baseline_ratio = 1.0
    threshold_ratio = 1 - data["margin"]
    values = [baseline_ratio, threshold_ratio]
    for series in ratio_series.values():
        values.extend(series)
    span = max(values) - min(values)
    padding = max(0.04, span * 0.12)
    minimum = min(values) - padding
    maximum = max(values) + padding

    def y(value: float) -> float:
        return 275 - (value - minimum) / (maximum - minimum) * 215

    series_markup = []
    seed_labels = []
    for reference in references:
        points = chart_coordinates(
            ratio_series[reference],
            left=70,
            right=850,
            top=60,
            bottom=275,
            minimum=minimum,
            maximum=maximum,
        )
        series_markup.append(
            f'<polyline class="ratio-{reference}" points="{point_string(points)}"/>'
        )
        for x, point_y in points:
            series_markup.append(
                f'<circle class="ratio-dot {reference}" cx="{x:.1f}" cy="{point_y:.1f}" r="4"/>'
            )
    label_points = chart_coordinates(
        [baseline_ratio] * INDEPENDENT_TARGET,
        left=70,
        right=850,
        top=60,
        bottom=275,
        minimum=minimum,
        maximum=maximum,
    )
    for index, (x, _) in enumerate(label_points, start=1):
        seed_labels.append(
            f'<text class="axis-label" x="{x:.1f}" y="302" text-anchor="middle">{index}</text>'
        )
    return f"""
      <svg viewBox="0 0 900 330" role="img" aria-label="Independent-seed better, no-op, and worse designated-baseline ratios with baseline and threshold">
        <line class="axis" x1="70" y1="275" x2="850" y2="275"/>
        <line class="axis" x1="70" y1="60" x2="70" y2="275"/>
        <line class="baseline-mean" data-baseline-id="independent-designated-baseline-ratio" x1="70" y1="{y(baseline_ratio):.1f}" x2="850" y2="{y(baseline_ratio):.1f}"/>
        <text class="baseline-label" x="846" y="{y(baseline_ratio) - 7:.1f}" text-anchor="end">designated same-round baseline = 1.000</text>
        <line class="threshold" data-threshold-for="independent-designated-baseline-ratio" x1="70" y1="{y(threshold_ratio):.1f}" x2="850" y2="{y(threshold_ratio):.1f}"/>
        <text class="threshold-label" data-threshold-label-for="independent-designated-baseline-ratio" x="846" y="{y(threshold_ratio) - 7:.1f}" text-anchor="end">significant-better ratio ≤ {threshold_ratio:.3f}</text>
        {''.join(series_markup)}
        {''.join(seed_labels)}
        <text class="axis-title" x="460" y="324" text-anchor="middle">independent hidden seed · candidate ÷ one precommitted designated baseline</text>
        <g transform="translate(86,78)"><circle class="ratio-dot better" cx="0" cy="0" r="4"/><text class="legend" x="10" y="4">better</text><circle class="ratio-dot noop" cx="76" cy="0" r="4"/><text class="legend" x="86" y="4">no-op</text><circle class="ratio-dot worse" cx="150" cy="0" r="4"/><text class="legend" x="160" y="4">worse</text></g>
      </svg>"""


def readiness_svg(data: dict[str, Any]) -> str:
    labels = [
        ("SOURCE", "9 repos"),
        ("FRONTIER", "p1800"),
        ("NOISE", "12 pairs"),
        ("REFERENCES", "≥4/5"),
        ("RELEASE", "OCI + SBOM"),
        ("STAGING", "failover"),
        ("SECURITY", "7/7 gates"),
    ]
    nodes = []
    arrows = []
    for index, (label, detail) in enumerate(labels):
        x = 28 + index * 124
        nodes.append(
            f'<rect class="ready-node" x="{x}" y="88" width="104" height="112" rx="14"/>'
            f'<circle class="check" cx="{x + 52}" cy="117" r="13"/>'
            f'<path class="check-mark" d="M{x + 45} 117 l5 5 l10 -12"/>'
            f'<text class="node-label" x="{x + 52}" y="153" text-anchor="middle">{label}</text>'
            f'<text class="node-detail" x="{x + 52}" y="177" text-anchor="middle">{detail}</text>'
        )
        if index < len(labels) - 1:
            arrows.append(
                f'<line class="ready-arrow" x1="{x + 104}" y1="144" x2="{x + 124}" y2="144"/>'
            )
    sealed_at = esc(data["readiness"].get("sealed_at"))
    return f"""
      <svg viewBox="0 0 900 300" role="img" aria-label="Authenticated production finalization chain">
        <text class="visual-title" x="28" y="45">LOCAL TECHNICAL LAUNCH CHAIN · AUTHENTICATED END TO END</text>
        {''.join(arrows)}
        {''.join(nodes)}
        <rect class="ready-banner" x="28" y="224" width="848" height="46" rx="12"/>
        <text class="ready-banner-text" x="452" y="253" text-anchor="middle">sealed {sealed_at} · cache, FIFO, recovery, containment, retention, and secret boundaries verified</text>
      </svg>"""


def metric(label: str, value: str) -> str:
    return (
        '<div class="metric"><strong>'
        + esc(value)
        + "</strong><span>"
        + esc(label)
        + "</span></div>"
    )


def render_html(data: dict[str, Any]) -> str:
    analysis = data["analysis"]
    selection = data["selection"]
    independent = data["independent_progress"]
    readiness = data["readiness"]
    baseline_stats = analysis["baseline_raw_score_ms"]
    pass_count = independent["reference_ordering_passes"]
    failed = independent.get("failed_ordering_seed_indices", [])
    placeable = int(analysis["noop_placeable_pairs"])
    nonplaceable = SAME_SEED_TARGET - placeable
    selected_option = next(
        option
        for option in analysis["aggregation_options"]
        if option["replicates"] == data["replicate_count"]
    )
    launch_placeability = finite_number(
        selected_option["launch_single_evaluation_placeability_probability"],
        "launch placeability",
    )
    familywise_placeability = finite_number(
        selected_option[
            "estimated_at_least_11_of_12_noop_placeability_probability"
        ],
        "familywise placeability",
    )
    failed_text = "none" if not failed else ", ".join(str(value) for value in failed)
    reference_placeability = data["reference_placeability_counts"]
    reference_placeability_text = " · ".join(
        f"{reference} {reference_placeability[reference]}/{INDEPENDENT_TARGET}"
        for reference in ("better", "noop", "worse")
    )
    sealed_at = readiness.get("sealed_at", "authenticated final seal")
    generated_at = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    report = f"""<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>Sim-latency competition finalization</title>
  <style>
    :root{{--ink:#122033;--muted:#617087;--paper:#f3f6fa;--card:#fff;--line:#d8e0ea;--navy:#173f70;--cyan:#17a9c5;--green:#15805d;--lime:#b6df55;--amber:#d78818;--red:#ca3d4c;--violet:#7357b7}}
    *{{box-sizing:border-box}} body{{margin:0;background:var(--paper);color:var(--ink);font:15px/1.55 Inter,ui-sans-serif,system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}} main{{max-width:1120px;margin:0 auto;padding:56px 28px 80px}} h1{{font-size:clamp(36px,6vw,68px);line-height:1.02;letter-spacing:-.045em;margin:10px 0 18px;max-width:900px}} h2{{font-size:29px;line-height:1.15;letter-spacing:-.025em;margin:4px 0 8px}} p{{color:var(--muted);max-width:780px}} .eyebrow,.index{{font-size:12px;font-weight:850;letter-spacing:.14em;text-transform:uppercase;color:var(--cyan)}} .summary{{font-size:18px;max-width:850px}} .stamp{{display:flex;flex-wrap:wrap;gap:8px;margin:24px 0 44px}} .pill{{border:1px solid var(--line);background:#fff;border-radius:999px;padding:7px 12px;font-size:12px;font-weight:750}} .pill.ready{{background:#dff5eb;border-color:#98d5bc;color:#075c40}} section{{background:var(--card);border:1px solid var(--line);border-radius:22px;padding:30px;margin:24px 0;box-shadow:0 10px 28px rgba(27,55,87,.055)}} .section-head{{display:flex;align-items:flex-start;justify-content:space-between;gap:24px;margin-bottom:20px}} .section-head>strong{{font-size:18px;color:var(--navy);white-space:nowrap}} svg{{width:100%;height:auto;background:#f9fbfd;border:1px solid var(--line);border-radius:15px}} svg text{{font-family:inherit}} .axis{{stroke:#a9b6c6;stroke-width:1}} .axis-label,.small{{fill:var(--muted);font-size:11px}} .axis-title{{fill:var(--muted);font-size:12px;font-weight:700}} .baseline-series{{fill:none;stroke:var(--navy);stroke-width:2.5}} .noop-series{{fill:none;stroke:var(--cyan);stroke-width:2.5}} .baseline-dot{{fill:var(--navy)}} .noop-dot{{fill:var(--cyan)}} .baseline-mean{{stroke:var(--navy);stroke-width:2;stroke-dasharray:3 4}} .baseline-label{{fill:var(--navy);font-size:11px;font-weight:800}} .threshold{{stroke:var(--red);stroke-width:2.5;stroke-dasharray:8 5}} .threshold-label{{fill:var(--red);font-size:11px;font-weight:850}} .legend{{fill:var(--muted);font-size:11px;font-weight:700}} .metrics{{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:12px;margin-top:16px}} .metric{{padding:15px;border:1px solid var(--line);border-radius:13px;background:#fbfcfe}} .metric strong{{display:block;color:var(--navy);font-size:17px;overflow-wrap:anywhere}} .metric span{{display:block;color:var(--muted);font-size:12px;margin-top:4px}} .host-box{{fill:#eef3f9;stroke:#b9c8d9}} .eval-box{{fill:#e7f3fb;stroke:#86c4d5}} .manage-box{{fill:#e5f5ee;stroke:#8ac6aa}} .service-box{{fill:#fff;stroke:#c8d4e1}} .box-title{{fill:var(--navy);font-size:11px;font-weight:850;letter-spacing:.09em}} .box-value{{fill:var(--ink);font-size:12px;font-weight:700}} .box-value.large{{font-size:14px}} .visual-title{{fill:var(--navy);font-size:13px;font-weight:850;letter-spacing:.04em}} .flow,.ready-arrow{{stroke:#98a9bc;stroke-width:2}} .frontier-selected{{fill:#e4f6ed;stroke:#69b78f;stroke-width:2}} .frontier-rejected{{fill:#fff1ee;stroke:#df948d}} .rejected{{fill:var(--red)}} .ratio-better{{fill:none;stroke:var(--green);stroke-width:2.2}} .ratio-noop{{fill:none;stroke:var(--violet);stroke-width:2.2}} .ratio-worse{{fill:none;stroke:var(--amber);stroke-width:2.2}} .ratio-dot.better{{fill:var(--green)}} .ratio-dot.noop{{fill:var(--violet)}} .ratio-dot.worse{{fill:var(--amber)}} .ready-node{{fill:#fff;stroke:#a9cdbd}} .check{{fill:var(--green)}} .check-mark{{fill:none;stroke:#fff;stroke-width:3;stroke-linecap:round;stroke-linejoin:round}} .node-label{{fill:var(--navy);font-size:10px;font-weight:850}} .node-detail{{fill:var(--muted);font-size:10px}} .ready-banner{{fill:#dcf4e8;stroke:#8bcbae}} .ready-banner-text{{fill:#075c40;font-size:12px;font-weight:800}} .disclosure{{margin-top:16px;padding:14px 16px;border-left:4px solid var(--amber);background:#fff8e9;color:#785114;font-size:13px}} footer{{color:var(--muted);font-size:12px;margin-top:28px}} code{{font-size:12px;overflow-wrap:anywhere}}
    @media(max-width:760px){{main{{padding:30px 14px 56px}}section{{padding:20px}}.section-head{{display:block}}.section-head>strong{{display:block;margin-top:12px}}.metrics{{grid-template-columns:repeat(2,minmax(0,1fr))}}}}
  </style>
</head>
<body>
<main>
  <header>
    <div class="eyebrow">URnetwork · sim-latency · final evidence</div>
    <h1>The competition evaluator is technically launch-ready.</h1>
    <p class="summary">The final single-host environment, p1800 workload, calibrated scoring policy, independent reference separation, and production control plane have completed their authenticated local gates.</p>
    <div class="stamp"><span class="pill ready">LOCAL TECHNICAL GATE OPEN</span><span class="pill">sealed {esc(sealed_at)}</span><span class="pill">score schema 1</span><span class="pill">source {compact_sha(SOURCE_LOCK_SHA256)}</span></div>
  </header>

  <section id="baseline-stability">
    <div class="section-head"><div><div class="index">01 · BASELINE &amp; THRESHOLD</div><h2>Twelve uncensored pairs freeze the scoring line</h2><p>All complete outcomes remain in the noise population, including {nonplaceable} non-placeable no-op draws. The original familywise placeability rule produced no eligible replicate count. The authorized launch rule instead controls one production evaluation and selects R={data['replicate_count']} at {fmt_pct(launch_placeability)} placeability against a {fmt_pct(LAUNCH_PLACEABILITY_TARGET)} floor.</p></div><strong>{fmt_ms(data['baseline_mean'])}</strong></div>
    {same_seed_svg(data)}
    <div class="metrics">
      {metric('baseline mean · significant-better line ' + fmt_ms(data['threshold']), fmt_ms(data['baseline_mean']))}
      {metric('sample SD · CV ' + fmt_pct(finite_number(baseline_stats['cv'], 'cv')), fmt_ms(finite_number(baseline_stats['sample_sd'], 'sd')))}
      {metric('paired no-op mean', fmt_ms(data['noop_mean']))}
      {metric(f"single-evaluation placeability {fmt_pct(launch_placeability)} ≥ {fmt_pct(LAUNCH_PLACEABILITY_TARGET)} · strict familywise {fmt_pct(familywise_placeability)} < 95.000%", f"R={data['replicate_count']} · {fmt_pct(data['margin'])} margin")}
    </div>
  </section>

  <section id="environment-scale">
    <div class="section-head"><div><div class="index">02 · ENVIRONMENT &amp; SCALE</div><h2>One bounded host at the measured frontier</h2><p>The final evaluator exposes ten physical cores to the complete untrusted stack and reserves two physical cores plus management memory for orchestration and cleanup. The exact-image impairment on/off sweep accepted p1800 and rejected p2700 at the 97% success floor.</p></div><strong>1,800 providers</strong></div>
    {environment_svg(data)}
    <div class="metrics">
      {metric('frozen workload', '1,800 providers · 200 clients')}
      {metric('load and topology', '80/min · q2 · 4×4')}
      {metric('measurement window · idle timeout 5 s', '3 minutes')}
      {metric('evaluator image', compact_sha(EVALUATOR_IMAGE))}
    </div>
  </section>

  <section id="reference-separability">
    <div class="section-head"><div><div class="index">03 · INDEPENDENT REFERENCES</div><h2>Better, no-op, and worse rank across hidden seeds</h2><p>Each reference used one candidate replicate on five independently generated and precommitted seeds. Within each seed, all candidates are divided by the one pristine baseline selected by the precommitted randomized reference order; lower is better. Placeability is a separate diagnostic and does not alter the ordering result. The authorized launch screen requires at least four of five correctly ordered seeds.</p></div><strong>{pass_count}/{INDEPENDENT_TARGET} ordered</strong></div>
    {independent_svg(data)}
    <div class="metrics">
      {metric('required reference gate', f'{pass_count}/{INDEPENDENT_TARGET} pass')}
      {metric('failed ordering seed indices', failed_text)}
      {metric('reference / competition aggregation', f"R=1 / median of {data['replicate_count']}")}
      {metric('reference gate placeability', reference_placeability_text)}
    </div>
    <div class="disclosure">Launch compromise disclosure: the authorized design retains all 12 same-seed pairs and uses five fresh independent seeds with a 4/5 ordering gate, replacing the superseded 12-seed/11-pass compromise and the original 20-seed/19-pass design. It is explicitly not confidence-equivalent to either larger protocol. Reference placeability counts and private paired ratios are diagnostic only; neither can censor the shared-baseline ordering result.</div>
  </section>

  <section id="production-readiness">
    <div class="section-head"><div><div class="index">04 · COMPETITION STATUS</div><h2>The secure evaluator and scoring service passed staging</h2><p>The digest-pinned API and worker release passed authenticated generate/submit/poll, FIFO and cache behavior, same-round calibration, worker failover, adversarial cleanup, immutable accounting, artifact retention, default-deny networking, and the local-only secret boundary.</p></div><strong>7/7 production gates</strong></div>
    {readiness_svg(data)}
    <div class="metrics">
      {metric('production readiness checks', '7 / 7 passed')}
      {metric('control-plane commit', compact_sha(str(readiness['control_plane_commit'])))}
      {metric('public patch base', f'{PUBLIC_AUTHORING_TAG} · {compact_sha(PUBLIC_AUTHORING_COMMIT)}')}
      {metric('evaluator source', compact_sha(BASE_SHA))}
      {metric('host qualification', compact_sha(HOST_QUALIFICATION_SHA256))}
    </div>
  </section>

  <footer>Generated {generated_at} from content-addressed local evidence. Public patch tag <code>{PUBLIC_AUTHORING_TAG}</code> at <code>{PUBLIC_AUTHORING_COMMIT}</code>; authoritative evaluator commit <code>{BASE_SHA}</code>; simulator <code>{SIMULATOR_SHA256}</code>.</footer>
</main>
</body>
</html>
"""
    validate_report_shape(report)
    return report


def render_preview_html(data: dict[str, Any]) -> str:
    preview = render_html(data)
    replacements = (
        (
            "<title>Sim-latency competition finalization</title>",
            "<title>Final sim-latency evaluation environment preview</title>",
        ),
        (
            "URnetwork · sim-latency · final evidence",
            "URnetwork · sim-latency · shareable final preview",
        ),
        (
            "<h1>The competition evaluator is technically launch-ready.</h1>",
            "<h1>Final evaluation environment: launch-ready preview.</h1>",
        ),
    )
    for old, new in replacements:
        require(preview.count(old) == 1, f"preview template marker changed: {old}")
        preview = preview.replace(old, new, 1)
    validate_report_shape(preview)
    return preview


def validate_report_shape(report: str) -> ReportShapeParser:
    parser = ReportShapeParser()
    parser.feed(report)
    require(parser.section_depth == 0, "unbalanced report sections")
    require(
        parser.section_visuals == [1, 1, 1, 1],
        "report must contain one SVG in each of four sections",
    )
    require(bool(parser.baseline_ids), "report contains no baseline mapping")
    require(
        parser.baseline_ids
        == parser.threshold_line_ids
        == parser.threshold_label_ids,
        "every baseline needs one threshold line and label mapping",
    )
    lowered = report.lower()
    require("preview-only" not in lowered and "pending" not in lowered, "draft language")
    return parser


def render_calibration_document(data: dict[str, Any]) -> str:
    analysis = data["analysis"]
    point = data["point"]
    readiness = data["readiness"]
    selection = data["selection"]
    baseline = analysis["baseline_raw_score_ms"]
    noop = analysis["noop_raw_score_ms"]
    ratio = analysis["noop_over_baseline_ratio"]
    options = analysis["aggregation_options"]
    independent = data["independent_progress"]

    option_rows = []
    for option in options:
        option_rows.append(
            "| {replicates} | {cv:.3f}% | {minimum:.3f}% | {single:.3f}% | {familywise:.3f}% | {strict} | {launch} |".format(
                replicates=option["replicates"],
                cv=100 * finite_number(option["cv"], "aggregation cv"),
                minimum=100
                * finite_number(option["minimum_margin"], "minimum margin"),
                single=100
                * finite_number(
                    option[
                        "launch_single_evaluation_placeability_probability"
                    ],
                    "single-evaluation placeability",
                ),
                familywise=100
                * finite_number(
                    option[
                        "estimated_at_least_11_of_12_noop_placeability_probability"
                    ],
                    "familywise placeability",
                ),
                strict=(
                    "yes"
                    if option.get("strict_familywise_quality_gate_eligible")
                    is True
                    else "no"
                ),
                launch=(
                    "yes" if option.get("selection_eligible") is True else "no"
                ),
            )
        )

    pair_rows = []
    for index, result in enumerate(data["progress"]["results"], start=1):
        gates = ", ".join(result.get("failed_gate_ids", [])) or "all passed"
        pair_rows.append(
            f"| {index} | {finite_number(result['baseline_raw_score'], 'baseline'):.3f} | "
            f"{finite_number(result['candidate_raw_score'], 'candidate'):.3f} | "
            f"{finite_number(result['candidate_to_baseline_ratio'], 'ratio'):.6f} | "
            f"{'yes' if result.get('placeable') is True else 'no'} | {gates} |"
        )

    seed_rows = []
    for result in data["seed_results"]:
        refs = result["references"]
        designated_raw = finite_number(
            result["designated_baseline"]["raw_score_ms"],
            "designated baseline",
        )
        seed_rows.append(
            f"| {result['seed_index']} | "
            f"{finite_number(refs['better']['candidate_raw_score_ms'], 'better') / designated_raw:.6f} | "
            f"{finite_number(refs['noop']['candidate_raw_score_ms'], 'noop') / designated_raw:.6f} | "
            f"{finite_number(refs['worse']['candidate_raw_score_ms'], 'worse') / designated_raw:.6f} | "
            f"{'yes' if result.get('ordering_passed') is True else 'no'} |"
        )

    checks = "\n".join(
        f"- `{check_id}`: passed; evidence `{record['evidence_sha256']}`"
        for check_id, record in sorted(readiness["checks"].items())
    )
    repositories = "\n".join(
        f"- `{name}`: `{commit}`"
        for name, commit in sorted(data["source_lock"]["repositories"].items())
    )
    selected_option = next(
        option
        for option in options
        if option["replicates"] == data["replicate_count"]
    )
    generated_at = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    peak_gib = finite_number(point["max_peak_rss_bytes"], "peak RSS") / 2**30
    mean_cpu = finite_number(point["max_mean_cpu_cores"], "mean CPU")
    source_lock_link = (
        "eval-12c/final-calibration-p1800-cf0fd3a9/source-lock.json"
    )
    season_base_link = (
        "eval-12c/final-calibration-p1800-cf0fd3a9/"
        "season-base-equivalence.json"
    )
    progress_link = (
        "eval-12c/final-calibration-p1800-cf0fd3a9/post-frontier/"
        "p1800-c200-r80-q2/same-seed/progress.json"
    )
    analysis_link = (
        "eval-12c/final-calibration-p1800-cf0fd3a9/post-frontier/"
        "p1800-c200-r80-q2/same-seed-analysis.json"
    )
    strict_analysis_link = (
        "eval-12c/final-calibration-p1800-cf0fd3a9/post-frontier/"
        "p1800-c200-r80-q2/same-seed-analysis-familywise.json"
    )
    placeability_policy_link = (
        "eval-12c/final-calibration-p1800-cf0fd3a9/"
        "launch-readiness-placeability-policy-amendment.json"
    )
    postprocessing_repair_link = (
        "eval-12c/final-calibration-p1800-cf0fd3a9/"
        "same-seed-postprocessing-repair.json"
    )
    independent_link = (
        "eval-12c/final-calibration-p1800-cf0fd3a9/"
        "reference-requalification-v5/hidden-launch-runtime/"
        "independent-references/progress.json"
    )
    readiness_link = (
        "eval-12c/final-calibration-p1800-cf0fd3a9/production-readiness-final.json"
    )
    return f"""# Apex production calibration

Status: **LOCALLY QUALIFIED — technical launch gate open**

Generated: `{generated_at}`  
Score schema: `1`  
Source lock: `{SOURCE_LOCK_SHA256}`
Historical calibration source lock: `{HISTORICAL_SOURCE_LOCK_SHA256}`
Attempt-06 remediation amendment: `{REMEDIATION_AMENDMENT_SHA256}`
Season-base equivalence: `{SEASON_BASE_EQUIVALENCE_SHA256}`

This is the terminal local calibration for the sim-latency competition. It
binds the selected workload, baseline noise, takeover policy, independent-seed
reference separability, resource boundary, and production staging evidence to
the frozen source and evaluator identities. Organizational activation and
on-call ownership are separate operational decisions.

## Frozen identity and environment

- Public patch-authoring base: `{PUBLIC_AUTHORING_TAG}` at
  `{PUBLIC_AUTHORING_COMMIT}`
- Entire editable surface: `connect/resident_contract_manager.go`, Git blob
  `{EDITABLE_BLOB}` at both the public tag and evaluator commit
- Authoritative evaluator source commit: `{BASE_SHA}`
- Evaluator image: `{EVALUATOR_IMAGE}`
- Simulator and scorer SHA-256: `{SIMULATOR_SHA256}`
- Host qualification SHA-256: `{HOST_QUALIFICATION_SHA256}`
- The p1800 frontier, baseline-noise, and reference-separability evidence was
  measured under historical source lock `{HISTORICAL_SOURCE_LOCK_SHA256}`. The
  authorized correctness remediation `{REMEDIATION_AMENDMENT_SHA256}` binds it
  to this evaluator through a clean same-round R=9 baseline/no-op bridge.
- Host: one authoritative 12-physical-core, 128 GiB machine; SMT and turbo off;
  performance governor; fixed affinity and IRQ placement.
- Evaluation boundary: physical CPUs `0,2,4,6,8,10,12,14,16,18`, 72 GiB runner
  ceiling, PostgreSQL 16 GiB, and Redis 8 GiB.
- Management reserve: physical CPUs `20,22` and at least 24 GiB, outside the
  untrusted job, retained for orchestration and forced cleanup.
- Candidate containers receive only direct read-only `config/local` and
  `vault/local` leaf mounts. Parent, `all`, `main`, control credentials, Docker
  socket, and external networking are unavailable.

Authoritative source-lock record: [`{source_lock_link}`]({source_lock_link}).
Public-tag/evaluator equivalence record:
[`{season_base_link}`]({season_base_link}).

### Frozen repository commits

{repositories}

## Frontier and selected workload

The exact evaluator image completed impairment-on and impairment-off runs. The
largest accepted point is p1800; p2700 is the first authenticated upper-bound
rejection because its minimum success rate was
`{100 * finite_number(data['frontier']['rejected_upper_bound']['minimum_success_rate'], 'rejected success'):.4f}%`,
below the 97% floor.

| Field | Accepted value |
|---|---:|
| providers | 1,800 |
| warm clients | 200 |
| arrivals per minute | 80 |
| multi-client quality window | 2 |
| exchange hosts / fleet shards | 4 / 4 |
| measured duration | 180 seconds |
| forward idle timeout | 5 seconds |
| client warmup timeout ceiling | 1,200 seconds |
| maximum frontier mean CPU | {mean_cpu:.3f} of 10 evaluation cores |
| maximum frontier peak RSS | {peak_gib:.3f} GiB |
| minimum accepted success rate | {100 * finite_number(point['minimum_success_rate'], 'minimum success'):.3f}% |

The 1,200-second client warmup ceiling accommodates cold template restoration,
service readiness, and worst-case client construction without charging an
infrastructure delay as candidate latency. The worker's 8,000-second score
timeout covers offline build, reset, warmup, `R={data['replicate_count']}`
baseline/candidate repetitions, scoring, hashing, TERM grace, and cleanup while
remaining bounded and killable from the two management cores.

## Same-seed baseline and takeover selection

All 12 complete pairs are retained without censoring. The baseline mean is
`{fmt_ms(data['baseline_mean'])}`; the paired no-op mean is
`{fmt_ms(data['noop_mean'])}`. Baseline sample SD is
`{fmt_ms(finite_number(baseline['sample_sd'], 'baseline SD'))}`, CV is
`{fmt_pct(finite_number(baseline['cv'], 'baseline CV'))}`, median is
`{fmt_ms(finite_number(baseline['median'], 'baseline median'))}`, and the range
is `{fmt_ms(finite_number(baseline['min'], 'baseline minimum'))}` to
`{fmt_ms(finite_number(baseline['max'], 'baseline maximum'))}`.

The selected candidate aggregation is the type-7 median of
`R={data['replicate_count']}` repetitions. The takeover margin and minimum
detectable relative improvement are `{fmt_pct(data['margin'])}`. A submission
must have an aggregate raw score at or below its same-round baseline times
`{1 - data['margin']:.3f}` and pass G1–G6. At the observed baseline mean, the
significant-better threshold is `{fmt_ms(data['threshold'])}`. The selected
bootstrap distribution has CV
`{fmt_pct(finite_number(selected_option['cv'], 'selected CV'))}` and minimum
supported margin
`{fmt_pct(finite_number(selected_option['minimum_margin'], 'selected margin'))}`.

Paired no-op ratio mean is
`{finite_number(ratio['mean'], 'paired ratio mean'):.6f}` and median is
`{finite_number(ratio['median'], 'paired ratio median'):.6f}`. Exactly
`{analysis['noop_placeable_pairs']}/12` no-op draws were placeable; the
`{SAME_SEED_TARGET - int(analysis['noop_placeable_pairs'])}` non-placeable complete draws
remain in every noise and quality calculation.

Raw evidence: [`{progress_link}`]({progress_link}),
[`{analysis_link}`]({analysis_link}), retained strict analysis
[`{strict_analysis_link}`]({strict_analysis_link}), authorized policy
[`{placeability_policy_link}`]({placeability_policy_link}), and authenticated
post-processing repair [`{postprocessing_repair_link}`]({postprocessing_repair_link}).

| Pair | baseline ms | no-op ms | no-op / baseline | placeable | failed gates |
|---:|---:|---:|---:|:---:|---|
{chr(10).join(pair_rows)}

### Aggregation candidates

The original familywise rule required run noise no greater than one quarter of
the takeover margin and at least a 95% estimated probability that 11 of 12
independent no-op results would be placeable. It failed for every candidate;
at R=9, the estimate was
`{fmt_pct(finite_number(selected_option['estimated_at_least_11_of_12_noop_placeability_probability'], 'selected familywise placeability'))}`.
The authorized launch amendment instead requires at least
`{fmt_pct(LAUNCH_PLACEABILITY_TARGET)}` estimated placeability for one
production evaluation. R=9 passes at
`{fmt_pct(finite_number(selected_option['launch_single_evaluation_placeability_probability'], 'selected launch placeability'))}`,
an estimated false-rejection probability of
`{fmt_pct(1 - finite_number(selected_option['launch_single_evaluation_placeability_probability'], 'selected launch placeability'))}`.
This is an explicit launch compromise and is not confidence-equivalent to the
strict familywise rule.

| R | bootstrap CV | minimum margin | P(single placeable) | P(at least 11/12 placeable) | strict eligible | launch eligible |
|---:|---:|---:|---:|---:|:---:|:---:|
{chr(10).join(option_rows)}

## Independent seeds and reference separability

Five CSPRNG seeds were committed before the first reference result and
revealed only after the campaign. Each seed ran the pinned better, no-op, and
worse patches in a precommitted randomized order with one candidate replicate
per reference. All three candidate raw scores within a seed use the same
precommitted designated pristine baseline denominator. The ordering gate passed
`{independent['reference_ordering_passes']}/{INDEPENDENT_TARGET}`, satisfying the required
`{INDEPENDENT_REQUIRED_PASSES}/{INDEPENDENT_TARGET}` reference separability threshold.

This launch compromise is not confidence-equivalent to the original design:
it retains 12 same-seed pairs and uses five independent seeds with a 4/5
ordering gate rather than the superseded 12-seed/11-pass compromise or the
original 20-seed/19-pass design. It preserves all complete and non-placeable
outcomes, uses fresh hidden independent-seed material, and leaves the calibrated
competition `R={data['replicate_count']}` and takeover margin unchanged.

Raw evidence: [`{independent_link}`]({independent_link}).

| Seed | better / designated baseline | no-op / designated baseline | worse / designated baseline | ordered |
|---:|---:|---:|---:|:---:|
{chr(10).join(seed_rows)}

## Production readiness and resource controls

The digest-pinned static API, worker, rebaseline, and migration binaries have
provenance and SBOM records. Service-backed staging verified authenticated
generate/submit/poll, origin-before-local migrations, single-job FIFO, cache
identity across principals, same-round rebaseline, terminal immutability,
worker lease recovery, submission retry, reveal commitment, provider download,
artifact retention, and default-deny networking. Adversarial CPU and memory
bombs remained killable; cleanup is issued from the management reserve.

The seven sealed production records are:

{checks}

Final readiness evidence: [`{readiness_link}`]({readiness_link}).

## Final signed selection

| Field | Accepted value |
|---|---|
| provider/client/arrival scale | 1,800 / 200 / 80 per minute |
| measured duration | 180 seconds |
| hosts / fleet shards / quality window | 4 / 4 / 2 |
| baseline and candidate replicates | median of {data['replicate_count']} |
| takeover margin / MDD | {fmt_pct(data['margin'])} |
| observed baseline mean | {fmt_ms(data['baseline_mean'])} |
| significant-better line at observed mean | {fmt_ms(data['threshold'])} |
| raw-score run noise | SD {fmt_ms(finite_number(baseline['sample_sd'], 'SD'))}; CV {fmt_pct(finite_number(baseline['cv'], 'CV'))} |
| production-evaluation no-op placeability | {fmt_pct(finite_number(selected_option['launch_single_evaluation_placeability_probability'], 'launch placeability'))}; gate {fmt_pct(LAUNCH_PLACEABILITY_TARGET)} |
| retained strict familywise result | {fmt_pct(finite_number(selected_option['estimated_at_least_11_of_12_noop_placeability_probability'], 'familywise placeability'))}; 95.000% gate failed |
| independent reference separability | {independent['reference_ordering_passes']}/{INDEPENDENT_TARGET}; gate {INDEPENDENT_REQUIRED_PASSES}/{INDEPENDENT_TARGET} |
| independent reference placeability diagnostics | better {data['reference_placeability_counts']['better']}/{INDEPENDENT_TARGET}; no-op {data['reference_placeability_counts']['noop']}/{INDEPENDENT_TARGET}; worse {data['reference_placeability_counts']['worse']}/{INDEPENDENT_TARGET}; ordering remains shared-baseline raw-score based |
| CPU / RSS headroom evidence | {mean_cpu:.3f}/10 cores; {peak_gib:.3f} GiB peak |
| evaluator identity | `{compact_sha(EVALUATOR_IMAGE, 16)}` |
| local technical review | authenticated production-readiness seal `{compact_sha(data['readiness_sha256'], 16)}` |

The local technical launch gate is open for this exact source, image, host,
scale, scoring policy, and control-plane release. Any identity or policy change
requires a new content-addressed qualification chain.
"""


def atomic_pending(path: Path, content: str, mode: int) -> Path:
    pending = path.with_name(path.name + ".new")
    require(not pending.exists(), f"pending artifact exists: {pending}")
    pending.write_text(content, encoding="utf-8")
    pending.chmod(mode)
    return pending


def render_outputs(data: dict[str, Any]) -> None:
    require(not REPORT.exists(), f"final report already exists: {REPORT}")
    require(
        PREVIEW.is_file()
        and not PREVIEW.is_symlink()
        and sha256(PREVIEW) == PREVIEW_TEMPLATE_SHA256,
        "preview template changed",
    )
    require(
        not REPORT_EVIDENCE.exists(),
        f"final report evidence already exists: {REPORT_EVIDENCE}",
    )
    require(
        sha256(CALIBRATION_DOCUMENT) == CALIBRATION_TEMPLATE_SHA256,
        "calibration template changed",
    )
    report = render_html(data)
    parser = validate_report_shape(report)
    preview = render_preview_html(data)
    preview_parser = validate_report_shape(preview)
    calibration = render_calibration_document(data)
    lowered = calibration.lower()
    require(
        "not yet qualified" not in lowered
        and "not set" not in lowered
        and "not run" not in lowered,
        "calibration document contains template state",
    )

    evidence = {
        "schema": 1,
        "kind": "sim-latency-finalize-report-evidence",
        "generated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "source_lock_sha256": SOURCE_LOCK_SHA256,
        "historical_calibration_source_lock_sha256": (
            HISTORICAL_SOURCE_LOCK_SHA256
        ),
        "production_staging_attempt_06_remediation_amendment_sha256": (
            REMEDIATION_AMENDMENT_SHA256
        ),
        "season_base_equivalence_sha256": (
            SEASON_BASE_EQUIVALENCE_SHA256
        ),
        "same_seed_selection_sha256": sha256(SELECTION),
        "same_seed_analysis_sha256": sha256(SAME_ANALYSIS),
        "strict_same_seed_analysis_sha256": STRICT_SAME_ANALYSIS_SHA256,
        "pre_repair_same_seed_progress_sha256": PRE_REPAIR_PROGRESS_SHA256,
        "placeability_policy_amendment_sha256": PLACEABILITY_POLICY_SHA256,
        "same_seed_postprocessing_repair_sha256": POSTPROCESSING_REPAIR_SHA256,
        "independent_attestation_sha256": sha256(INDEPENDENT_ATTESTATION),
        "independent_attestation_repair_sha256": sha256(
            INDEPENDENT_ATTESTATION_REPAIR
        ),
        "independent_attestation_repair_script_sha256": sha256(
            INDEPENDENT_ATTESTATION_REPAIR_SCRIPT
        ),
        "independent_calibration_decision_sha256": sha256(INDEPENDENT_DECISION),
        "independent_terminal_decision_sha256": sha256(
            INDEPENDENT_TERMINAL_DECISION
        ),
        "independent_protocol_sha256": sha256(INDEPENDENT_PROTOCOL),
        "reference_v5_qualification_sha256": sha256(
            REFERENCE_V5_QUALIFICATION
        ),
        "production_staging_reference_v5_amendment_sha256": sha256(
            STAGING_REFERENCE_V5_AMENDMENT
        ),
        "independent_seed_commitment_sha256": sha256(INDEPENDENT_COMMITMENT),
        "independent_seed_results_sha256": data[
            "independent_seed_results_sha256"
        ],
        "independent_seed_result_hashes": data[
            "independent_seed_result_hashes"
        ],
        "reference_placeability_counts": data[
            "reference_placeability_counts"
        ],
        "production_readiness_sha256": sha256(READINESS),
        "calibration_document_sha256": sha256_text(calibration),
        "report_sha256": sha256_text(report),
        "preview_sha256": sha256_text(preview),
        "sections": 4,
        "section_svg_counts": parser.section_visuals,
        "baseline_ids": sorted(parser.baseline_ids),
        "all_baselines_have_threshold_lines": True,
        "preview_sections": 4,
        "preview_section_svg_counts": preview_parser.section_visuals,
        "preview_baseline_ids": sorted(preview_parser.baseline_ids),
        "preview_all_baselines_have_threshold_lines": True,
    }
    pending_paths: list[Path] = []
    try:
        report_pending = atomic_pending(REPORT, report, 0o444)
        pending_paths.append(report_pending)
        preview_pending = atomic_pending(PREVIEW, preview, 0o444)
        pending_paths.append(preview_pending)
        calibration_pending = atomic_pending(
            CALIBRATION_DOCUMENT, calibration, 0o444
        )
        pending_paths.append(calibration_pending)
        evidence_pending = atomic_pending(
            REPORT_EVIDENCE,
            json.dumps(evidence, indent=2, sort_keys=True, allow_nan=False) + "\n",
            0o400,
        )
        pending_paths.append(evidence_pending)

        calibration_pending.replace(CALIBRATION_DOCUMENT)
        preview_pending.replace(PREVIEW)
        report_pending.replace(REPORT)
        evidence_pending.replace(REPORT_EVIDENCE)
    except Exception:
        for pending in pending_paths:
            pending.unlink(missing_ok=True)
        raise
    exact_mode(CALIBRATION_DOCUMENT, 0o444)
    exact_mode(PREVIEW, 0o444)
    exact_mode(REPORT, 0o444)
    exact_mode(REPORT_EVIDENCE, 0o400)
    print(sha256(REPORT))


def self_test() -> None:
    security = {
        **{key: True for key in SECURITY_BOOLEAN_IDS},
        **{key: f"test-{key}" for key in SECURITY_ID_IDS},
    }
    require(
        security_evidence_authenticated(security),
        "self-test valid security evidence",
    )
    invalid_security = security.copy()
    invalid_security.pop("cleanup_complete")
    require(
        not security_evidence_authenticated(invalid_security),
        "self-test incomplete security evidence",
    )
    baseline_scores = [
        40_000 + index * 100 for index in range(SAME_SEED_TARGET)
    ]
    noop_scores = [39_000 + index * 110 for index in range(SAME_SEED_TARGET)]
    options = [
        {
            "replicates": count,
            "cv": 0.05 / math.sqrt(count),
            "minimum_margin": 0.2 / math.sqrt(count),
            "launch_single_evaluation_placeability_probability": (
                0.94614 if count == 9 else 0.80 + count / 100
            ),
            "estimated_at_least_11_of_12_noop_placeability_probability": (
                0.866 if count == 9 else 0.50 + count / 100
            ),
            "strict_familywise_quality_gate_eligible": False,
            "quality_gate_eligible": count == 9,
            "selection_eligible": count == 9,
        }
        for count in (1, 3, 5, 7, 9)
    ]
    fixture = {
        "baseline_scores": baseline_scores,
        "noop_scores": noop_scores,
        "baseline_mean": 40_550.0,
        "noop_mean": 39_605.0,
        "threshold": 32_440.0,
        "margin": 0.2,
        "replicate_count": 9,
        "analysis": {
            "baseline_raw_score_ms": {
                "sample_sd": 360.0,
                "cv": 0.009,
                "median": 40_550.0,
                "min": min(baseline_scores),
                "max": max(baseline_scores),
            },
            "noop_raw_score_ms": {"mean": 39_605.0},
            "noop_over_baseline_ratio": {"mean": 0.977, "median": 0.976},
            "noop_placeable_pairs": 8,
            "aggregation_options": options,
        },
        "selection": {},
        "progress": {
            "results": [
                {
                    "baseline_raw_score": baseline_scores[index],
                    "candidate_raw_score": noop_scores[index],
                    "candidate_to_baseline_ratio": noop_scores[index]
                    / baseline_scores[index],
                    "placeable": True,
                    "failed_gate_ids": [],
                }
                for index in range(SAME_SEED_TARGET)
            ]
        },
        "independent_progress": {
            "reference_ordering_passes": 5,
            "failed_ordering_seed_indices": [],
        },
        "seed_results": [
            {
                "seed_index": index + 1,
                "ordering_passed": True,
                "designated_baseline": {"raw_score_ms": 40_000.0},
                "references": {
                    "better": {
                        "candidate_raw_score_ms": 30_000.0 + index * 10
                    },
                    "noop": {
                        "candidate_raw_score_ms": 39_200.0 + index * 10
                    },
                    "worse": {
                        "candidate_raw_score_ms": 48_000.0 + index * 10
                    },
                }
            }
            for index in range(INDEPENDENT_TARGET)
        ],
        "independent_seed_results_sha256": "c" * 64,
        "independent_seed_result_hashes": {
            f"{index:02d}": f"{index:064x}"
            for index in range(1, INDEPENDENT_TARGET + 1)
        },
        "reference_placeability_counts": {
            "better": 4,
            "noop": 4,
            "worse": 3,
        },
        "readiness": {
            "sealed_at": "2026-08-27T00:00:00Z",
            "control_plane_commit": "5070445ddb1764ad80f999102a9d71946e5a9e29",
            "checks": {
                f"check-{index}": {
                    "passed": True,
                    "evidence_sha256": f"{index + 1:064x}",
                }
                for index in range(7)
            },
        },
        "readiness_sha256": "a" * 64,
        "source_lock": {
            "repositories": {
                "server": BASE_SHA,
                "connect": "b" * 40,
            }
        },
        "point": {
            "minimum_success_rate": 0.9868,
            "max_peak_rss_bytes": 12 * 2**30,
            "max_mean_cpu_cores": 4.6,
        },
        "frontier": {
            "rejected_upper_bound": {"minimum_success_rate": 0.9698}
        },
    }
    with tempfile.TemporaryDirectory(prefix="sim-latency-report-") as directory:
        report_path = Path(directory) / "report.html"
        rendered = render_html(fixture)
        report_path.write_text(rendered, encoding="utf-8")
        parser = validate_report_shape(report_path.read_text(encoding="utf-8"))
        require(
            parser.baseline_ids
            == {"same-seed-score", "independent-designated-baseline-ratio"},
            "self-test baseline mapping",
        )
        lowered_report = rendered.lower()
        require(
            "original familywise placeability rule produced no eligible"
            in lowered_report
            and "94.614%" in rendered
            and "86.600%" in rendered
            and "placeability is a separate diagnostic" in lowered_report
            and "better 4/5" in rendered,
            "self-test launch policy disclosure",
        )
        preview = render_preview_html(fixture)
        preview_parser = validate_report_shape(preview)
        require(
            preview_parser.baseline_ids == parser.baseline_ids
            and "shareable final preview" in preview
            and "launch-ready preview" in preview,
            "self-test preview rendering",
        )
        calibration = render_calibration_document(fixture)
        calibration_path = Path(directory) / "calibration.md"
        calibration_path.write_text(calibration, encoding="utf-8")
        lowered = calibration_path.read_text(encoding="utf-8").lower()
        require(
            SOURCE_LOCK_SHA256 in calibration
            and all(
                term in lowered
                for term in (
                    "baseline",
                    "takeover",
                    "independent",
                    "reference",
                    "resource",
                    "timeout",
                    "4/5",
                    "8/12",
                    "r=9",
                    "familywise",
                    "false-rejection",
                    "not confidence-equivalent",
                )
            )
            and "not yet qualified" not in lowered
            and "not set" not in lowered
            and "not run" not in lowered,
            "self-test calibration document",
        )
    print("finalization artifact renderer self-test: passed")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--self-test", action="store_true")
    parser.add_argument("--preflight", action="store_true")
    args = parser.parse_args()
    require(
        not (args.self_test and args.preflight),
        "diagnostic modes are mutually exclusive",
    )
    if args.self_test:
        self_test()
        return 0
    data = validate_terminal_inputs()
    if args.preflight:
        print("finalization artifact inputs: terminal and authenticated")
        return 0
    require(os.geteuid() == 0, "render final artifacts as root to read sealed evidence")
    render_outputs(data)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (
        KeyError,
        OSError,
        TypeError,
        ValueError,
        json.JSONDecodeError,
        RenderError,
    ) as exc:
        raise SystemExit(f"finalization artifact renderer: {exc}") from exc
