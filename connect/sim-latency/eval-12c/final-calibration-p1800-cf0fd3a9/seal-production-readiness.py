#!/usr/bin/env python3
"""Seal the seven authenticated production checks into the final readiness chain."""

from __future__ import annotations

import hashlib
import json
import math
import os
import time
from pathlib import Path
from typing import Any


ROOT = Path(
    "/home/by/urnetwork/server/connect/sim-latency/eval-12c/"
    "final-calibration-p1800-cf0fd3a9"
)
EVIDENCE_ROOT = ROOT / "production-readiness"
OUTPUT = ROOT / "production-readiness-final.json"
SELECTION = ROOT / "post-frontier/final-calibration-selection.json"
SELECTION_ATTESTATION = (
    ROOT / "post-frontier/launch-compromise-selection-attestation.json"
)
SAME_PROGRESS = (
    ROOT / "post-frontier/p1800-c200-r80-q2/same-seed/progress.json"
)
PRE_REPAIR_PROGRESS = (
    ROOT
    / "post-frontier/p1800-c200-r80-q2/same-seed/"
    "progress-before-postprocessing-repair.json"
)
SAME_ANALYSIS = (
    ROOT / "post-frontier/p1800-c200-r80-q2/same-seed-analysis.json"
)
SAME_DECISION = (
    ROOT / "post-frontier/p1800-c200-r80-q2/calibration-decision.json"
)
STRICT_SAME_ANALYSIS = (
    ROOT
    / "post-frontier/p1800-c200-r80-q2/same-seed-analysis-familywise.json"
)
PLACEABILITY_POLICY = (
    ROOT / "launch-readiness-placeability-policy-amendment.json"
)
POSTPROCESSING_REPAIR = ROOT / "same-seed-postprocessing-repair.json"
REFERENCE_V5 = ROOT / "reference-requalification-v5"
INDEPENDENT_ROOT = REFERENCE_V5 / "hidden-launch-runtime"
INDEPENDENT = INDEPENDENT_ROOT / "independent-campaign-attestation.json"
INDEPENDENT_DECISION = INDEPENDENT_ROOT / "calibration-decision.json"
INDEPENDENT_ANALYSIS = INDEPENDENT_ROOT / "same-seed-analysis.json"
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
    ROOT / "production-staging-reference-v5-amendment.json"
)
INDEPENDENT_COMMITMENT = (
    INDEPENDENT_ROOT / "independent-references/campaign-commitment.json"
)
INDEPENDENT_RESULTS = INDEPENDENT_ROOT / "independent-references"
SOURCE_RELEASE = ROOT / "control-plane-release/source-release.json"
RELEASE_ROOT = ROOT / "control-plane-release/final"
RELEASE_MANIFEST = RELEASE_ROOT / "release-build.json"

