#!/usr/bin/env python3
"""Authenticate the standalone final-baseline infographic."""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import re
from html.parser import HTMLParser
from pathlib import Path
from typing import Any


SERVER = Path("/home/by/urnetwork/server")
EVIDENCE_SERVER = Path("/home/by/urnetwork/server-finalization-evidence")
ROOT = (
    SERVER
    / "connect/sim-latency/eval-12c/"
    "final-calibration-p1800-cf0fd3a9"
)
EVIDENCE_ROOT = (
    EVIDENCE_SERVER
    / "connect/sim-latency/eval-12c/"
    "final-calibration-p1800-cf0fd3a9"
)
REPORT = SERVER / "connect/sim-latency/final-baseline.html"
EVIDENCE_REPORT = (
    EVIDENCE_SERVER / "connect/sim-latency/final-baseline.html"
)
SAME_SEED = ROOT / "post-frontier/p1800-c200-r80-q2/same-seed-analysis.json"
SELECTION = ROOT / "post-frontier/final-calibration-selection.json"
FRONTIER_POINT = ROOT / "exact-frontier/p1800-c200-r80-q2/point-summary.json"
REFERENCE_DECISION = ROOT / "reference-requalification-v5/hidden-launch-decision.json"
WORKER_ATTEMPT = Path(
    "/var/lib/urnetwork/competition/"
    "01a04755-63f0-041c-60bb-47b92ccbcf8c/attempt-02"
)
WORKER_RESULT = WORKER_ATTEMPT / "worker-result.json"
WORKER_BASELINE = WORKER_ATTEMPT / "baseline.json"
WORKER_MANIFEST = WORKER_ATTEMPT / "evidence-manifest.json"
FINAL_AUDIT = EVIDENCE_ROOT / "finalization-handoff-attempt-07/finalization-audit.json"
REPORT_EVIDENCE = EVIDENCE_ROOT / "finalize-report-evidence.json"

EXPECTED_HASHES = {
    SAME_SEED: "41325e3aabd98495afb22264ab2981ad690e4e2ca7ebb295b40c40ea96d0de9c",
    SELECTION: "5838b36d8414162497c4537f8ac07d46b3c74badcb6083cae4d71a9e40517042",
    FRONTIER_POINT: "f0789d4ccf7f310cb8c480ea5bd248469eaa4a3813a1b751f526093c9fc3e721",
    REFERENCE_DECISION: "3e4cc70d783b01a87328736caf82f49016138c97ff384b26dc38864f8cede835",
    WORKER_RESULT: "5b9c57ea35346ffd9b281e4cc6e49bb56485770e62dca5a55f1d1f3b9c0573ec",
    WORKER_BASELINE: "84d49d0847104ae79b878a6cbf6eaef7675ef8234e0d8df2615f324e85da623f",
    WORKER_MANIFEST: "378be6195463f55bcae1bdfb57008a3d0f34eb141c08b8b33258091b372a9fd9",
    FINAL_AUDIT: "f28e630e5e7a8a79bedc97dbdc9dafd4302cb067daa875de20ce2278f7999423",
    REPORT_EVIDENCE: "ce6b3daa416c70b388e991b1f0a521e296699b6be91f44afe78d87c5ef653696",
}
EXPECTED_BASELINES = {
    "calibration-policy",
    "same-seed-calibration",
    "production-staging-baseline",
    "independent-designated-baseline",
    "product-ttfb",
    "product-speed",
}
BASELINE_RUN_PATTERN = re.compile(
    r"^evidence/scorer-input/baseline-[0-9]{2}-[^/]+/run\.json$"
)


class ValidationError(RuntimeError):
    pass


