#!/usr/bin/env python3
"""Authenticate the terminal reports, playbook, and status handoff."""

from __future__ import annotations

import hashlib
import json
import re
import subprocess
from pathlib import Path
from typing import Any


SERVER = Path("/home/by/urnetwork/server")
EVIDENCE = Path("/home/by/urnetwork/server-finalization-evidence")
CALIBRATION_ROOT = (
    EVIDENCE
    / "connect/sim-latency/eval-12c/"
    "final-calibration-p1800-cf0fd3a9"
)
BASELINE_VALIDATOR = CALIBRATION_ROOT / "validate-final-baseline.py"
BASELINE_EVIDENCE = CALIBRATION_ROOT / "final-baseline-evidence.json"
PARENT_AUDIT = (
    CALIBRATION_ROOT
    / "finalization-handoff-attempt-07/finalization-audit.json"
)

ARTIFACTS = {
    "final_baseline": (
        EVIDENCE / "connect/sim-latency/final-baseline.html",
        "3eae32c77b862b5d77c8fd4d490f63747e85a8b632bf54335a1bcb71821966b8",
    ),
    "playbook": (
        EVIDENCE / "connect/sim-latency/playbook.md",
        "22fff87155ce79853a5af43d6b8aa9c9c100844ed6e5c78a4a4bf6a34cb9878a",
    ),
    "finalization_plan": (
        EVIDENCE / "connect/sim-latency/FINALIZE.md",
        "1925142238a93e9fd5f47dd7e743a0458fa874f5a6a03d94575abf67d9366edf",
    ),
    "finalization_status": (
        EVIDENCE / "connect/sim-latency/FINALIZATION-STATUS.md",
        "6c6ca200a9ef1ec10820f7cd37e026d6e79113acdf7a4b876eafb6c9e9464c4d",
    ),
    "calibration": (
        EVIDENCE / "connect/sim-latency/APEX-CALIBRATION.md",
        "dc473cecc91b9eea993fd3e43e2230e0a235bfc30771e6a9c65de2db284b2090",
    ),
    "final_report": (
        EVIDENCE / "finalize-report.html",
        "f81211d11aec2dca7705a941c1481b75c7c7f35ae847f986b168579c3ce76f52",
    ),
    "final_preview": (
        EVIDENCE / "final-preview.html",
        "0529ab0059dbac235667e6fca417b6aba3b8ac200fbb1fda89b95d067d6d912c",
    ),
    "final_baseline_evidence": (
        BASELINE_EVIDENCE,
        "0f02578e72b83e4e2909caa837de9cb9ddaf8efb6b8056833246d703099c0318",
    ),
    "final_baseline_validator": (
        BASELINE_VALIDATOR,
        "a9e021b52b2a9b46118cf8fc9000a2909f53100732db182d3c52a8efc42d9dea",
    ),
    "parent_completion_audit": (
        PARENT_AUDIT,
        "f28e630e5e7a8a79bedc97dbdc9dafd4302cb067daa875de20ce2278f7999423",
    ),
}

SOURCE_COPIES = {
    "final_baseline": SERVER / "connect/sim-latency/final-baseline.html",
    "playbook": SERVER / "connect/sim-latency/playbook.md",
    "calibration": SERVER / "connect/sim-latency/APEX-CALIBRATION.md",
    "final_report": SERVER / "finalize-report.html",
    "final_preview": SERVER / "final-preview.html",
}


class ValidationError(RuntimeError):
    pass


def require(condition: bool, message: str) -> None:
    if not condition:
        raise ValidationError(message)


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def load_object(path: Path) -> dict[str, Any]:
    require(path.is_file() and not path.is_symlink(), f"unsafe JSON: {path}")
    value = json.loads(path.read_text(encoding="utf-8"))
    require(isinstance(value, dict), f"expected object: {path}")
    return value


def validate_links(path: Path, text: str) -> None:
    for target in re.findall(r"\]\(([^)]+)\)", text):
        if target.startswith(("http://", "https://", "#")):
            continue
        target_path = (path.parent / target.split("#", 1)[0]).resolve()
        require(target_path.exists(), f"broken playbook link: {target}")