SOURCE_LOCK_SHA256 = (
    "0cf71458833f3b1ae96a663357c583eba3a9c25a19d6c795c8549e4154141838"
)
PROTOCOL_SHA256 = (
    "6fc4a809779bf6e694ef3afa71522fa50d0512c56177b42da4249738a37dc7af"
)
STAGING_REFERENCE_V5_AMENDMENT_SHA256 = (
    "618393539636b69cfcdbd6fec14afef3e58fe20d43bda06fbcbf15693802b695"
)
CONTROL_COMMIT = "5070445ddb1764ad80f999102a9d71946e5a9e29"
CONTROL_SOURCE_RELEASE_SHA256 = (
    "b942c70bae7e69bf08c811084075a094d4cbb18d74083e53a8935de110f4c940"
)
EVALUATOR_IMAGE = (
    "sha256:cf0fd3a9e73385729ee8dcd8da7ea53eb59d5f372b9ff36789ec923056222038"
)
HOST_QUALIFICATION_SHA256 = (
    "9cb7a977f171babafb5ff35c045799cbd54ec734ecfdebe7ebd106e482683d2f"
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
INDEPENDENT_ATTESTATION_REPAIR_SHA256 = (
    "499efd5e6d99f4d56a55f05d3949f6107ae8fcdeb2c7dfeb5b9877207541412d"
)
INDEPENDENT_ATTESTATION_REPAIR_SCRIPT_SHA256 = (
    "a5bfedfd7228b8e7c01a41334aa01b0d6a413ffadc4cca380073ac9ecdb668a0"
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
LAUNCH_REPLICATES = 9
LAUNCH_PLACEABILITY_TARGET = 0.94
LAUNCH_PLACEABILITY_OBSERVED = 0.94614
LAUNCH_TAKEOVER_MARGIN = 0.161
SAME_SEED_TARGET = 12
INDEPENDENT_TARGET = 5
INDEPENDENT_REQUIRED_PASSES = 4
ORDERING_METRIC = "candidate_raw_score_ms_over_designated_baseline_raw_score_ms"
REFERENCE_PATCH_SHA256 = {
    "better": "1a81e5a5fb7897cee38eb3952ed0db82a6cccb4a7821eb9db84d93eb55d9ff82",
    "noop": "8bd57a48ac82a6e846b607a9301c48145da5c66717c9e3a341138d034d1e0775",
    "worse": "982b192198ffa63942db1804629844f1cf9801bd4a71f64d2847a305217257a0",
}

CHECKS: dict[str, tuple[str, set[str]]] = {
    "release_artifacts": (
        "release-artifacts.json",
        {
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
        },
    ),
    "service_backed_fifo_cache_failover": (
        "service-backed-fifo-cache-failover.json",
        {
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
        },
    ),
    "authenticated_api_generate_submit_poll": (
        "authenticated-api.json",
        {
            "operator_generate_authenticated",
            "submitter_submit_authenticated",
            "poll_authenticated",
            "second_principal_cache_hit",
            "active_raw_score_redacted",
            "reveal_commitment_verified",
            "providers_download_hash_verified",
        },
    ),
    "full_staging_round": (
        "full-staging-round.json",
        {
            "same_round_baseline_verified",
            "exact_noop_patch_verified",
            "frozen_six_gate_set_verified",
            "evaluator_identity_verified",
            "host_identity_verified",
            "cleanup_verified",
            "artifact_manifest_verified",
        },
    ),
    "monitoring_and_recovery": (
        "monitoring-and-recovery.json",
        {
            "single_job_fifo_verified",
            "lease_recovery_verified",
            "host_heartbeat_verified",
            "resource_reports_verified",
            "cleanup_after_failure_verified",
        },
    ),
    "artifact_retention": (
        "artifact-retention.json",
        {
            "accounting_immutable",
            "resources_immutable",
            "artifact_quota_verified",
            "retain_until_verified",
            "failure_evidence_retained",
        },
    ),
    "no_secrets_audit": (
        "no-secrets-audit.json",
        {
            "direct_config_local_read_only",
            "direct_vault_local_read_only",
            "no_parent_config_mount",
            "no_parent_vault_mount",
            "no_control_secret_mount",
            "no_docker_socket_mount",
            "evidence_secret_scan_passed",
            "raw_tokens_not_stored",
        },
    ),
}


class SealError(RuntimeError):
    pass


def require(condition: bool, message: str) -> None:
    if not condition:
        raise SealError(message)


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def load(path: Path) -> dict[str, Any]:
    require(path.is_file() and not path.is_symlink(), f"unsafe evidence: {path}")
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


def independent_seed_results_digest(
    commitment: dict[str, Any],
) -> str:
    entries = commitment.get("seeds")
    require(
        isinstance(entries, list)
        and len(entries) == INDEPENDENT_TARGET
        and all(isinstance(entry, dict) for entry in entries)
        and [entry.get("seed_index") for entry in entries]
        == list(range(1, INDEPENDENT_TARGET + 1)),
        "independent seed commitment order changed",
    )
    commitments = {int(entry["seed_index"]): entry for entry in entries}
    digest = hashlib.sha256()
    for index in range(1, INDEPENDENT_TARGET + 1):
        path = INDEPENDENT_RESULTS / f"seed-{index:02d}/seed-result.json"
        require(
            path.stat().st_mode & 0o777 == 0o400,
            f"independent seed result mode changed: {path}",
        )
        result = load(path)
        entry = commitments[index]
        references = result.get("references")
        reference_order = result.get("reference_order")
        designated = result.get("designated_baseline")
        require(
            result.get("schema") == 1
            and result.get("kind") == "sim-latency-independent-seed-result"
            and result.get("seed_index") == index
            and result.get("replicates_per_reference") == 1
            and result.get("round_id") == entry.get("round_id")
            and result.get("seed_commitment") == entry.get("seed_commitment")
            and result.get("providers_sha256") == entry.get("providers_sha256")
            and result.get("calibration_decision_sha256")
            == sha256(INDEPENDENT_DECISION)
            and result.get("ordering_metric") == ORDERING_METRIC
            and isinstance(reference_order, list)
            and len(reference_order) == 3
            and set(reference_order) == {"better", "noop", "worse"}
            and isinstance(designated, dict)
            and designated.get("reference") == reference_order[0]
            and isinstance(references, dict)
            and set(references) == {"better", "noop", "worse"},
            f"independent seed result {index} is not authenticated",
        )
        designated_raw = finite_number(
            designated.get("raw_score_ms"),
            f"seed {index} designated baseline",
        )
        ratios = {}
        for reference in ("better", "noop", "worse"):
            record = references[reference]
            require(
                isinstance(record, dict)
                and record.get("patch_sha256")
                == REFERENCE_PATCH_SHA256[reference],
                f"seed {index} {reference} patch identity changed",
            )
            candidate = finite_number(
                record.get("candidate_raw_score_ms"),
                f"seed {index} {reference} candidate raw score",
            )
            finite_number(
                record.get("paired_ratio"),
                f"seed {index} {reference} private paired ratio diagnostic",
            )
            ratios[reference] = candidate / designated_raw
        require(
            result.get("ordering_passed")
            == (ratios["better"] < ratios["noop"] < ratios["worse"]),
            f"independent seed result {index} ordering changed",
        )
        result_sha256 = sha256(path)
        relative = path.relative_to(ROOT).as_posix()
        digest.update(
            f"{index:02d}\t{relative}\t{result_sha256}\n".encode("utf-8")
        )
    return digest.hexdigest()


def verify_authenticated_input(path: Path, expected_sha256: str) -> None:
    require(path.stat().st_mode & 0o777 == 0o400, f"evidence mode changed: {path}")
    require(sha256(path) == expected_sha256, f"authenticated evidence changed: {path}")


def verify_release_content(check: dict[str, Any]) -> None:
    """Resolve the release check's hashes to retained, read-only artifacts."""
    require(
        RELEASE_MANIFEST.stat().st_mode & 0o777 == 0o400,
        "release manifest mode changed",
    )
    manifest = load(RELEASE_MANIFEST)
    evidence = check.get("evidence_sha256")
    require(isinstance(evidence, dict), "release check evidence is malformed")
    require(
        evidence.get("release_manifest") == sha256(RELEASE_MANIFEST),
        "release manifest hash is unbound",
    )
    require(
        manifest.get("control_plane_commit") == CONTROL_COMMIT
        and manifest.get("source_lock_sha256") == SOURCE_LOCK_SHA256
        and manifest.get("production_staging_protocol_sha256") == PROTOCOL_SHA256
        and manifest.get("image_contexts_contain_config_or_vault") is False,
        "release manifest identity failed",
    )
    expected_paths = {
        "api_binary": RELEASE_ROOT / "binaries/api",
        "worker_binary": RELEASE_ROOT / "binaries/competitionworker",
        "rebaseline_binary": RELEASE_ROOT / "binaries/competitionrebaseline",
        "dbinit_binary": RELEASE_ROOT / "binaries/competitiondbinit",
        "api_archive": RELEASE_ROOT / "images/api.docker.tar",
        "worker_archive": RELEASE_ROOT / "images/worker.docker.tar",
        "api_metadata": RELEASE_ROOT / "images/api.metadata.json",
        "worker_metadata": RELEASE_ROOT / "images/worker.metadata.json",
        "api_provenance": RELEASE_ROOT / "images/api.attestations/provenance.json",
        "worker_provenance": RELEASE_ROOT / "images/worker.attestations/provenance.json",
        "api_sbom": RELEASE_ROOT / "images/api.attestations/sbom.spdx.json",
        "worker_sbom": RELEASE_ROOT / "images/worker.attestations/sbom.spdx.json",
        "builder_inspect": RELEASE_ROOT / "images/builder.inspect.log",
    }
    for label, path in expected_paths.items():
        require(path.is_file() and not path.is_symlink(), f"unsafe release artifact: {label}")
        required_mode = 0o500 if label.endswith("_binary") else 0o400
        require(path.stat().st_mode & 0o777 == required_mode, f"release artifact mode changed: {label}")

    binaries = manifest.get("binaries")
    images = manifest.get("images")
    builder = manifest.get("builder")
    attestations = manifest.get("attestations")
    require(
        isinstance(binaries, dict)
        and isinstance(images, dict)
        and isinstance(builder, dict)
        and isinstance(attestations, dict),
        "release manifest content is malformed",
    )
    binary_evidence = evidence.get("binaries")
    image_evidence = evidence.get("images")
    builder_evidence = evidence.get("builder")
    require(
        isinstance(binary_evidence, dict)
        and isinstance(image_evidence, dict)
        and isinstance(builder_evidence, dict),
        "release check content hashes are malformed",
    )
    for key, label in {
        "api": "api_binary",
        "worker": "worker_binary",
        "rebaseline": "rebaseline_binary",
        "dbinit": "dbinit_binary",
    }.items():
        binary = binaries.get(key)
        require(isinstance(binary, dict), f"release binary is malformed: {key}")
        digest = sha256(expected_paths[label])
        require(
            binary.get("sha256") == digest
            and binary_evidence.get(key) == digest,
            f"release binary hash failed: {key}",
        )
    for key in ("api", "worker"):
        image = images.get(key)
        checked = image_evidence.get(key)
        require(isinstance(image, dict) and isinstance(checked, dict), f"release image is malformed: {key}")
        for field, suffix in {
            "docker_archive_sha256": "archive",
            "metadata_sha256": "metadata",
            "provenance_sha256": "provenance",
            "sbom_sha256": "sbom",
        }.items():
            digest = sha256(expected_paths[f"{key}_{suffix}"])
            require(
                image.get(field) == digest and checked.get(field) == digest,
                f"release image content hash failed: {key}.{field}",
            )
        require(
            image.get("image_id") == checked.get("image_id")
            and isinstance(image.get("image_id"), str)
            and image["image_id"].startswith("sha256:"),
            f"release image ID failed: {key}",
        )
    require(
        builder.get("driver") == "docker-container"
        and isinstance(builder.get("image_ref"), str)
        and "@sha256:" in builder["image_ref"]
        and builder.get("image_id") == builder_evidence.get("image_id")
        and builder.get("inspect_sha256") == sha256(expected_paths["builder_inspect"])
        and builder.get("inspect_sha256") == builder_evidence.get("inspect_sha256"),
        "digest-pinned BuildKit evidence failed",
    )
    require(
        attestations.get("provenance_mode") == "max"
        and attestations.get("provenance_version") == "v1"
        and attestations.get("sbom_format") == "SPDX"
        and isinstance(attestations.get("sbom_generator_image_ref"), str)
        and "@sha256:" in attestations["sbom_generator_image_ref"]
        and attestations.get("provenance_verified") is True
        and attestations.get("sbom_verified") is True,
        "release attestation identity failed",
    )
    require(
        evidence.get("openapi") == manifest.get("competition_openapi_sha256"),
        "competition OpenAPI hash is unbound",
    )
    for key in ("api", "worker"):
        provenance = load(expected_paths[f"{key}_provenance"])
        sbom = load(expected_paths[f"{key}_sbom"])
        provenance_predicate = provenance.get("predicate")
        sbom_predicate = sbom.get("predicate")
        require(
            provenance.get("predicateType") == "https://slsa.dev/provenance/v1"
            and isinstance(provenance_predicate, dict)
            and isinstance(provenance_predicate.get("buildDefinition"), dict)
            and isinstance(provenance_predicate.get("runDetails"), dict),
            f"SLSA v1 provenance failed: {key}",
        )
        require(
            str(sbom.get("predicateType", "")).startswith("https://spdx.dev/Document")
            and isinstance(sbom_predicate, dict)
            and str(sbom_predicate.get("spdxVersion", "")).startswith("SPDX-"),
            f"SPDX SBOM failed: {key}",
        )


def verify_check(check_id: str, name: str, expected: set[str]) -> dict[str, Any]:
    path = EVIDENCE_ROOT / name
    require(path.stat().st_mode & 0o777 == 0o400, f"check mode changed: {path}")
    value = load(path)
    assertions = value.get("assertions")
    evidence = value.get("evidence_sha256") or value.get("logs")
    require(
        value.get("schema") == 1
        and value.get("kind") == "sim-latency-production-readiness-check"
        and value.get("check_id") == check_id
        and value.get("passed") is True
        and value.get("source_lock_sha256") == SOURCE_LOCK_SHA256
        and value.get("production_staging_protocol_sha256") == PROTOCOL_SHA256
        and value.get("control_plane_commit") == CONTROL_COMMIT,
        f"check identity failed: {check_id}",
    )
    require(
        isinstance(assertions, dict)
        and set(assertions) == expected
        and all(assertion is True for assertion in assertions.values()),
        f"check assertions failed: {check_id}",
    )
    if check_id not in {
        "release_artifacts",
        "service_backed_fifo_cache_failover",
    }:
        require(
            value.get("production_staging_reference_v5_amendment_sha256")
            == STAGING_REFERENCE_V5_AMENDMENT_SHA256,
            f"reference-v5 staging amendment is unbound: {check_id}",
        )
    require(isinstance(evidence, dict) and evidence, f"check has no content evidence: {check_id}")
    serialized = path.read_text(encoding="utf-8").lower()
    require(
        "authorization: bearer" not in serialized
        and "begin private key" not in serialized
        and "seed_key_base64" not in serialized,
        f"check contains secret-shaped content: {check_id}",
    )
    if check_id == "release_artifacts":
        verify_release_content(value)
    return {
        "passed": True,
        "evidence_path": str(path.relative_to(ROOT)),
        "evidence_sha256": sha256(path),
    }


def main() -> int:
    require(os.geteuid() == 0, "production readiness sealing must run as root")
    require(not OUTPUT.exists(), f"final readiness already exists: {OUTPUT}")
    require(sha256(SOURCE_RELEASE) == CONTROL_SOURCE_RELEASE_SHA256, "source release changed")
    for path, expected_hash in (
        (PLACEABILITY_POLICY, PLACEABILITY_POLICY_SHA256),
        (POSTPROCESSING_REPAIR, POSTPROCESSING_REPAIR_SHA256),
        (STRICT_SAME_ANALYSIS, STRICT_SAME_ANALYSIS_SHA256),
        (PRE_REPAIR_PROGRESS, PRE_REPAIR_PROGRESS_SHA256),
        (INDEPENDENT_ATTESTATION_REPAIR, INDEPENDENT_ATTESTATION_REPAIR_SHA256),
    ):
        verify_authenticated_input(path, expected_hash)

    selection = load(SELECTION)
    selection_attestation = load(SELECTION_ATTESTATION)
    progress = load(SAME_PROGRESS)
    pre_repair_progress = load(PRE_REPAIR_PROGRESS)
    analysis = load(SAME_ANALYSIS)
    strict_analysis = load(STRICT_SAME_ANALYSIS)
    policy = load(PLACEABILITY_POLICY)
    repair = load(POSTPROCESSING_REPAIR)
    independent = load(INDEPENDENT)
    independent_decision = load(INDEPENDENT_DECISION)
    independent_analysis = load(INDEPENDENT_ANALYSIS)
    independent_commitment = load(INDEPENDENT_COMMITMENT)
    independent_terminal = load(INDEPENDENT_TERMINAL_DECISION)
    independent_attestation_repair = load(INDEPENDENT_ATTESTATION_REPAIR)
    independent_protocol = load(INDEPENDENT_PROTOCOL)
    independent_measurement_amendment = load(
        INDEPENDENT_MEASUREMENT_AMENDMENT
    )
    reference_v5_qualification = load(REFERENCE_V5_QUALIFICATION)
    staging_reference_v5_amendment = load(STAGING_REFERENCE_V5_AMENDMENT)

    require(
        sha256(STAGING_REFERENCE_V5_AMENDMENT)
        == STAGING_REFERENCE_V5_AMENDMENT_SHA256
        and STAGING_REFERENCE_V5_AMENDMENT.stat().st_mode & 0o777 == 0o400
        and staging_reference_v5_amendment.get("kind")
        == "sim-latency-production-staging-reference-v5-amendment"
        and staging_reference_v5_amendment.get("draft") is False
        and staging_reference_v5_amendment.get("authorized") is True
        and staging_reference_v5_amendment.get("source_lock_sha256")
        == SOURCE_LOCK_SHA256
        and staging_reference_v5_amendment.get(
            "original_production_staging_protocol_sha256"
        )
        == PROTOCOL_SHA256
        and staging_reference_v5_amendment.get(
            "hidden_campaign_attestation_sha256"
        )
        == sha256(INDEPENDENT)
        and staging_reference_v5_amendment.get(
            "hidden_campaign_decision_sha256"
        )
        == sha256(INDEPENDENT_TERMINAL_DECISION)
        and staging_reference_v5_amendment.get(
            "hidden_campaign_protocol_sha256"
        )
        == sha256(INDEPENDENT_PROTOCOL)
        and staging_reference_v5_amendment.get(
            "hidden_attestation_repair_sha256"
        )
        == INDEPENDENT_ATTESTATION_REPAIR_SHA256
        and staging_reference_v5_amendment.get(
            "hidden_attestation_repair_script_sha256"
        )
        == INDEPENDENT_ATTESTATION_REPAIR_SCRIPT_SHA256
        and staging_reference_v5_amendment.get(
            "reference_v5_qualification_sha256"
        )
        == sha256(REFERENCE_V5_QUALIFICATION)
        and staging_reference_v5_amendment.get(
            "replacement_measurement_dependencies", {}
        ).get("same_seed_pairs")
        == SAME_SEED_TARGET
        and staging_reference_v5_amendment.get(
            "replacement_measurement_dependencies", {}
        ).get("independent_seeds")
        == INDEPENDENT_TARGET
        and staging_reference_v5_amendment.get(
            "replacement_measurement_dependencies", {}
        ).get("required_reference_ordering_passes")
        == INDEPENDENT_REQUIRED_PASSES,
        "reference-v5 staging amendment is not authenticated",
    )
    require(
        sha256(INDEPENDENT_ATTESTATION_REPAIR_SCRIPT)
        == INDEPENDENT_ATTESTATION_REPAIR_SCRIPT_SHA256
        and independent_attestation_repair.get("kind")
        == "sim-latency-hidden-attestation-schema-postprocessing-repair"
        and independent_attestation_repair.get("source_lock_sha256")
        == SOURCE_LOCK_SHA256
        and independent_attestation_repair.get("campaign_commitment_sha256")
        == sha256(INDEPENDENT_COMMITMENT)
        and independent_attestation_repair.get("terminal_decision_sha256")
        == sha256(INDEPENDENT_TERMINAL_DECISION)
        and independent_attestation_repair.get("attestation_sha256")
        == sha256(INDEPENDENT)
        and independent_attestation_repair.get("statistical_measurements_rerun")
        is False
        and independent_attestation_repair.get("measurements_censored") is False
        and independent_attestation_repair.get(
            "original_measurement_artifacts_changed"
        )
        is False,
        "independent attestation repair is not authenticated",
    )

    active_measurements = dict(progress)
    retained_measurements = dict(pre_repair_progress)
    active_measurements.pop("generated_at", None)
    retained_measurements.pop("generated_at", None)
    require(
        active_measurements == retained_measurements,
        "post-processing repair changed same-seed measurements",
    )
    strict_policy = policy.get("strict_policy")
    launch_policy = policy.get("launch_policy")
    retention = policy.get("retention")
    require(
        policy.get("kind")
        == "sim-latency-launch-readiness-placeability-policy-amendment"
        and policy.get("authorized") is True
        and policy.get("source_lock_sha256") == SOURCE_LOCK_SHA256
        and policy.get("same_seed_progress_sha256")
        == PRE_REPAIR_PROGRESS_SHA256
        and policy.get("strict_same_seed_analysis_sha256")
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
        "placeability policy amendment is not authenticated",
    )
    strict_options = strict_analysis.get("aggregation_options")
    require(
        strict_analysis.get("kind")
        == "sim-latency-launch-compromise-same-seed-analysis"
        and strict_analysis.get("decision_ready") is False
        and strict_analysis.get("replicate_count") == 12
        and strict_analysis.get("recommended_replicates") is None
        and strict_analysis.get("recommended_takeover_margin") is None
        and strict_analysis.get("progress_sha256")
        == PRE_REPAIR_PROGRESS_SHA256
        and strict_analysis.get("noop_placeable_pairs") == 8
        and isinstance(strict_options, list)
        and [option.get("replicates") for option in strict_options]
        == [1, 3, 5, 7, 9]
        and all(
            option.get("selection_eligible") is False
            for option in strict_options
        ),
        "strict familywise failure is not retained",
    )
    options = analysis.get("aggregation_options")
    require(
        analysis.get("kind")
        == "sim-latency-launch-compromise-same-seed-analysis"
        and analysis.get("decision_ready") is True
        and analysis.get("replicate_count") == 12
        and analysis.get("recommended_replicates") == LAUNCH_REPLICATES
        and math.isclose(
            finite_number(
                analysis.get("recommended_takeover_margin"),
                "analysis takeover margin",
            ),
            LAUNCH_TAKEOVER_MARGIN,
            rel_tol=1e-12,
        )
        and analysis.get("progress_sha256") == sha256(SAME_PROGRESS)
        and analysis.get("noop_placeable_pairs") == 8
        and analysis.get("baseline_raw_score_ms")
        == strict_analysis.get("baseline_raw_score_ms")
        and analysis.get("noop_raw_score_ms")
        == strict_analysis.get("noop_raw_score_ms")
        and isinstance(options, list)
        and [option.get("replicates") for option in options]
        == [1, 3, 5, 7, 9],
        "launch same-seed analysis is not authenticated",
    )
    selected_option = next(
        option
        for option in options
        if option.get("replicates") == LAUNCH_REPLICATES
    )
    require(
        selected_option.get("selection_eligible") is True
        and selected_option.get("strict_familywise_quality_gate_eligible")
        is False
        and math.isclose(
            finite_number(
                selected_option.get(
                    "launch_single_evaluation_placeability_probability"
                ),
                "selected placeability",
            ),
            LAUNCH_PLACEABILITY_OBSERVED,
            rel_tol=1e-12,
        )
        and all(
            option.get("selection_eligible") is False
            for option in options
            if option.get("replicates") != LAUNCH_REPLICATES
        ),
        "launch replicate selection is not unique",
    )
    require(
        selection.get("accepted") is True
        and selection.get("source_lock_sha256") == SOURCE_LOCK_SHA256
        and selection.get("same_seed_pairs") == 12
        and selection.get("independent_seed_target") == 12
        and selection.get("reference_required_passes") == 11
        and selection.get("replicate_count") == LAUNCH_REPLICATES
        and math.isclose(
            finite_number(selection.get("takeover_margin"), "selection margin"),
            LAUNCH_TAKEOVER_MARGIN,
            rel_tol=1e-12,
        )
        and selection.get("same_seed_progress_sha256")
        == sha256(SAME_PROGRESS)
        and selection.get("same_seed_analysis_sha256")
        == sha256(SAME_ANALYSIS),
        "same-seed selection is not terminal",
    )
    baseline_mean = finite_number(
        analysis.get("baseline_raw_score_ms", {}).get("mean"),
        "baseline mean",
    )
    threshold = finite_number(
        selection.get("baseline_mean_significantly_better_threshold_ms"),
        "baseline threshold",
    )
    require(
        math.isclose(
            threshold,
            baseline_mean * (1 - LAUNCH_TAKEOVER_MARGIN),
            rel_tol=1e-12,
        )
        and math.isclose(
            threshold,
            finite_number(
                launch_policy.get(
                    "baseline_mean_significantly_better_threshold_ms"
                ),
                "policy baseline threshold",
            ),
            rel_tol=1e-12,
        ),
        "launch threshold arithmetic failed",
    )
    require(
        selection_attestation.get("accepted") is True
        and selection_attestation.get("calibration_selection_sha256")
        == sha256(SELECTION)
        and selection_attestation.get("same_seed_analysis_sha256")
        == sha256(SAME_ANALYSIS)
        and selection_attestation.get("same_seed_progress_sha256")
        == sha256(SAME_PROGRESS),
        "same-seed selection attestation is not terminal",
    )
    require(
        repair.get("kind") == "sim-latency-same-seed-postprocessing-repair"
        and repair.get("passed") is True
        and repair.get("source_lock_sha256") == SOURCE_LOCK_SHA256
        and repair.get("placeability_policy_amendment_sha256")
        == PLACEABILITY_POLICY_SHA256
        and repair.get("strict_familywise_analysis_sha256")
        == STRICT_SAME_ANALYSIS_SHA256
        and repair.get("retained_pre_repair_progress_sha256")
        == PRE_REPAIR_PROGRESS_SHA256
        and repair.get("terminal_progress_sha256") == sha256(SAME_PROGRESS)
        and repair.get("launch_analysis_sha256") == sha256(SAME_ANALYSIS)
        and repair.get("calibration_selection_sha256") == sha256(SELECTION)
        and repair.get("selection_attestation_sha256")
        == sha256(SELECTION_ATTESTATION)
        and repair.get("selected_replicates") == LAUNCH_REPLICATES
        and repair.get("measurements_rerun") is False
        and repair.get("measurements_censored") is False
        and repair.get("strict_analysis_retained") is True,
        "same-seed post-processing repair is not authenticated",
    )
    require(
        independent.get("accepted") is True
        and independent.get("target_independent_seeds") == INDEPENDENT_TARGET
        and independent.get("reference_required_passes")
        == INDEPENDENT_REQUIRED_PASSES
        and independent.get("reference_ordering_passes", 0)
        >= INDEPENDENT_REQUIRED_PASSES
        and independent.get("campaign_commitment_sha256")
        == sha256(INDEPENDENT_COMMITMENT)
        and independent.get("protocol_sha256") == sha256(INDEPENDENT_PROTOCOL)
        and independent.get("measurement_amendment_sha256")
        == sha256(INDEPENDENT_MEASUREMENT_AMENDMENT)
        and independent.get("independent_reference_r1_correction_sha256")
        == R1_CORRECTION_SHA256
        and independent.get("terminal_decision_sha256")
        == sha256(INDEPENDENT_TERMINAL_DECISION)
        and independent.get("ordering_metric") == ORDERING_METRIC
        and independent.get("placeability_is_diagnostic_only") is True
        and independent.get("confidence_equivalent_to_original_protocol")
        is False,
        "independent reference attestation is not terminal",
    )
    require(
        INDEPENDENT_DECISION.stat().st_mode & 0o777 == 0o400
        and INDEPENDENT_ANALYSIS.stat().st_mode & 0o777 == 0o400,
        "independent calibration projection mode changed",
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
        "independent seed commitment is not authenticated",
    )
    require(
        independent_decision.get("decision_ready") is True
        and independent_decision.get("source_lock_sha256")
        == SOURCE_LOCK_SHA256
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
        "independent calibration projection is not authenticated",
    )
    require(
        independent_terminal.get("kind")
        == "sim-latency-reference-v5-hidden-launch-decision"
        and independent_terminal.get("accepted") is True
        and independent_terminal.get("campaign_exit_code") == 0
        and independent_terminal.get("completed_independent_seeds")
        == INDEPENDENT_TARGET
        and independent_terminal.get("reference_required_passes")
        == INDEPENDENT_REQUIRED_PASSES
        and independent_terminal.get("reference_ordering_passes", 0)
        >= INDEPENDENT_REQUIRED_PASSES
        and independent_terminal.get("ordering_metric") == ORDERING_METRIC
        and independent_terminal.get(
            "one_designated_same_round_baseline_per_seed"
        )
        is True
        and independent_terminal.get("placeability_is_diagnostic_only") is True
        and independent_terminal.get("campaign_commitment_sha256")
        == sha256(INDEPENDENT_COMMITMENT)
        and independent_terminal.get("protocol_sha256")
        == sha256(INDEPENDENT_PROTOCOL)
        and independent_terminal.get("measurement_amendment_sha256")
        == sha256(INDEPENDENT_MEASUREMENT_AMENDMENT)
        and independent_terminal.get("cleanup", {}).get(
            "residual_competition_containers"
        )
        == 0
        and independent_terminal.get("cleanup", {}).get(
            "residual_competition_networks"
        )
        == 0,
        "independent terminal decision is not authenticated",
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
        and independent_protocol.get("placeability_is_diagnostic_only") is True
        and reference_v5_qualification.get("kind")
        == "sim-latency-reference-v5-pilot-qualification"
        and reference_v5_qualification.get("draft") is False
        and reference_v5_qualification.get(
            "accepted_for_hidden_five_seed_screen"
        )
        is True,
        "reference-v5 protocol or qualification is not authenticated",
    )
    seed_results_sha256 = independent_seed_results_digest(
        independent_commitment
    )
    records = {
        check_id: verify_check(check_id, name, assertions)
        for check_id, (name, assertions) in CHECKS.items()
    }
    result = {
        "schema": 1,
        "kind": "sim-latency-production-readiness-final",
        "sealed_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "source_lock_sha256": SOURCE_LOCK_SHA256,
        "production_staging_protocol_sha256": PROTOCOL_SHA256,
        "production_staging_reference_v5_amendment_sha256": (
            STAGING_REFERENCE_V5_AMENDMENT_SHA256
        ),
        "control_plane_commit": CONTROL_COMMIT,
        "control_plane_source_release_sha256": CONTROL_SOURCE_RELEASE_SHA256,
        "evaluator_image_digest": EVALUATOR_IMAGE,
        "host_qualification_sha256": HOST_QUALIFICATION_SHA256,
        "same_seed_selection_sha256": sha256(SELECTION),
        "placeability_policy_amendment_sha256": PLACEABILITY_POLICY_SHA256,
        "same_seed_postprocessing_repair_sha256": POSTPROCESSING_REPAIR_SHA256,
        "strict_same_seed_analysis_sha256": STRICT_SAME_ANALYSIS_SHA256,
        "pre_repair_same_seed_progress_sha256": PRE_REPAIR_PROGRESS_SHA256,
        "independent_attestation_sha256": sha256(INDEPENDENT),
        "independent_attestation_repair_sha256": (
            INDEPENDENT_ATTESTATION_REPAIR_SHA256
        ),
        "independent_attestation_repair_script_sha256": (
            INDEPENDENT_ATTESTATION_REPAIR_SCRIPT_SHA256
        ),
        "independent_calibration_decision_sha256": sha256(INDEPENDENT_DECISION),
        "independent_terminal_decision_sha256": sha256(
            INDEPENDENT_TERMINAL_DECISION
        ),
        "independent_protocol_sha256": sha256(INDEPENDENT_PROTOCOL),
        "reference_v5_qualification_sha256": sha256(
            REFERENCE_V5_QUALIFICATION
        ),
        "independent_seed_commitment_sha256": sha256(INDEPENDENT_COMMITMENT),
        "independent_seed_results_sha256": seed_results_sha256,
        "checks": records,
    }
    pending = OUTPUT.with_name(OUTPUT.name + ".new")
    require(not pending.exists(), f"pending readiness output exists: {pending}")
    pending.write_text(
        json.dumps(result, indent=2, sort_keys=True, allow_nan=False) + "\n",
        encoding="utf-8",
    )
    pending.chmod(0o400)
    pending.replace(OUTPUT)
    print(sha256(OUTPUT))
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, ValueError, json.JSONDecodeError, SealError) as exc:
        raise SystemExit(f"production readiness seal: {exc}") from exc
