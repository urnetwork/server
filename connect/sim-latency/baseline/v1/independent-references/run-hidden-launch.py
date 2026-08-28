#!/usr/bin/env python3
"""Execute the v5 five-seed hidden campaign with stronger controls."""

from __future__ import annotations

import hashlib
from pathlib import Path


ROOT = Path("/home/by/urnetwork/server/connect/sim-latency/eval-12c/final-calibration-p1800-cf0fd3a9")
PARENT = ROOT / "reference-requalification-v2/run-hidden-launch.py"
PARENT_SHA256 = "7cfde29ffbdda5e21dce411174b94bfbaebf299947677bb7797ce1fb6073542b"
PILOT_SOURCE_SHA256 = "c347d61a959d4ba18282737077a22ec13b14c152e52b8a8ad4b351f2d0e626a2"
PILOT_TRANSFORMED_SOURCE_SHA256 = "77c66465fb27b4bd8ee84aede3f03803349e1d62c9bb824396a224a8889f9078"
OFFICIAL_PILOT_RUNNER_SHA256 = "4a290509307a84fadf0866e1836c617f78e1f7b01fd00a8fd4f5bc2afc50a17f"
QUALIFICATION_SHA256 = "8bdc86dcf68a8f8a4c686d8d6267510e121ab7800c9bbcc7cfa4dbce1ac1ca10"
DESIGN_SHA256 = "6e05b1872648a0d9f28755e5ca4b0470445ea40e95b4fdb991e676d2d453ffa1"
STATIC_QUALIFICATION_SHA256 = "4b51548ff4910cd8d1b79247973cd47e65680b814b4ffa5c1b9153bf61d718fd"
PILOT_RETIRED_COMMITMENTS_SHA256 = "1a17718ead0b2d5114be670a2b155679c92ac95d79a58160b161c8f0b03a7a04"
RETIRED_COMMITMENTS_SHA256 = "4fe791ae5cc6fd838fad7cba6727c7325f27e5dda8fb5f4731b15359ddbd7eaf"
V4_REJECTION_SHA256 = "d1a782831e9cfedfbe9c5835385f490e655ca38856698b88c59ee91f1ca993e1"
BETTER_SHA256 = "1a81e5a5fb7897cee38eb3952ed0db82a6cccb4a7821eb9db84d93eb55d9ff82"
WORSE_SHA256 = "982b192198ffa63942db1804629844f1cf9801bd4a71f64d2847a305217257a0"
V2_INVALID_REJECTION_SHA256 = "89205a071866417fd768364539aa8092eea5d3bd5156a6507969fa37abe6b4a4"
V3_REJECTION_SHA256 = "f160ef6188e3f962317355a6971cea8b8dbadfb84f62d499be7f5f49cc39620c"
ORDERING_METRIC = "candidate_raw_score_ms_over_designated_baseline_raw_score_ms"


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source_file:
        for block in iter(lambda: source_file.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def replace_exact(source: str, old: str, new: str, count: int, label: str) -> str:
    if source.count(old) != count:
        raise SystemExit(f"reference-v5 hidden wrapper: ambiguous transform: {label}")
    return source.replace(old, new)


if not PARENT.is_file() or PARENT.is_symlink() or sha256(PARENT) != PARENT_SHA256:
    raise SystemExit("reference-v5 hidden wrapper: frozen parent runner changed")

source = PARENT.read_text(encoding="utf-8")
source = replace_exact(
    source,
    "reference-requalification-v2",
    "reference-requalification-v5",
    1,
    "artifact root",
)
source = replace_exact(source, "reference-v2", "reference-v5", 13, "v5 identity")
source = replace_exact(
    source,
    '            "the fresh pilot seed was committed before reference evaluation",',
    '            "the fresh pilot seed was committed before reference evaluation and excluded all retired commitments",',
    1,
    "pilot precommit log source",
)
source = replace_exact(
    source,
    '            "all five fresh hidden seeds were committed before reference evaluation",',
    '            "all five fresh hidden seeds were committed before reference evaluation and excluded all retired commitments",',
    1,
    "hidden precommit log",
)
source = replace_exact(
    source,
    '    for old, new, count, label in transforms:\n'
    '        source = replace_exact(source, old, new, count, label)\n'
    '    require("TARGET_SEEDS=1" not in source and "1/1 pilot" not in source, "stale pilot target")',
    '    for old, new, count, label in transforms:\n'
    '        source = replace_exact(source, old, new, count, label)\n'
    '    source = replace_exact(source, str(module.RETIRED_COMMITMENTS), str(RETIRED_COMMITMENTS), 1, "hidden retired commitment path")\n'
    '    source = replace_exact(source, module.RETIRED_COMMITMENTS_SHA256, RETIRED_COMMITMENTS_SHA256, 1, "hidden retired commitment hash")\n'
    '    source = replace_exact(source, ".commitment_count == 21 and (.commitments | length) == 21", ".commitment_count == 22 and (.commitments | length) == 22", 1, "hidden retired commitment count")\n'
    '    require("TARGET_SEEDS=1" not in source and "1/1 pilot" not in source, "stale pilot target")',
    1,
    "hidden retired commitment transform",
)
source = replace_exact(
    source,
    'PILOT_RUNNER = V2 / "run-pilot.py"',
    'PILOT_RUNNER = V2 / "pilot-source.py"\n'
    'OFFICIAL_PILOT_RUNNER = V2 / "run-pilot.py"\n'
    'QUALIFICATION = V2 / "qualification.json"\n'
    'DESIGN = V2 / "design.json"\n'
    'STATIC_QUALIFICATION = V2 / "static-qualification.json"\n'
    'PILOT_RETIRED_COMMITMENTS = V2 / "retired-seed-commitments.json"\n'
    'RETIRED_COMMITMENTS = V2 / "retired-seed-commitments-before-hidden.json"\n'
    'V4_REJECTION = ROOT / "reference-requalification-v4/hidden-campaign-rejection.json"\n'
    'V2_INVALID_REJECTION = ROOT / "reference-requalification-v2/pilot-attempt-02-invalid-worse-rejection.json"\n'
    'V3_REJECTION = ROOT / "reference-requalification-v3/pilot-rejection.json"',
    1,
    "pilot lineage paths",
)
source = replace_exact(
    source,
    'BETTER = V2 / "better.patch"',
    'BETTER = V2 / "better.patch"',
    1,
    "qualified better path",
)
source = replace_exact(
    source,
    'WORSE = V2 / "worse.patch"',
    'WORSE = V2 / "worse.patch"',
    1,
    "qualified worse path",
)
source = replace_exact(
    source,
    'NOOP = SERVER / "competition/references/noop.patch"',
    'NOOP = V2 / "noop.patch"',
    1,
    "qualified noop path",
)
source = replace_exact(
    source,
    'PILOT_RUNNER_SHA256 = "7f10e722867b13a02686840f19425d3edf6152223487931762e91e15938d121f"',
    f'PILOT_RUNNER_SHA256 = "{PILOT_SOURCE_SHA256}"\n'
    f'PILOT_TRANSFORMED_SOURCE_SHA256 = "{PILOT_TRANSFORMED_SOURCE_SHA256}"\n'
    f'OFFICIAL_PILOT_RUNNER_SHA256 = "{OFFICIAL_PILOT_RUNNER_SHA256}"\n'
    f'QUALIFICATION_SHA256 = "{QUALIFICATION_SHA256}"\n'
    f'DESIGN_SHA256 = "{DESIGN_SHA256}"\n'
    f'STATIC_QUALIFICATION_SHA256 = "{STATIC_QUALIFICATION_SHA256}"\n'
    f'PILOT_RETIRED_COMMITMENTS_SHA256 = "{PILOT_RETIRED_COMMITMENTS_SHA256}"\n'
    f'RETIRED_COMMITMENTS_SHA256 = "{RETIRED_COMMITMENTS_SHA256}"\n'
    f'V4_REJECTION_SHA256 = "{V4_REJECTION_SHA256}"\n'
    f'V2_INVALID_REJECTION_SHA256 = "{V2_INVALID_REJECTION_SHA256}"\n'
    f'V3_REJECTION_SHA256 = "{V3_REJECTION_SHA256}"\n'
    f'ORDERING_METRIC = "{ORDERING_METRIC}"',
    1,
    "pilot lineage hashes",
)
source = replace_exact(
    source,
    'BETTER_SHA256 = "5cfb3e4a3fa9c0ffb86e1d10fb276a3a92fdc10175a7d86440bbc2a543dd0987"',
    f'BETTER_SHA256 = "{BETTER_SHA256}"',
    1,
    "better hash",
)
source = replace_exact(
    source,
    'WORSE_SHA256 = "6239ab98018536a30a3842fae51c6df4ecd74c6626e9969f427117d7edefadb2"',
    f'WORSE_SHA256 = "{WORSE_SHA256}"',
    1,
    "worse hash",
)
source = replace_exact(
    source,
    '        PILOT_RUNNER: PILOT_RUNNER_SHA256,\n        SOURCE_LOCK: SOURCE_LOCK_SHA256,',
    '        PILOT_RUNNER: PILOT_RUNNER_SHA256,\n'
    '        OFFICIAL_PILOT_RUNNER: OFFICIAL_PILOT_RUNNER_SHA256,\n'
    '        QUALIFICATION: QUALIFICATION_SHA256,\n'
    '        DESIGN: DESIGN_SHA256,\n'
    '        STATIC_QUALIFICATION: STATIC_QUALIFICATION_SHA256,\n'
    '        PILOT_RETIRED_COMMITMENTS: PILOT_RETIRED_COMMITMENTS_SHA256,\n'
    '        RETIRED_COMMITMENTS: RETIRED_COMMITMENTS_SHA256,\n'
    '        V4_REJECTION: V4_REJECTION_SHA256,\n'
    '        V2_INVALID_REJECTION: V2_INVALID_REJECTION_SHA256,\n'
    '        V3_REJECTION: V3_REJECTION_SHA256,\n'
    '        SOURCE_LOCK: SOURCE_LOCK_SHA256,',
    1,
    "static lineage",
)
source = replace_exact(
    source,
    '    pilot_sha = sha256(PILOT_DECISION)\n'
    '    pilot = load_json(PILOT_DECISION)\n'
    '    require(',
    '    pilot_sha = sha256(PILOT_DECISION)\n'
    '    pilot = load_json(PILOT_DECISION)\n'
    '    qualification = load_json(QUALIFICATION)\n'
    '    design = load_json(DESIGN)\n'
    '    static_qualification = load_json(STATIC_QUALIFICATION)\n'
    '    pilot_retired = load_json(PILOT_RETIRED_COMMITMENTS)\n'
    '    retired = load_json(RETIRED_COMMITMENTS)\n'
    '    v4_rejection = load_json(V4_REJECTION)\n'
    '    pilot_reveal_path = V2 / "pilot-seed-reveal.json"\n'
    '    pilot_reveal = load_json(pilot_reveal_path)\n'
    '    pilot_result_path = V2 / "pilot-runtime/independent-references/seed-01/seed-result.json"\n'
    '    pilot_commitment_path = V2 / "pilot-runtime/independent-references/campaign-commitment.json"\n'
    '    pilot_commitment = pilot_reveal.get("seed_commitment")\n'
    '    require(\n'
    '        qualification.get("kind") == "sim-latency-reference-v5-pilot-qualification"\n'
    '        and qualification.get("draft") is False\n'
    '        and qualification.get("accepted_for_hidden_five_seed_screen") is True\n'
    '        and qualification.get("official_reference_set_accepted") is False\n'
    '        and qualification.get("pilot_decision_sha256") == pilot_sha\n'
    '        and qualification.get("pilot_seed_reused") is False\n'
    '        and qualification.get("strict_ordering_passed") is True\n'
    '        and qualification.get("placeability_is_diagnostic_only") is True\n'
    '        and qualification.get("fresh_hidden_seed_material_created") is False,\n'
    '        "pilot qualification",\n'
    '    )\n'
    '    require(\n'
    '        design.get("kind") == "sim-latency-reference-v5-design"\n'
    '        and design.get("draft") is False\n'
    '        and design.get("authorized") is True\n'
    '        and design.get("ordering_metric") == ORDERING_METRIC\n'
    '        and design.get("v4_hidden_campaign_rejection_sha256") == V4_REJECTION_SHA256,\n'
    '        "v5 design",\n'
    '    )\n'
    '    require(\n'
    '        static_qualification.get("kind") == "sim-latency-reference-v5-static-qualification"\n'
    '        and static_qualification.get("accepted_for_full_scale_evaluator_pilot") is True\n'
    '        and static_qualification.get("official_reference_set_accepted") is False,\n'
    '        "static qualification",\n'
    '    )\n'
    '    require(\n'
    '        v4_rejection.get("kind") == "sim-latency-reference-v4-hidden-campaign-rejection"\n'
    '        and v4_rejection.get("accepted") is False\n'
    '        and v4_rejection.get("completed_seeds") == 5\n'
    '        and v4_rejection.get("all_precommitted_measurements_retained") is True,\n'
    '        "v4 rejection",\n'
    '    )\n'
    '    prior_values = pilot_retired.get("commitments")\n'
    '    retired_values = retired.get("commitments")\n'
    '    require(\n'
    '        pilot_retired.get("commitment_count") == 21\n'
    '        and isinstance(prior_values, list)\n'
    '        and len(prior_values) == len(set(prior_values)) == 21\n'
    '        and retired.get("commitment_count") == 22\n'
    '        and isinstance(retired_values, list)\n'
    '        and len(retired_values) == len(set(retired_values)) == 22\n'
    '        and isinstance(pilot_commitment, str)\n'
    '        and pilot_commitment not in prior_values\n'
    '        and sorted(retired_values) == sorted(prior_values + [pilot_commitment])\n'
    '        and retired.get("seed_material_included") is False,\n'
    '        "pre-hidden retired commitment set",\n'
    '    )\n'
    '    require(\n'
    '        pilot_result_path.is_file()\n'
    '        and sha256(pilot_result_path) == pilot.get("seed_result_sha256")\n'
    '        and sha256(pilot_reveal_path) == pilot.get("seed_reveal_sha256")\n'
    '        and sha256(pilot_commitment_path) == pilot.get("campaign_commitment_sha256"),\n'
    '        "pilot terminal artifact chain",\n'
    '    )\n'
    '    require(',
    1,
    "qualified pilot semantics",
)
source = replace_exact(
    source,
    '        pilot.get("kind") == "sim-latency-reference-v5-pilot-decision"\n'
    '        and pilot.get("accepted") is True\n'
    '        and pilot.get("pilot_seed_reuse_in_hidden_campaign_forbidden") is True,',
    '        pilot.get("kind") == "sim-latency-reference-v5-pilot-decision"\n'
    '        and pilot.get("accepted") is True\n'
    '        and pilot.get("runner_sha256") == OFFICIAL_PILOT_RUNNER_SHA256\n'
    '        and pilot.get("ordering_metric") == ORDERING_METRIC\n'
    '        and pilot.get("transformed_campaign_shell_sha256") == PILOT_TRANSFORMED_SOURCE_SHA256\n'
    '        and pilot.get("design_sha256") == DESIGN_SHA256\n'
    '        and pilot.get("v2_invalid_pilot_rejection_sha256") == V2_INVALID_REJECTION_SHA256\n'
    '        and pilot.get("v3_pilot_rejection_sha256") == V3_REJECTION_SHA256\n'
    '        and pilot.get("v4_hidden_campaign_rejection_sha256") == V4_REJECTION_SHA256\n'
    '        and pilot.get("static_qualification_sha256") == STATIC_QUALIFICATION_SHA256\n'
    '        and pilot.get("retired_seed_commitments_sha256") == PILOT_RETIRED_COMMITMENTS_SHA256\n'
    '        and pilot.get("placeability_is_diagnostic_only") is True\n'
    '        and pilot.get("strict_ordering_passed") is True\n'
    '        and pilot.get("cleanup", {}).get("residual_competition_containers") == 0\n'
    '        and pilot.get("cleanup", {}).get("residual_competition_networks") == 0\n'
    '        and pilot.get("pilot_seed_reuse_in_hidden_campaign_forbidden") is True,',
    1,
    "pilot decision lineage",
)
source = replace_exact(
    source,
    '        and amendment.get("pilot_decision_sha256") == pilot_sha\n'
    '        and amendment.get("fresh_hidden_seed_material_created") is False,',
    '        and amendment.get("pilot_decision_sha256") == pilot_sha\n'
    '        and amendment.get("ordering_metric") == ORDERING_METRIC\n'
    '        and amendment.get("draft") is False\n'
    '        and amendment.get("design_sha256") == DESIGN_SHA256\n'
    '        and amendment.get("qualification_sha256") == QUALIFICATION_SHA256\n'
    '        and amendment.get("v2_invalid_pilot_rejection_sha256") == V2_INVALID_REJECTION_SHA256\n'
    '        and amendment.get("v3_pilot_rejection_sha256") == V3_REJECTION_SHA256\n'
    '        and amendment.get("v4_hidden_campaign_rejection_sha256") == V4_REJECTION_SHA256\n'
    '        and amendment.get("static_qualification_sha256") == STATIC_QUALIFICATION_SHA256\n'
    '        and amendment.get("retired_seed_commitments_sha256") == RETIRED_COMMITMENTS_SHA256\n'
    '        and amendment.get("placeability_is_diagnostic_only") is True\n'
    '        and amendment.get("fresh_hidden_seed_material_created") is False,',
    1,
    "amendment lineage",
)
source = replace_exact(
    source,
    '        and protocol.get("measurement_amendment_sha256") == amendment_sha\n'
    '        and protocol.get("patch_sha256")',
    '        and protocol.get("measurement_amendment_sha256") == amendment_sha\n'
    '        and protocol.get("ordering_metric") == ORDERING_METRIC\n'
    '        and protocol.get("draft") is False\n'
    '        and protocol.get("design_sha256") == DESIGN_SHA256\n'
    '        and protocol.get("qualification_sha256") == QUALIFICATION_SHA256\n'
    '        and protocol.get("v2_invalid_pilot_rejection_sha256") == V2_INVALID_REJECTION_SHA256\n'
    '        and protocol.get("v3_pilot_rejection_sha256") == V3_REJECTION_SHA256\n'
    '        and protocol.get("v4_hidden_campaign_rejection_sha256") == V4_REJECTION_SHA256\n'
    '        and protocol.get("static_qualification_sha256") == STATIC_QUALIFICATION_SHA256\n'
    '        and protocol.get("retired_seed_commitments_sha256") == RETIRED_COMMITMENTS_SHA256\n'
    '        and protocol.get("placeability_is_diagnostic_only") is True\n'
    '        and protocol.get("patch_sha256")',
    1,
    "protocol lineage",
)
source = replace_exact(
    source,
    '        "v1_rejection_sha256": V1_REJECTION_SHA256,\n        "cleanup": {',
    '        "v1_rejection_sha256": V1_REJECTION_SHA256,\n'
    '        "v2_invalid_pilot_rejection_sha256": V2_INVALID_REJECTION_SHA256,\n'
    '        "v3_pilot_rejection_sha256": V3_REJECTION_SHA256,\n'
    '        "v4_hidden_campaign_rejection_sha256": V4_REJECTION_SHA256,\n'
    '        "static_qualification_sha256": STATIC_QUALIFICATION_SHA256,\n'
    '        "retired_seed_commitments_sha256": RETIRED_COMMITMENTS_SHA256,\n'
    '        "design_sha256": DESIGN_SHA256,\n'
    '        "ordering_metric": ORDERING_METRIC,\n'
    '        "one_designated_same_round_baseline_per_seed": True,\n'
    '        "placeability_is_diagnostic_only": True,\n'
    '        "cleanup": {',
    1,
    "terminal decision lineage",
)
source = replace_exact(
    source,
    '        "v1_rejection_sha256": V1_REJECTION_SHA256,\n    }\n    atomic_json(FINAL_ATTESTATION, attestation)',
    '        "v1_rejection_sha256": V1_REJECTION_SHA256,\n'
    '        "v2_invalid_pilot_rejection_sha256": V2_INVALID_REJECTION_SHA256,\n'
    '        "v3_pilot_rejection_sha256": V3_REJECTION_SHA256,\n'
    '        "v4_hidden_campaign_rejection_sha256": V4_REJECTION_SHA256,\n'
    '        "static_qualification_sha256": STATIC_QUALIFICATION_SHA256,\n'
    '        "retired_seed_commitments_sha256": RETIRED_COMMITMENTS_SHA256,\n'
    '        "design_sha256": DESIGN_SHA256,\n'
    '        "ordering_metric": ORDERING_METRIC,\n'
    '        "one_designated_same_round_baseline_per_seed": True,\n'
    '        "placeability_is_diagnostic_only": True,\n'
    '    }\n    atomic_json(FINAL_ATTESTATION, attestation)',
    1,
    "final attestation lineage",
)
source = replace_exact(
    source,
    'f"readonly ROOT={module.RUNTIME}"',
    'f"readonly ROOT={module.V5_RUNTIME}"',
    1,
    "pilot source runtime compatibility",
)
source = replace_exact(
    source,
    '    ratios: dict[str, float] = {}\n    for name, patch_sha in expected.items():',
    '    designated = result.get("designated_baseline")\n'
    '    require(isinstance(designated, dict), f"seed {index}: designated baseline")\n'
    '    designated_raw = designated.get("raw_score_ms")\n'
    '    require(isinstance(designated_raw, (int, float)) and math.isfinite(designated_raw) and designated_raw > 0, f"seed {index}: designated baseline score")\n'
    '    ratios: dict[str, float] = {}\n'
    '    for name, patch_sha in expected.items():',
    1,
    "shared baseline verifier setup",
)
source = replace_exact(
    source,
    '        ratio = entry.get("paired_ratio")\n'
    '        require(isinstance(ratio, (int, float)) and math.isfinite(ratio) and ratio > 0, f"seed {index} {name}: ratio")\n'
    '        ratios[name] = float(ratio)',
    '        paired = entry.get("paired_ratio")\n'
    '        require(isinstance(paired, (int, float)) and math.isfinite(paired) and paired > 0, f"seed {index} {name}: paired ratio diagnostic")\n'
    '        candidate = entry.get("candidate_raw_score_ms")\n'
    '        require(isinstance(candidate, (int, float)) and math.isfinite(candidate) and candidate > 0, f"seed {index} {name}: candidate score")\n'
    '        ratios[name] = float(candidate) / float(designated_raw)',
    1,
    "shared baseline ratios",
)
source = replace_exact(
    source,
    '    require(result.get("ordering_passed") is (ratios["better"] < ratios["noop"] < ratios["worse"]), f"seed {index}: ordering arithmetic")',
    '    require(result.get("ordering_metric") == ORDERING_METRIC, f"seed {index}: ordering metric")\n'
    '    require(result.get("ordering_passed") is (ratios["better"] < ratios["noop"] < ratios["worse"]), f"seed {index}: ordering arithmetic")',
    1,
    "shared baseline ordering verification",
)
source = replace_exact(
    source,
    '                "ratios": {\n'
    '                    name: result["references"][name]["paired_ratio"]\n'
    '                    for name in ("better", "noop", "worse")\n'
    '                },',
    '                "ratios": {\n'
    '                    name: result["references"][name]["candidate_raw_score_ms"] / result["designated_baseline"]["raw_score_ms"]\n'
    '                    for name in ("better", "noop", "worse")\n'
    '                },\n'
    '                "private_paired_ratios_diagnostic_only": {\n'
    '                    name: result["references"][name]["paired_ratio"]\n'
    '                    for name in ("better", "noop", "worse")\n'
    '                },',
    1,
    "terminal shared ratios",
)
source = replace_exact(
    source,
    '        require(private.get("seed_commitment") != load_json(V2 / "pilot-seed-reveal.json").get("seed_commitment"), f"seed {index}: pilot reuse")',
    '        seed_commitment = private.get("seed_commitment")\n'
    '        require(seed_commitment != load_json(V2 / "pilot-seed-reveal.json").get("seed_commitment"), f"seed {index}: pilot reuse")\n'
    '        retired_values = load_json(RETIRED_COMMITMENTS).get("commitments")\n'
    '        require(isinstance(retired_values, list) and all(isinstance(value, str) and len(value) == 64 for value in retired_values) and len(retired_values) == len(set(retired_values)) == 22 and seed_commitment not in retired_values, f"seed {index}: prior seed reuse")',
    1,
    "all prior seed reuse exclusion",
)
source = replace_exact(
    source,
    '            "seed_material_retired": True,',
    '            "seed_material_retired": True,\n'
    '            "pilot_seed_reuse_forbidden": True,\n'
    '            "all_prior_seed_reuse_forbidden": True,\n'
    '            "retired_seed_commitments_sha256": RETIRED_COMMITMENTS_SHA256,',
    1,
    "hidden reveal retirement lineage",
)

exec(compile(source, str(Path(__file__).resolve()), "exec"), {"__file__": __file__, "__name__": "__main__"})