class ShapeParser(HTMLParser):
    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.section_depth = 0
        self.section_visuals: list[int] = []
        self.baseline_ids: set[str] = set()
        self.threshold_ids: set[str] = set()
        self.threshold_label_ids: set[str] = set()
        self.fringe_ids: set[str] = set()
        self.scripts = 0
        self.external_resources: list[str] = []

    def handle_starttag(
        self, tag: str, attrs: list[tuple[str, str | None]]
    ) -> None:
        attributes = dict(attrs)
        if tag == "section":
            self.section_depth += 1
            self.section_visuals.append(0)
        elif tag == "svg" and self.section_depth == 1:
            self.section_visuals[-1] += 1
        if tag == "script":
            self.scripts += 1
        for key in ("href", "src"):
            value = attributes.get(key)
            if value and value.startswith(("http://", "https://", "//")):
                self.external_resources.append(value)
        baseline = attributes.get("data-baseline-id")
        if baseline:
            self.baseline_ids.add(baseline)
        threshold = attributes.get("data-threshold-for")
        if threshold:
            self.threshold_ids.add(threshold)
        threshold_label = attributes.get("data-threshold-label-for")
        if threshold_label:
            self.threshold_label_ids.add(threshold_label)
        fringe = attributes.get("data-improvement-fringe-for")
        if fringe and tag in {"rect", "path", "polygon"}:
            self.fringe_ids.add(fringe)

    def handle_startendtag(
        self, tag: str, attrs: list[tuple[str, str | None]]
    ) -> None:
        self.handle_starttag(tag, attrs)

    def handle_endtag(self, tag: str) -> None:
        if tag == "section":
            self.section_depth -= 1


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


def close(actual: Any, expected: float) -> bool:
    return bool(
        isinstance(actual, (int, float))
        and not isinstance(actual, bool)
        and math.isfinite(actual)
        and math.isclose(float(actual), expected, rel_tol=1e-12)
    )


def validate_shape(text: str) -> ShapeParser:
    parser = ShapeParser()
    parser.feed(text)
    require(parser.section_depth == 0, "unbalanced sections")
    require(parser.section_visuals == [1, 1, 1, 1, 1, 1, 1], "visual coverage")
    require(
        parser.baseline_ids
        == parser.threshold_ids
        == parser.threshold_label_ids
        == parser.fringe_ids
        == EXPECTED_BASELINES,
        "baseline, threshold, label, and fringe mappings differ",
    )
    require(parser.scripts == 0, "scripts are forbidden")
    require(not parser.external_resources, "external resources are forbidden")
    lowered = text.lower()
    require(
        lowered.count("significant-improvement fringe") >= 8,
        "significant-improvement fringe disclosure is incomplete",
    )
    require("lower is better" in lowered, "score direction is missing")
    return parser


def self_test() -> None:
    valid = "".join(
        f'<section><svg><line data-baseline-id="{name}"/>'
        f'<line data-threshold-for="{name}"/>'
        f'<text data-threshold-label-for="{name}">threshold</text>'
        f'<rect data-improvement-fringe-for="{name}"/></svg></section>'
        for name in sorted(EXPECTED_BASELINES)
    )
    valid += "<section><svg/></section>"
    parser = ShapeParser()
    parser.feed(valid)
    require(parser.section_visuals == [1] * 7, "self-test visual coverage")
    require(
        parser.baseline_ids
        == parser.threshold_ids
        == parser.threshold_label_ids
        == parser.fringe_ids
        == EXPECTED_BASELINES,
        "self-test valid mapping",
    )
    missing = ShapeParser()
    missing.feed(valid.replace(' data-improvement-fringe-for="calibration-policy"', ""))
    require(missing.fringe_ids != missing.baseline_ids, "self-test missing fringe")
    print("final baseline validator self-test: passed")


