#!/usr/bin/env python3
"""Expose the v5 shared-baseline campaign shell with stronger controls."""

from __future__ import annotations

import hashlib
import importlib.util
from pathlib import Path


ROOT = Path("/home/by/urnetwork/server/connect/sim-latency/eval-12c/final-calibration-p1800-cf0fd3a9")
V4_SOURCE = ROOT / "reference-requalification-v4/pilot-source.py"
V4_SOURCE_SHA256 = "f1d1fe7ebf1c36745feaafeceff23544a6f1ebdd0d5ddd2bcdfa3b1b661e78da"
V4_RUNTIME = ROOT / "reference-requalification-v4/pilot-runtime"
V5 = ROOT / "reference-requalification-v5"
V5_RUNTIME = V5 / "pilot-runtime"
V4_BETTER = ROOT / "reference-requalification-v2/better.patch"
V4_WORSE = ROOT / "reference-requalification-v3/worse.patch"
V5_BETTER = V5 / "better.patch"
V5_WORSE = V5 / "worse.patch"
RETIRED_COMMITMENTS = V5 / "retired-seed-commitments.json"
V4_BETTER_SHA256 = "5cfb3e4a3fa9c0ffb86e1d10fb276a3a92fdc10175a7d86440bbc2a543dd0987"
V4_WORSE_SHA256 = "fd789f47f41ade788edabfc71f95b545241ba03fc78aac52748f8535d7c9cb62"
V5_BETTER_SHA256 = "1a81e5a5fb7897cee38eb3952ed0db82a6cccb4a7821eb9db84d93eb55d9ff82"
V5_WORSE_SHA256 = "982b192198ffa63942db1804629844f1cf9801bd4a71f64d2847a305217257a0"
RETIRED_COMMITMENTS_SHA256 = "1a17718ead0b2d5114be670a2b155679c92ac95d79a58160b161c8f0b03a7a04"


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source_file:
        for block in iter(lambda: source_file.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def replace_exact(source: str, old: str, new: str, count: int, label: str) -> str:
    if source.count(old) != count:
        raise RuntimeError(f"reference-v5 pilot source: ambiguous transform: {label}")
    return source.replace(old, new)


def transformed_source() -> str:
    if not V4_SOURCE.is_file() or V4_SOURCE.is_symlink() or sha256(V4_SOURCE) != V4_SOURCE_SHA256:
        raise RuntimeError("reference-v5 pilot source: frozen v4 source adapter changed")
    for path, digest in ((V5_BETTER, V5_BETTER_SHA256), (V5_WORSE, V5_WORSE_SHA256)):
        if not path.is_file() or path.is_symlink() or sha256(path) != digest:
            raise RuntimeError(f"reference-v5 pilot source: control changed: {path}")
    if (
        not RETIRED_COMMITMENTS.is_file()
        or RETIRED_COMMITMENTS.is_symlink()
        or sha256(RETIRED_COMMITMENTS) != RETIRED_COMMITMENTS_SHA256
    ):
        raise RuntimeError("reference-v5 pilot source: retired commitment set changed")

    spec = importlib.util.spec_from_file_location("frozen_reference_v4_pilot_source", V4_SOURCE)
    if spec is None or spec.loader is None:
        raise RuntimeError("reference-v5 pilot source: cannot load v4 source adapter")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    source = module.transformed_source()
    source = replace_exact(source, f"readonly ROOT={V4_RUNTIME}", f"readonly ROOT={V5_RUNTIME}", 1, "runtime root")
    source = replace_exact(
        source,
        "sim-latency-season-1-reference-v4-pilot",
        "sim-latency-season-1-reference-v5-pilot",
        1,
        "competition identity",
    )
    source = replace_exact(source, str(V4_BETTER), str(V5_BETTER), 1, "better path")
    source = replace_exact(source, str(V4_WORSE), str(V5_WORSE), 1, "worse path")
    source = replace_exact(source, V4_BETTER_SHA256, V5_BETTER_SHA256, 2, "better digest")
    source = replace_exact(source, V4_WORSE_SHA256, V5_WORSE_SHA256, 2, "worse digest")
    source = replace_exact(
        source,
        f"readonly WORSE_SHA={V5_WORSE_SHA256}",
        f"readonly WORSE_SHA={V5_WORSE_SHA256}\n"
        f'readonly RETIRED_COMMITMENTS="{RETIRED_COMMITMENTS}"\n'
        f"readonly RETIRED_COMMITMENTS_SHA={RETIRED_COMMITMENTS_SHA256}",
        1,
        "retired commitment constants",
    )
    source = replace_exact(
        source,
        '    [ "$(sha256_file "$WORSE_PATCH")" = "$WORSE_SHA" ] || die "worse patch changed"',
        '    [ "$(sha256_file "$WORSE_PATCH")" = "$WORSE_SHA" ] || die "worse patch changed"\n'
        '    [ -f "$RETIRED_COMMITMENTS" ] && [ ! -L "$RETIRED_COMMITMENTS" ] || die "retired commitment set is unsafe"\n'
        '    [ "$(sha256_file "$RETIRED_COMMITMENTS")" = "$RETIRED_COMMITMENTS_SHA" ] || die "retired commitment set changed"\n'
        '    jq -e \'.kind == "sim-latency-retired-seed-commitment-set" and .seed_material_included == false and .commitment_count == 21 and (.commitments | length) == 21 and (.commitments | length) == (.commitments | unique | length)\' "$RETIRED_COMMITMENTS" >/dev/null || die "retired commitment set is invalid"',
        1,
        "retired commitment preflight",
    )
    source = replace_exact(
        source,
        'log "the fresh pilot seed was committed before reference evaluation"',
        'jq -e --slurpfile retired "$RETIRED_COMMITMENTS" \'.seeds | all(.seed_commitment as $candidate | ($retired[0].commitments | index($candidate) | not))\' "$commitment" >/dev/null || die "retired seed commitment reused"\n'
        'log "the fresh pilot seed was committed before reference evaluation and excluded all retired commitments"',
        1,
        "pre-measurement seed exclusion",
    )
    return source
