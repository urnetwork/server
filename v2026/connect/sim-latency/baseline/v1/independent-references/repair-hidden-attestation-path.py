#!/usr/bin/env python3
"""Reconstruct the missing v5 campaign reveal without rerunning measurements."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import time
from typing import Any
import uuid


SERVER = Path("/home/by/urnetwork/server")
ROOT = SERVER / "connect/sim-latency/eval-12c/final-calibration-p1800-cf0fd3a9"
V5 = ROOT / "reference-requalification-v5"
CAMPAIGN = V5 / "hidden-launch-runtime/independent-references"
TERMINAL_REVEAL = V5 / "hidden-launch-seed-reveal.json"
CAMPAIGN_REVEAL = CAMPAIGN / "seed-reveal.json"
CAMPAIGN_COMMITMENT = CAMPAIGN / "campaign-commitment.json"
CALIBRATION_DECISION = V5 / "hidden-launch-runtime/calibration-decision.json"
DECISION = V5 / "hidden-launch-decision.json"
PROGRESS = CAMPAIGN / "progress.json"
ATTESTATION = V5 / "hidden-launch-runtime/independent-campaign-attestation.json"
SOURCE_RUNNER = V5 / "run-hidden-launch.py"
INSTALLED_RUNNER = Path(
    "/usr/local/libexec/urnetwork/run-reference-v5-hidden-a889248b-attempt-01"
)
EVIDENCE = V5 / "hidden-attestation-path-repair.json"
INVALID_ATTEMPT = V5 / "hidden-attestation-path-repair-attempt-01-invalid"
SERVICE = "urnetwork-reference-v5-hidden-a889248b-attempt-01.service"

SOURCE_LOCK_SHA256 = (
    "0cf71458833f3b1ae96a663357c583eba3a9c25a19d6c795c8549e4154141838"
)
RUNNER_SHA256 = (
    "a889248bf2b2175e79ce0f5526cfa294fea17700f56f6eec46eda8f53aae519e"
)
PROTOCOL_SHA256 = (
    "4969535eb343049d7b790c5fff8e82b7eb7a60b6e92d2e2aa94e6466e7789fad"
)
DECISION_SHA256 = (
    "3e4cc70d783b01a87328736caf82f49016138c97ff384b26dc38864f8cede835"
)
PROGRESS_SHA256 = (
    "f2bbe8797dc463bb85d576cea29575579757df3eb7109a2170fd0566008d1e8b"
)
REVEAL_SHA256 = (
    "57435cb82f1a0d4689f1ba32d56fba6483d8fc233eb3569697171c981aad3441"
)
CAMPAIGN_COMMITMENT_SHA256 = (
    "c79798ad1769fda861fd86da0cdec7bd2f12f11f5964e2582cfe84a69c9afd69"
)
CALIBRATION_DECISION_SHA256 = (
    "ba49014d7ceef1ff044a2d799f7911868b2eb159a6c587425a9c9f3d4fac2649"
)
INVALID_ATTESTATION_SHA256 = (
    "50ffce32792a9ecc73c7b135a0f9050727d4ad467c6f26a87bd09f18d68a925f"
)
INVALID_REPAIR_EVIDENCE_SHA256 = (
    "3591d3e860aa8b6892df76187954430fe9abcafad22b0f2f943b50f4022541c9"
)


class RepairError(RuntimeError):
    pass


def require(condition: bool, message: str) -> None:
    if not condition:
        raise RepairError(message)


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def bytes_sha256(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def load_json(path: Path) -> dict[str, Any]:
    value = json.loads(path.read_text(encoding="utf-8"))
    require(isinstance(value, dict), f"{path}: expected JSON object")
    return value


def atomic_json(path: Path, value: dict[str, Any]) -> None:
    encoded = json.dumps(value, indent=2, sort_keys=True, allow_nan=False) + "\n"
    require(not path.exists() and not path.is_symlink(), f"output already exists: {path}")
    pending = path.with_name(path.name + ".new")
    require(not pending.exists() and not pending.is_symlink(), f"pending output exists: {pending}")
    pending.write_text(encoded, encoding="utf-8")
    pending.chmod(0o400)
    pending.replace(path)


def derive_round_fields(round_id: str, seed_bytes: bytes) -> tuple[str, int]:
    require(len(seed_bytes) == 32, "round seed must contain 32 bytes")
    try:
        parsed_round_id = uuid.UUID(round_id)
    except (AttributeError, ValueError) as error:
        raise RepairError("round id is not a UUID") from error
    require(str(parsed_round_id) == round_id, "round id is not canonical")
    commitment = hashlib.sha256(
        b"urnetwork-sim-latency-round-v1\0" + parsed_round_id.bytes + seed_bytes
    ).hexdigest()
    generator_digest = hashlib.sha256(
        b"urnetwork-sim-latency-generator-v1\0" + seed_bytes
    ).digest()
    generator_seed = int.from_bytes(generator_digest[:8], "big") & ((1 << 63) - 1)
    if generator_seed == 0:
        generator_seed = 1
    return commitment, generator_seed


def validate_campaign_reveal(value: dict[str, Any]) -> None:
    require(value.get("schema") == 1, "campaign reveal schema")
    require(
        value.get("kind") == "sim-latency-independent-seed-reveal",
        "campaign reveal kind",
    )
    require(value.get("replicates_per_reference") == 1, "campaign reveal replicate count")
    require(
        value.get("calibration_decision_sha256") == CALIBRATION_DECISION_SHA256,
        "campaign reveal calibration decision",
    )
    revealed_at = value.get("revealed_at")
    require(
        isinstance(revealed_at, str)
        and re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z", revealed_at) is not None,
        "campaign reveal timestamp",
    )
    seeds = value.get("seeds")
    require(isinstance(seeds, list) and len(seeds) == 5, "campaign reveal seed count")
    require(
        [seed.get("seed_index") for seed in seeds if isinstance(seed, dict)]
        == [1, 2, 3, 4, 5],
        "campaign reveal seed order",
    )
    expected_keys = {
        "seed_index",
        "round_id",
        "round_seed_hex",
        "seed_commitment",
        "generator_seed",
        "providers_sha256",
    }
    for seed in seeds:
        require(isinstance(seed, dict) and set(seed) == expected_keys, "campaign reveal seed fields")
        require(
            isinstance(seed.get("round_seed_hex"), str)
            and re.fullmatch(r"[0-9a-f]{64}", seed["round_seed_hex"]) is not None,
            "campaign reveal seed material",
        )
        require(
            isinstance(seed.get("seed_commitment"), str)
            and re.fullmatch(r"[0-9a-f]{64}", seed["seed_commitment"]) is not None,
            "campaign reveal seed commitment",
        )
        require(
            isinstance(seed.get("providers_sha256"), str)
            and re.fullmatch(r"[0-9a-f]{64}", seed["providers_sha256"]) is not None,
            "campaign reveal provider digest",
        )
        expected_commitment, expected_generator_seed = derive_round_fields(
            seed["round_id"], bytes.fromhex(seed["round_seed_hex"])
        )
        require(seed["seed_commitment"] == expected_commitment, "campaign reveal commitment derivation")
        require(seed.get("generator_seed") == expected_generator_seed, "campaign reveal generator derivation")


def build_campaign_reveal() -> dict[str, Any]:
    commitment = load_json(CAMPAIGN_COMMITMENT)
    terminal_reveal = load_json(TERMINAL_REVEAL)
    commitment_seeds = commitment.get("seeds")
    terminal_seeds = terminal_reveal.get("seeds")
    require(
        commitment.get("schema") == 1
        and commitment.get("kind") == "sim-latency-independent-seed-campaign-commitment"
        and commitment.get("target_independent_seeds") == 5
        and commitment.get("independent_reference_replicates") == 1
        and commitment.get("calibration_decision_sha256") == CALIBRATION_DECISION_SHA256
        and isinstance(commitment_seeds, list)
        and len(commitment_seeds) == 5,
        "campaign commitment is invalid",
    )
    require(
        terminal_reveal.get("kind") == "sim-latency-reference-v5-hidden-launch-seed-reveal"
        and terminal_reveal.get("campaign_commitment_sha256") == CAMPAIGN_COMMITMENT_SHA256
        and terminal_reveal.get("seed_material_retired") is True
        and isinstance(terminal_seeds, list)
        and len(terminal_seeds) == 5,
        "terminal reveal is invalid",
    )
    revealed: list[dict[str, Any]] = []
    for index in range(1, 6):
        directory = CAMPAIGN / f"seed-{index:02d}"
        private_path = directory / "round-private.json"
        public_path = directory / "round-public.json"
        seed_path = directory / "round-seed.bin"
        for path in (private_path, public_path, seed_path):
            require(path.is_file() and not path.is_symlink(), f"unsafe round input: {path}")
        private = load_json(private_path)
        public = load_json(public_path)
        seed_bytes = seed_path.read_bytes()
        require(
            private.get("seed_index") == index
            and public.get("seed_index") == index,
            f"seed {index}: index",
        )
        entry = {
            "seed_index": index,
            "round_id": private.get("round_id"),
            "round_seed_hex": private.get("round_seed_hex"),
            "seed_commitment": private.get("seed_commitment"),
            "generator_seed": private.get("generator_seed"),
            "providers_sha256": private.get("providers_sha256"),
        }
        require(
            isinstance(entry["round_seed_hex"], str)
            and seed_bytes.hex() == entry["round_seed_hex"],
            f"seed {index}: binary seed mismatch",
        )
        public_fields = {
            "seed_index": public.get("seed_index"),
            "round_id": public.get("round_id"),
            "seed_commitment": public.get("seed_commitment"),
            "providers_sha256": public.get("providers_sha256"),
        }
        commitment_fields = {
            "seed_index": commitment_seeds[index - 1].get("seed_index"),
            "round_id": commitment_seeds[index - 1].get("round_id"),
            "seed_commitment": commitment_seeds[index - 1].get("seed_commitment"),
            "providers_sha256": commitment_seeds[index - 1].get("providers_sha256"),
        }
        reveal_public_fields = {
            key: entry[key] for key in ("seed_index", "round_id", "seed_commitment", "providers_sha256")
        }
        require(public_fields == commitment_fields == reveal_public_fields, f"seed {index}: public lineage")
        require(terminal_seeds[index - 1] == entry, f"seed {index}: terminal reveal lineage")
        revealed.append(entry)
    require(
        len({entry["round_seed_hex"] for entry in revealed}) == 5
        and len({entry["seed_commitment"] for entry in revealed}) == 5,
        "campaign reveal seeds are not unique",
    )
    value = {
        "schema": 1,
        "kind": "sim-latency-independent-seed-reveal",
        "replicates_per_reference": 1,
        "revealed_at": terminal_reveal["revealed_at"],
        "calibration_decision_sha256": CALIBRATION_DECISION_SHA256,
        "seeds": revealed,
    }
    validate_campaign_reveal(value)
    return value


def attempt_snapshot() -> dict[str, dict[str, str]]:
    snapshot: dict[str, dict[str, str]] = {}
    attempts = sorted(CAMPAIGN.glob("seed-*/reference-*/attempt-*"))
    require(len(attempts) == 15, f"expected 15 attempt directories, found {len(attempts)}")
    for attempt in attempts:
        require(attempt.is_dir() and not attempt.is_symlink(), f"unsafe attempt: {attempt}")
        worker = attempt / "worker-result.json"
        manifest = attempt / "evidence-manifest.json"
        require(worker.is_file() and manifest.is_file(), f"incomplete attempt: {attempt}")
        relative = str(attempt.relative_to(CAMPAIGN))
        snapshot[relative] = {
            "worker_result_sha256": sha256(worker),
            "evidence_manifest_sha256": sha256(manifest),
        }
    for index in range(1, 6):
        result = CAMPAIGN / f"seed-{index:02d}/seed-result.json"
        require(result.is_file() and not result.is_symlink(), f"missing seed result {index}")
        snapshot[f"seed-{index:02d}"] = {"seed_result_sha256": sha256(result)}
    return snapshot


def service_state() -> dict[str, Any]:
    completed = subprocess.run(
        [
            "systemctl",
            "show",
            SERVICE,
            "-p",
            "ActiveState",
            "-p",
            "SubState",
            "-p",
            "Result",
            "-p",
            "ExecMainStatus",
            "-p",
            "NRestarts",
            "-p",
            "InvocationID",
            "--no-pager",
        ],
        check=True,
        capture_output=True,
        text=True,
    )
    values: dict[str, str] = {}
    for line in completed.stdout.splitlines():
        key, separator, value = line.partition("=")
        require(bool(separator), f"malformed systemd property: {line}")
        values[key] = value
    return {
        "active_state": values.get("ActiveState"),
        "sub_state": values.get("SubState"),
        "result": values.get("Result"),
        "exec_main_status": int(values.get("ExecMainStatus", "-1")),
        "restarts": int(values.get("NRestarts", "-1")),
        "invocation_id": values.get("InvocationID"),
    }


def residual_counts() -> dict[str, int]:
    containers = subprocess.run(
        ["sudo", "-n", "docker", "ps", "-aq", "--filter", "label=com.urnetwork.competition.job-id"],
        check=True,
        capture_output=True,
        text=True,
    ).stdout.splitlines()
    networks = subprocess.run(
        ["sudo", "-n", "docker", "network", "ls", "-q", "--filter", "label=com.urnetwork.competition.job-id"],
        check=True,
        capture_output=True,
        text=True,
    ).stdout.splitlines()
    return {
        "residual_competition_containers": len([value for value in containers if value]),
        "residual_competition_networks": len([value for value in networks if value]),
    }


def self_test() -> None:
    with tempfile.TemporaryDirectory(prefix="hidden-attestation-repair-") as directory:
        root = Path(directory)
        seed_bytes = bytes(range(32))
        round_id = "12345678-1234-4678-9234-567812345678"
        commitment, generator_seed = derive_round_fields(round_id, seed_bytes)
        seed = {
            "seed_index": 1,
            "round_id": round_id,
            "round_seed_hex": seed_bytes.hex(),
            "seed_commitment": commitment,
            "generator_seed": generator_seed,
            "providers_sha256": "1" * 64,
        }
        reveal = {
            "schema": 1,
            "kind": "sim-latency-independent-seed-reveal",
            "replicates_per_reference": 1,
            "revealed_at": "2026-01-02T03:04:05Z",
            "calibration_decision_sha256": CALIBRATION_DECISION_SHA256,
            "seeds": [{**seed, "seed_index": index} for index in range(1, 6)],
        }
        validate_campaign_reveal(reveal)
        output = root / "campaign.json"
        atomic_json(output, reveal)
        require(output.stat().st_mode & 0o777 == 0o400, "self-test output mode")
        require(load_json(output) == reveal, "self-test deterministic serialization")
        terminal_style = {
            "schema": 1,
            "kind": "sim-latency-reference-v5-hidden-launch-seed-reveal",
            "revealed_at": "2026-01-02T03:04:05Z",
            "seeds": reveal["seeds"],
        }
        try:
            validate_campaign_reveal(terminal_style)
        except RepairError:
            pass
        else:
            raise RepairError("terminal reveal schema was accepted as campaign reveal")
        wrong_generator = json.loads(json.dumps(reveal))
        wrong_generator["seeds"][0]["generator_seed"] += 1
        try:
            validate_campaign_reveal(wrong_generator)
        except RepairError:
            pass
        else:
            raise RepairError("incorrect generator derivation was accepted")
    print("hidden attestation schema repair self-test: passed")


def repair() -> None:
    require(os.geteuid() != 0, "run the repair as the measurement owner, not root")
    require(not EVIDENCE.exists() and not EVIDENCE.is_symlink(), "repair evidence already exists")
    require(not ATTESTATION.exists() and not ATTESTATION.is_symlink(), "attestation already exists")
    require(not CAMPAIGN_REVEAL.exists() and not CAMPAIGN_REVEAL.is_symlink(), "campaign reveal already exists")
    for path, expected in (
        (ROOT / "source-lock.json", SOURCE_LOCK_SHA256),
        (SOURCE_RUNNER, RUNNER_SHA256),
        (INSTALLED_RUNNER, RUNNER_SHA256),
        (V5 / "hidden-launch-protocol.json", PROTOCOL_SHA256),
        (DECISION, DECISION_SHA256),
        (PROGRESS, PROGRESS_SHA256),
        (TERMINAL_REVEAL, REVEAL_SHA256),
        (CAMPAIGN_COMMITMENT, CAMPAIGN_COMMITMENT_SHA256),
        (CALIBRATION_DECISION, CALIBRATION_DECISION_SHA256),
        (INVALID_ATTEMPT / "independent-campaign-attestation.json", INVALID_ATTESTATION_SHA256),
        (INVALID_ATTEMPT / "repair-evidence.json", INVALID_REPAIR_EVIDENCE_SHA256),
        (INVALID_ATTEMPT / "seed-reveal.json", REVEAL_SHA256),
    ):
        require(path.is_file() and not path.is_symlink(), f"unsafe fixed input: {path}")
        require(sha256(path) == expected, f"fixed input changed: {path}")

    decision = load_json(DECISION)
    progress = load_json(PROGRESS)
    require(
        decision.get("accepted") is True
        and decision.get("campaign_exit_code") == 0
        and decision.get("completed_independent_seeds") == 5
        and decision.get("reference_ordering_passes") == 4
        and decision.get("reference_required_passes") == 4
        and decision.get("seed_reveal_sha256") == REVEAL_SHA256,
        "terminal decision is not the accepted 4/5 result",
    )
    require(
        progress.get("complete") is True
        and progress.get("completed_independent_seeds") == 5
        and progress.get("reference_ordering_passes") == 4
        and progress.get("separability_passed") is True,
        "campaign progress is not terminal",
    )
    state = service_state()
    require(
        state["active_state"] == "failed"
        and state["result"] == "exit-code"
        and state["exec_main_status"] == 1
        and state["restarts"] == 0,
        f"unexpected original service state: {state}",
    )
    cleanup_before = residual_counts()
    require(all(value == 0 for value in cleanup_before.values()), "resources remain before repair")
    before = attempt_snapshot()
    campaign_reveal = build_campaign_reveal()
    atomic_json(CAMPAIGN_REVEAL, campaign_reveal)
    campaign_reveal_sha256 = sha256(CAMPAIGN_REVEAL)
    require(campaign_reveal_sha256 != REVEAL_SHA256, "distinct reveal schemas have identical bytes")

    completed = subprocess.run(
        [str(INSTALLED_RUNNER)],
        cwd=SERVER,
        check=False,
        capture_output=True,
    )
    require(completed.returncode == 0, f"terminal-only runner repair failed: {completed.returncode}")
    require(ATTESTATION.is_file() and not ATTESTATION.is_symlink(), "attestation was not created")
    after = attempt_snapshot()
    require(after == before, "measurement attempt or seed-result evidence changed")
    cleanup_after = residual_counts()
    require(all(value == 0 for value in cleanup_after.values()), "resources remain after repair")

    attestation = load_json(ATTESTATION)
    require(
        attestation.get("accepted") is True
        and attestation.get("target_independent_seeds") == 5
        and attestation.get("reference_required_passes") == 4
        and attestation.get("reference_ordering_passes") == 4
        and attestation.get("campaign_progress_sha256") == PROGRESS_SHA256
        and attestation.get("seed_reveal_sha256") == campaign_reveal_sha256
        and attestation.get("terminal_decision_sha256") == DECISION_SHA256
        and attestation.get("protocol_sha256") == PROTOCOL_SHA256
        and attestation.get("runner_sha256") == RUNNER_SHA256,
        "repaired attestation lineage is invalid",
    )

    evidence = {
        "schema": 1,
        "kind": "sim-latency-hidden-attestation-schema-postprocessing-repair",
        "repaired_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "root_cause": "The hidden runner completed and sealed its v5 terminal reveal, but its inherited campaign shell did not leave the distinct generic independent-seed reveal required by the inherited attestation path.",
        "repair": "Reconstruct the generic campaign reveal from the original private rounds, authenticate each derivation against the public manifests, premeasurement commitment, and sealed terminal reveal, then re-enter only the frozen runner's existing-decision attestation branch.",
        "invalid_first_repair": {
            "reason": "A byte-for-byte mirror of the terminal reveal was rejected because the terminal and generic campaign reveals intentionally have different schemas.",
            "retained_private_for_forensics": True,
            "attestation_sha256": INVALID_ATTESTATION_SHA256,
            "repair_evidence_sha256": INVALID_REPAIR_EVIDENCE_SHA256,
            "mirrored_reveal_sha256": REVEAL_SHA256,
        },
        "source_lock_sha256": SOURCE_LOCK_SHA256,
        "original_runner_sha256": RUNNER_SHA256,
        "repair_script_sha256": sha256(Path(__file__).resolve()),
        "terminal_decision_sha256": DECISION_SHA256,
        "terminal_progress_sha256": PROGRESS_SHA256,
        "campaign_commitment_sha256": CAMPAIGN_COMMITMENT_SHA256,
        "calibration_decision_sha256": CALIBRATION_DECISION_SHA256,
        "terminal_reveal_sha256": REVEAL_SHA256,
        "campaign_reveal_sha256": campaign_reveal_sha256,
        "attestation_sha256": sha256(ATTESTATION),
        "original_service_state": state,
        "terminal_only_runner_exit_code": completed.returncode,
        "terminal_only_runner_stdout_sha256": bytes_sha256(completed.stdout),
        "terminal_only_runner_stderr_sha256": bytes_sha256(completed.stderr),
        "attempt_directory_count_before": 15,
        "attempt_directory_count_after": 15,
        "attempt_snapshot_sha256": bytes_sha256(
            json.dumps(before, sort_keys=True, separators=(",", ":")).encode("utf-8")
        ),
        "seed_results_or_worker_evidence_changed": False,
        "statistical_measurements_rerun": False,
        "measurements_censored": False,
        "original_measurement_artifacts_changed": False,
        "campaign_reveal_reconstructed": True,
        "campaign_reveal_kind": "sim-latency-independent-seed-reveal",
        "terminal_reveal_kind": "sim-latency-reference-v5-hidden-launch-seed-reveal",
        "reveal_documents_byte_identical": False,
        "commitment_derivations_reverified": True,
        "generator_seed_derivations_reverified": True,
        "public_private_terminal_lineage_reverified": True,
        "cleanup_before": cleanup_before,
        "cleanup_after": cleanup_after,
        "repair_evidence_contains_seed_material": False,
        "private_reveal_artifacts_exportable": False,
    }
    atomic_json(EVIDENCE, evidence)
    print(
        json.dumps(
            {
                "repaired": True,
                "attestation_sha256": evidence["attestation_sha256"],
                "repair_evidence_sha256": sha256(EVIDENCE),
                "statistical_measurements_rerun": False,
            },
            sort_keys=True,
        )
    )


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    if args.self_test:
        self_test()
    else:
        repair()


if __name__ == "__main__":
    try:
        main()
    except (OSError, RepairError, ValueError, subprocess.SubprocessError) as error:
        raise SystemExit(f"hidden attestation path repair: {error}") from error
