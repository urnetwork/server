#!/usr/bin/env python3

import hashlib
import importlib.util
import json
import tempfile
from pathlib import Path
from types import ModuleType
from typing import Any, Callable


HERE = Path(__file__).resolve().parent
STAGING = HERE / "run-production-staging.sh"
SEALER = HERE / "seal-production-readiness.py"
AUDITOR = HERE / "audit-finalization.py"
CHECK_ID = "authenticated_api_generate_submit_poll"


def require(condition: bool, message: str) -> None:
    if not condition:
        raise RuntimeError(message)


def load_module(name: str, path: Path) -> ModuleType:
    spec = importlib.util.spec_from_file_location(name, path)
    require(spec is not None and spec.loader is not None, f"cannot load {path}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def write_fixture(path: Path, value: dict[str, Any]) -> None:
    path.chmod(0o600) if path.exists() else None
    path.write_text(
        json.dumps(value, indent=2, sort_keys=True, allow_nan=False) + "\n",
        encoding="utf-8",
    )
    path.chmod(0o400)


def expect_failure(action: Callable[[], Any], label: str) -> None:
    try:
        action()
    except Exception:
        return
    raise RuntimeError(f"accepted {label}")


def main() -> int:
    producer = STAGING.read_text(encoding="utf-8")
    emitted_field = (
        "production_staging_attempt_06_remediation_amendment_sha256:"
        "$remediation_amendment,"
    )
    require(
        producer.count(emitted_field) == 5,
        "all five staging records must emit the attempt-06 remediation binding",
    )
    require(
        producer.count('--arg remediation_amendment "$REMEDIATION_AMENDMENT_SHA"')
        == 2,
        "the producer and write-check validator must receive the remediation hash",
    )

    sealer = load_module("production_readiness_sealer", SEALER)
    auditor = load_module("production_readiness_auditor", AUDITOR)
    expected_assertions = sealer.CHECKS[CHECK_ID][1]
    require(
        expected_assertions == auditor.PRODUCTION_ASSERTIONS[CHECK_ID],
        "sealer and auditor assertion contracts differ",
    )

    with tempfile.TemporaryDirectory(
        prefix="sim-latency-remediation-binding-"
    ) as directory:
        root = Path(directory)
        evidence_root = root / "production-readiness"
        evidence_root.mkdir()
        evidence_path = evidence_root / "authenticated-api.json"
        fixture = {
            "schema": 1,
            "kind": "sim-latency-production-readiness-check",
            "check_id": CHECK_ID,
            "passed": True,
            "source_lock_sha256": sealer.SOURCE_LOCK_SHA256,
            "production_staging_protocol_sha256": sealer.PROTOCOL_SHA256,
            "production_staging_reference_v5_amendment_sha256": (
                sealer.STAGING_REFERENCE_V5_AMENDMENT_SHA256
            ),
            "production_release_self_check_contract_amendment_sha256": (
                sealer.RELEASE_SELF_CHECK_AMENDMENT_SHA256
            ),
            "production_staging_attempt_06_remediation_amendment_sha256": (
                sealer.REMEDIATION_AMENDMENT_SHA256
            ),
            "control_plane_commit": sealer.CONTROL_COMMIT,
            "evidence_sha256": {"fixture": "a" * 64},
            "assertions": {name: True for name in expected_assertions},
        }
        write_fixture(evidence_path, fixture)

        sealer.EVIDENCE_ROOT = evidence_root
        sealer.ROOT = root
        sealer.verify_check(
            CHECK_ID,
            evidence_path.name,
            expected_assertions,
        )

        readiness = {
            "production_staging_reference_v5_amendment_sha256": (
                fixture[
                    "production_staging_reference_v5_amendment_sha256"
                ]
            ),
            "production_release_self_check_contract_amendment_sha256": (
                auditor.RELEASE_SELF_CHECK_AMENDMENT_SHA256
            ),
            "production_staging_attempt_06_remediation_amendment_sha256": (
                auditor.REMEDIATION_AMENDMENT_SHA256
            ),
            "checks": {
                CHECK_ID: {
                    "passed": True,
                    "evidence_path": (
                        "production-readiness/authenticated-api.json"
                    ),
                    "evidence_sha256": sha256(evidence_path),
                }
            },
        }
        auditor.ROOT = root
        auditor.load_production_check(readiness, CHECK_ID)

        missing = fixture.copy()
        missing.pop(
            "production_staging_attempt_06_remediation_amendment_sha256"
        )
        write_fixture(evidence_path, missing)
        readiness["checks"][CHECK_ID]["evidence_sha256"] = sha256(evidence_path)
        expect_failure(
            lambda: sealer.verify_check(
                CHECK_ID,
                evidence_path.name,
                expected_assertions,
            ),
            "a sealer record with no remediation binding",
        )
        expect_failure(
            lambda: auditor.load_production_check(readiness, CHECK_ID),
            "an audit record with no remediation binding",
        )

        wrong = fixture.copy()
        wrong[
            "production_staging_attempt_06_remediation_amendment_sha256"
        ] = "0" * 64
        write_fixture(evidence_path, wrong)
        readiness["checks"][CHECK_ID]["evidence_sha256"] = sha256(evidence_path)
        expect_failure(
            lambda: sealer.verify_check(
                CHECK_ID,
                evidence_path.name,
                expected_assertions,
            ),
            "a sealer record with the wrong remediation binding",
        )
        expect_failure(
            lambda: auditor.load_production_check(readiness, CHECK_ID),
            "an audit record with the wrong remediation binding",
        )

    print("production readiness remediation binding test: passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