def validate() -> dict[str, Any]:
    artifact_hashes = {}
    for name, (path, expected) in ARTIFACTS.items():
        require(path.is_file() and not path.is_symlink(), f"unsafe artifact: {path}")
        actual = sha256(path)
        require(actual == expected, f"artifact hash changed: {name}")
        artifact_hashes[name] = actual

    for name, source_path in SOURCE_COPIES.items():
        evidence_path = ARTIFACTS[name][0]
        require(source_path.read_bytes() == evidence_path.read_bytes(), f"copy differs: {name}")

    parent = load_object(PARENT_AUDIT)
    require(
        parent.get("local_finalization_complete") is True
        and parent.get("required_passes") == 10
        and parent.get("required_pending") == 0
        and parent.get("required_failures") == 0,
        "parent completion audit",
    )

    completed = subprocess.run(
        ["python3", str(BASELINE_VALIDATOR)],
        check=False,
        capture_output=True,
        text=True,
    )
    require(completed.returncode == 0, f"baseline validator: {completed.stderr.strip()}")
    baseline_result = json.loads(completed.stdout)
    baseline_evidence = load_object(BASELINE_EVIDENCE)
    require(baseline_result == baseline_evidence, "baseline evidence differs from validator")
    require(
        baseline_result.get("sections") == 7
        and baseline_result.get("section_svg_counts") == [1] * 7
        and baseline_result.get("all_thresholds_have_significant_improvement_fringes") is True,
        "baseline report visual contract",
    )
    require(
        baseline_result.get("product_projection", {}).get("rounds", [])[-1]
        == {
            "round": 6,
            "throughput_p50_mbps": 4.224258622309758,
            "ttfb_p95_ms": 232.6173141665131,
        },
        "six-round product projection",
    )

    playbook_path = ARTIFACTS["playbook"][0]
    playbook = playbook_path.read_text(encoding="utf-8")
    validate_links(playbook_path, playbook)
    require(playbook.count("- [x]") == 8, "technical checklist count")
    require(playbook.count("- [ ]") == 11, "operator checklist count")
    for required in (
        "Trust-boundary rule that must not be weakened",
        "Do **not** add the API's `competition.yml`",
        "atomic live credential/seed-key rotation",
        "durable control-plane PostgreSQL/Redis",
        "public DNS/TLS/reverse-proxy",
        "monitoring destinations",
        "leaderboard/takeover publication",
        "Macrocosmos adapter/staging/registry acceptance",
    ):
        require(required in playbook, f"missing playbook disclosure: {required}")

    evaluator_config = Path("/home/by/urnetwork/config/local/competition.yml")
    evaluator_vault = Path("/home/by/urnetwork/vault/local/competition.yml")
    private_config = Path("/etc/urnetwork/competition-api/config/local/competition.yml")
    private_vault = Path("/etc/urnetwork/competition-api/vault/local/competition.yml")
    private_credentials = Path("/etc/urnetwork/competition-api/credentials.json")
    require(not evaluator_config.exists() and not evaluator_vault.exists(), "API resources entered evaluator leaves")
    require(private_config.is_file() and private_vault.is_file(), "private API resources missing")
    require(private_credentials.is_file(), "root-only credential record missing")
    require(private_credentials.stat().st_mode & 0o777 == 0o400, "credential mode")

    return {
        "schema": 1,
        "kind": "sim-latency-final-delivery-audit",
        "local_finalization_complete": True,
        "parent_completion_audit": {
            "sha256": artifact_hashes["parent_completion_audit"],
            "required_passes": 10,
            "required_pending": 0,
            "required_failures": 0,
        },
        "artifacts": artifact_hashes,
        "final_baseline": {
            "sections": 7,
            "svg_per_section": True,
            "all_thresholds_have_significant_improvement_fringes": True,
            "six_round_projection": baseline_result["product_projection"]["rounds"],
        },
        "playbook": {
            "technical_items_complete": 8,
            "operator_or_external_items_pending": 11,
            "candidate_readable_api_resources_absent": True,
            "private_api_resources_present": True,
            "raw_credentials_root_only": True,
        },
        "public_launch_follow_through_required": True,
        "external_apex_handoff_complete": False,
        "external_apex_handoff_launch_required_for_local_gate": False,
    }


if __name__ == "__main__":
    try:
        print(json.dumps(validate(), indent=2, sort_keys=True, allow_nan=False))
    except (OSError, ValueError, json.JSONDecodeError, ValidationError) as exc:
        raise SystemExit(f"final delivery validation: {exc}") from exc