def validate() -> dict[str, Any]:
    for path, expected in EXPECTED_HASHES.items():
        require(sha256(path) == expected, f"evidence hash changed: {path}")
    require(REPORT.read_bytes() == EVIDENCE_REPORT.read_bytes(), "report copies differ")
    text = REPORT.read_text(encoding="utf-8")
    parser = validate_shape(text)

    analysis = load_object(SAME_SEED)
    selection = load_object(SELECTION)
    frontier = load_object(FRONTIER_POINT)
    references = load_object(REFERENCE_DECISION)
    worker = load_object(WORKER_RESULT)
    baseline = load_object(WORKER_BASELINE)
    worker_manifest = load_object(WORKER_MANIFEST)
    audit = load_object(FINAL_AUDIT)
    report_evidence = load_object(REPORT_EVIDENCE)

    baseline_stats = analysis.get("baseline_raw_score_ms", {})
    noop_stats = analysis.get("noop_raw_score_ms", {})
    require(close(baseline_stats.get("mean"), 43101.69114999998), "baseline mean")
    require(close(baseline_stats.get("sample_sd"), 4364.9049320891145), "baseline SD")
    require(close(baseline_stats.get("cv"), 0.10126992272527378), "baseline CV")
    require(close(noop_stats.get("mean"), 42196.87311249998), "no-op mean")
    require(selection.get("replicate_count") == 9, "replicate policy")
    require(close(selection.get("takeover_margin"), 0.161), "takeover margin")
    require(
        close(
            selection.get("baseline_mean_significantly_better_threshold_ms"),
            36162.31887484998,
        ),
        "calibration threshold",
    )
    require(frontier.get("point_id") == "p1800-c200-r80-q2", "frontier point")
    require(frontier.get("quality_passed") is True, "frontier quality")
    require(references.get("accepted") is True, "reference decision")
    require(references.get("reference_ordering_passes") == 4, "reference passes")
    require(references.get("reference_required_passes") == 4, "reference gate")

    score = worker.get("score", {})
    diagnostics = score.get("diagnostics", {})
    require(close(score.get("raw_score"), 40476.36540000001), "staging score")
    require(close(score.get("normalized_score"), 98.81357961552511), "normalized score")
    require(score.get("placeable") is True, "staging placeability")
    require(score.get("takeover_eligible") is False, "staging takeover")
    require(
        all(item.get("passed") is True for item in score.get("gates", {}).values())
        and len(score.get("gates", {})) == 6,
        "staging gates",
    )
    require(
        close(diagnostics.get("baseline", {}).get("raw_score"), 39996.14554999987),
        "staging baseline",
    )
    require(
        close(diagnostics.get("baseline_takeover_raw_max"), 33556.76611644989),
        "staging threshold",
    )
    require(len(baseline.get("replicates", [])) == 9, "baseline replicates")
    require(len(diagnostics.get("replicates", [])) == 9, "candidate replicates")

    manifest_entries = [
        item
        for item in worker_manifest.get("artifacts", [])
        if isinstance(item, dict)
        and isinstance(item.get("path"), str)
        and BASELINE_RUN_PATTERN.fullmatch(item["path"])
    ]
    require(len(manifest_entries) == 9, "staging baseline run-manifest count")
    run_manifests = []
    run_manifest_hashes = {}
    for item in sorted(manifest_entries, key=lambda value: value["path"]):
        run_path = WORKER_ATTEMPT / item["path"]
        require(run_path.stat().st_size == item.get("bytes"), f"run-manifest size: {run_path}")
        require(sha256(run_path) == item.get("sha256"), f"run-manifest hash: {run_path}")
        run_manifests.append(load_object(run_path))
        run_manifest_hashes[item["path"]] = item["sha256"]

    ttfb_values = sorted(
        run["metrics"]["ttfb_p95_ms"]["value"] for run in run_manifests
    )
    throughput_values = sorted(
        run["metrics"]["throughput_p50_bytes_per_s"]["value"]
        for run in run_manifests
    )
    ttfb_baseline_ms = ttfb_values[4]
    throughput_baseline_bytes_per_s = throughput_values[4]
    throughput_baseline_mbps = throughput_baseline_bytes_per_s * 8 / 1_000_000
    require(close(ttfb_baseline_ms, 666.9146721499995), "product TTFB baseline")
    require(
        close(throughput_baseline_bytes_per_s, 215609.1531419782),
        "product throughput baseline",
    )
    projection = [
        {
            "round": round_number,
            "ttfb_p95_ms": ttfb_baseline_ms * (0.839**round_number),
            "throughput_p50_mbps": throughput_baseline_mbps * (1.161**round_number),
        }
        for round_number in range(7)
    ]
    require(close(projection[6]["ttfb_p95_ms"], 232.6173141665131), "round-6 TTFB")
    require(
        close(projection[6]["throughput_p50_mbps"], 4.224258622309758),
        "round-6 throughput",
    )
    require(
        all(value is True for key, value in worker.get("security", {}).items() if isinstance(value, bool))
        and sum(isinstance(value, bool) for value in worker.get("security", {}).values()) == 15,
        "security flags",
    )
    require(
        audit.get("local_finalization_complete") is True
        and audit.get("required_passes") == 10
        and audit.get("required_pending") == 0
        and audit.get("required_failures") == 0,
        "completion audit",
    )
    require(
        report_evidence.get("all_thresholds_have_improvement_fringes") is True,
        "parent report fringe evidence",
    )

    required_text = (
        "43.102 s",
        "36.162 s",
        "39.996 s",
        "33.557 s",
        "40.476 s",
        "98.814",
        "4/5 ordered",
        "10 / 10",
        "1,800 providers",
        "666.915 → 232.617 ms",
        "1.725 → 4.224 Mbps",
        "×0.839 / round",
        "×1.161 / round",
        "Projection, not a performance promise",
    )
    require(all(value in text for value in required_text), "rendered values")
    return {
        "schema": 1,
        "kind": "sim-latency-final-baseline-evidence",
        "report_sha256": sha256(REPORT),
        "evidence_report_sha256": sha256(EVIDENCE_REPORT),
        "sections": len(parser.section_visuals),
        "section_svg_counts": parser.section_visuals,
        "baseline_ids": sorted(parser.baseline_ids),
        "threshold_ids": sorted(parser.threshold_ids),
        "improvement_fringe_ids": sorted(parser.fringe_ids),
        "all_thresholds_have_significant_improvement_fringes": True,
        "evidence_sha256": {
            path.name: expected for path, expected in EXPECTED_HASHES.items()
        },
        "calibration": {
            "baseline_mean_ms": baseline_stats["mean"],
            "significantly_better_threshold_ms": selection[
                "baseline_mean_significantly_better_threshold_ms"
            ],
            "replicates": selection["replicate_count"],
            "takeover_margin": selection["takeover_margin"],
        },
        "production_staging": {
            "baseline_median_ms": diagnostics["baseline"]["raw_score"],
            "significantly_better_threshold_ms": diagnostics[
                "baseline_takeover_raw_max"
            ],
            "candidate_median_ms": score["raw_score"],
            "normalized_score": score["normalized_score"],
            "gates_passed": 6,
            "security_assertions_passed": 15,
        },
        "product_projection": {
            "kind": "illustrative_compounding_projection",
            "source": "median of nine authenticated production staging baseline run manifests",
            "ttfb_metric": "ttfb_p95_ms",
            "throughput_metric": "throughput_p50_bytes_per_s converted to decimal Mbps",
            "ttfb_round_multiplier": 0.839,
            "throughput_round_multiplier": 1.161,
            "rounds": projection,
            "run_manifest_sha256": run_manifest_hashes,
            "disclosure": "future gains are hypothetical and are not the competition takeover contract",
        },
        "completion_audit": {
            "required_passes": audit["required_passes"],
            "required_pending": audit["required_pending"],
            "required_failures": audit["required_failures"],
        },
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    if args.self_test:
        self_test()
        return 0
    print(json.dumps(validate(), indent=2, sort_keys=True, allow_nan=False))
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, TypeError, ValueError, json.JSONDecodeError, ValidationError) as exc:
        raise SystemExit(f"final baseline validation: {exc}") from exc
