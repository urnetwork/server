#!/usr/bin/env python3
"""Authenticate and summarize a completed local sim-latency campaign.

This is deliberately independent of baseline.go's stored summaries: it hashes
each CSV and run manifest, validates the completion marker, reparses every CSV,
and recomputes the Apex failure-ceiling p95. It then cross-checks baseline.json
and folds in the campaign's local resource telemetry.
"""

from __future__ import annotations

import argparse
import csv
import hashlib
import io
import json
import math
import random
import re
import statistics
import subprocess
from datetime import datetime, timezone
from html import escape
from pathlib import Path
from typing import Any, Iterable


SUMMARY_SCHEMA = 1
SUMMARY_KIND = "sim-latency-local-baseline-summary"
RUN_KIND = "sim-latency-run"
MARKER_KIND = "sim-latency-complete"
BASELINE_KIND = "sim-latency-baseline"
EXPECTED_CSV_COLUMNS = (
    "t_start_ms",
    "client",
    "path",
    "depth",
    "status",
    "bytes",
    "ttfb_ms",
    "total_ms",
    "bytes_per_s",
)
RSS_TELEMETRY_COLUMNS = (
    "unix_s",
    "sim_rss_kb",
    "docker_rss_kb",
    "pg_mem_mb",
    "redis_mem_mb",
)
HOST_TELEMETRY_COLUMNS = (
    "unix_s",
    "sim_processes",
    "sim_cpu_pct",
    "sim_rss_kb",
    "load1",
    "mem_available_kb",
    "swap_used_kb",
    "tcp_established",
)
SERVICE_TELEMETRY_COLUMNS = (
    "unix_s",
    "postgres_processes",
    "postgres_summed_rss_kb",
    "redis_processes",
    "redis_summed_rss_kb",
)
TELEMETRY_INTEGER_COLUMNS = {
    "sim_processes",
    "sim_rss_kb",
    "docker_rss_kb",
    "mem_available_kb",
    "swap_used_kb",
    "tcp_established",
    "postgres_processes",
    "postgres_summed_rss_kb",
    "redis_processes",
    "redis_summed_rss_kb",
}
METRIC_ORDER = (
    "apex_raw_score_ms",
    "success_rate",
    "request_count",
    "received_bytes",
    "fail_rate",
    "ttfb_p50_ms",
    "ttfb_p95_ms",
    "total_p50_ms",
    "total_p95_ms",
    "throughput_p05_bytes_per_s",
    "throughput_p50_bytes_per_s",
    "throughput_p95_bytes_per_s",
    "goodput_bytes_per_s",
)
STABILITY_METRICS = (
    ("ttfb_p95_ms", "TTFB p95", "ms", 1.0),
    ("throughput_p50_bytes_per_s", "Throughput p50", "kB/s", 1000.0),
    ("throughput_p05_bytes_per_s", "Throughput p05", "kB/s", 1000.0),
)
OUTLIER_METRICS = (
    "apex_raw_score_ms",
    "success_rate",
    "request_count",
    "received_bytes",
    "ttfb_p95_ms",
    "total_p95_ms",
    "throughput_p05_bytes_per_s",
    "throughput_p50_bytes_per_s",
)


class SummaryError(RuntimeError):
    pass


def strict_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    out: dict[str, Any] = {}
    for key, value in pairs:
        if key in out:
            raise SummaryError(f"duplicate JSON key: {key}")
        out[key] = value
    return out


def reject_json_constant(value: str) -> Any:
    raise SummaryError(f"non-standard JSON constant: {value}")


def parse_json_bytes(payload: bytes, path: Path) -> dict[str, Any]:
    try:
        value = json.loads(
            payload.decode("utf-8"),
            object_pairs_hook=strict_object,
            parse_constant=reject_json_constant,
        )
    except (UnicodeDecodeError, ValueError) as exc:
        raise SummaryError(f"read {path}: {exc}") from exc
    if not isinstance(value, dict):
        raise SummaryError(f"{path}: top-level JSON value is not an object")
    return value


def read_json_bytes(path: Path) -> tuple[dict[str, Any], bytes]:
    try:
        payload = path.read_bytes()
    except OSError as exc:
        raise SummaryError(f"read {path}: {exc}") from exc
    return parse_json_bytes(payload, path), payload


def read_json(path: Path) -> dict[str, Any]:
    value, _ = read_json_bytes(path)
    return value


def payload_fingerprint(path: Path, payload: bytes) -> dict[str, Any]:
    return {
        "path": str(path),
        "bytes": len(payload),
        "sha256": hashlib.sha256(payload).hexdigest(),
    }


def file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    try:
        with path.open("rb") as handle:
            for block in iter(lambda: handle.read(1024 * 1024), b""):
                digest.update(block)
    except OSError as exc:
        raise SummaryError(f"hash {path}: {exc}") from exc
    return digest.hexdigest()


def require(condition: bool, message: str) -> None:
    if not condition:
        raise SummaryError(message)


def finite_number(value: Any, label: str) -> float:
    if isinstance(value, bool):
        raise SummaryError(f"{label}: boolean is not numeric")
    try:
        number = float(value)
    except (TypeError, ValueError) as exc:
        raise SummaryError(f"{label}: not numeric") from exc
    if not math.isfinite(number):
        raise SummaryError(f"{label}: non-finite")
    return number


def integer(value: Any, label: str) -> int:
    if isinstance(value, bool):
        raise SummaryError(f"{label}: boolean is not an integer")
    try:
        number = int(value)
    except (TypeError, ValueError) as exc:
        raise SummaryError(f"{label}: not an integer") from exc
    if str(number) != str(value).strip() and not isinstance(value, int):
        raise SummaryError(f"{label}: not an exact integer")
    return number


def type7_quantile(values: Iterable[float], q: float) -> float:
    ordered = sorted(values)
    if not ordered:
        raise SummaryError("cannot compute a quantile of an empty sequence")
    h = (len(ordered) - 1) * q
    lower = math.floor(h)
    upper = math.ceil(h)
    if lower == upper:
        return ordered[lower]
    return ordered[lower] + (h - lower) * (ordered[upper] - ordered[lower])


def beta_continued_fraction(a: float, b: float, x: float) -> float:
    """Incomplete-beta continued fraction via the modified Lentz method."""
    tiny = 1e-300
    qab = a + b
    qap = a + 1
    qam = a - 1
    c = 1.0
    d = 1 - qab * x / qap
    if abs(d) < tiny:
        d = tiny
    d = 1 / d
    result = d
    for iteration in range(1, 501):
        fm = float(iteration)
        m2 = 2 * fm
        numerator = fm * (b - fm) * x / ((qam + m2) * (a + m2))
        d = 1 + numerator * d
        if abs(d) < tiny:
            d = tiny
        c = 1 + numerator / c
        if abs(c) < tiny:
            c = tiny
        d = 1 / d
        result *= d * c

        numerator = -(a + fm) * (qab + fm) * x / (
            (a + m2) * (qap + m2)
        )
        d = 1 + numerator * d
        if abs(d) < tiny:
            d = tiny
        c = 1 + numerator / c
        if abs(c) < tiny:
            c = tiny
        d = 1 / d
        delta = d * c
        result *= delta
        if abs(delta - 1) < 1e-14:
            break
    return result


def regularized_incomplete_beta(a: float, b: float, x: float) -> float:
    if x <= 0:
        return 0.0
    if x >= 1:
        return 1.0
    front = math.exp(
        math.lgamma(a + b)
        - math.lgamma(a)
        - math.lgamma(b)
        + a * math.log(x)
        + b * math.log1p(-x)
    )
    if x < (a + 1) / (a + b + 2):
        return front * beta_continued_fraction(a, b, x) / a
    return 1 - front * beta_continued_fraction(b, a, 1 - x) / b


def student_t_survival(t_value: float, degrees_freedom: float) -> float:
    require(degrees_freedom > 0, "Student-t degrees of freedom must be positive")
    if math.isinf(t_value):
        return 0.0 if t_value > 0 else 1.0
    x = degrees_freedom / (degrees_freedom + t_value * t_value)
    probability = 0.5 * regularized_incomplete_beta(
        degrees_freedom / 2, 0.5, x
    )
    return 1 - probability if t_value < 0 else probability


def student_t_critical(alpha: float, degrees_freedom: float) -> float:
    require(0 < alpha < 1, "Student-t alpha must be inside (0, 1)")
    require(degrees_freedom > 0, "Student-t degrees of freedom must be positive")
    if alpha == 0.5:
        return 0.0
    lower, upper = -1e6, 1e6
    for _ in range(200):
        midpoint = (lower + upper) / 2
        if student_t_survival(midpoint, degrees_freedom) > alpha:
            lower = midpoint
        else:
            upper = midpoint
        if upper - lower < 1e-10:
            break
    return (lower + upper) / 2


def drift(
    values: list[float], x_values: list[float] | None = None
) -> dict[str, Any]:
    n = len(values)
    if n < 3:
        if x_values is None:
            return {"x_basis": "run_index", "slope_per_run": 0.0, "t_stat": 0.0}
        return {
            "x_basis": "measure_start_elapsed_hours",
            "slope_per_hour": 0.0,
            "t_stat": 0.0,
        }
    if x_values is None:
        xs = [float(i) for i in range(n)]
        x_basis = "run_index"
        slope_name = "slope_per_run"
    else:
        require(len(x_values) == n, "drift x/y length mismatch")
        origin = finite_number(x_values[0], "drift time origin")
        xs = [
            (finite_number(value, "drift timestamp") - origin) / 3_600_000
            for value in x_values
        ]
        require(
            all(left < right for left, right in zip(xs, xs[1:])),
            "drift timestamps are not strictly increasing",
        )
        x_basis = "measure_start_elapsed_hours"
        slope_name = "slope_per_hour"
    x_mean = statistics.fmean(xs)
    y_mean = statistics.fmean(values)
    sxx = sum((x - x_mean) ** 2 for x in xs)
    slope = sum((x - x_mean) * (y - y_mean) for x, y in zip(xs, values)) / sxx
    intercept = y_mean - slope * x_mean
    residual_ss = sum(
        (y - (intercept + slope * x)) ** 2 for x, y in zip(xs, values)
    )
    if residual_ss == 0:
        t_stat = 0.0 if slope == 0 else None
        infinite_direction = 0 if slope == 0 else (1 if 0 < slope else -1)
    else:
        slope_se = math.sqrt((residual_ss / (n - 2)) / sxx)
        t_stat = slope / slope_se
        infinite_direction = 0
    return {
        "x_basis": x_basis,
        slope_name: slope,
        "t_stat": t_stat,
        "perfect_fit_infinite_t_direction": infinite_direction,
    }


def describe(
    values: list[float], x_values: list[float] | None = None
) -> dict[str, Any]:
    require(bool(values), "cannot summarize an empty metric")
    mean = statistics.fmean(values)
    sd = statistics.stdev(values) if len(values) >= 2 else 0.0
    return {
        "n": len(values),
        "mean": mean,
        "sd": sd,
        "cv": sd / abs(mean) if mean else 0.0,
        "median": statistics.median(values),
        "min": min(values),
        "max": max(values),
        "drift": drift(values, x_values),
    }


def bootstrap_median_noise(values: list[float]) -> dict[str, dict[str, float]]:
    if len(values) < 3:
        return {}
    rng = random.Random(48)
    result: dict[str, dict[str, float]] = {}
    for replicate_count in (1, 3, 5, 7):
        if len(values) < replicate_count:
            continue
        medians = [
            statistics.median(rng.choices(values, k=replicate_count))
            for _ in range(20_000)
        ]
        mean = statistics.fmean(medians)
        sd = statistics.stdev(medians)
        result[str(replicate_count)] = {
            "mean": mean,
            "sd": sd,
            "cv": sd / abs(mean) if mean else 0.0,
        }
    return result


def sd_convergence(values: list[float]) -> dict[str, Any]:
    require(len(values) >= 3, "need at least three values for SD convergence")
    series = [statistics.stdev(values[:k]) for k in range(2, len(values) + 1)]
    first_index = max(0, len(series) - 5)
    tail = series[first_index:]
    final_sd = series[-1]
    relative_span = (
        (max(tail) - min(tail)) / abs(final_sd) if final_sd != 0 else 0.0
    )
    return {
        "replicate_counts": list(range(2, len(values) + 1)),
        "sd_by_replicates": series,
        "last_five_replicate_counts": list(
            range(first_index + 2, first_index + 2 + len(tail))
        ),
        "last_five_sd": tail,
        "last_five_relative_span": relative_span,
        "last_five_within_10_percent_span": relative_span <= 0.10,
    }


def robust_outlier_candidates(runs: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """Flag, but never auto-exclude, extreme run metrics using modified z."""
    findings: list[dict[str, Any]] = []
    for metric_name in OUTLIER_METRICS:
        values = [
            finite_number(run["metrics"].get(metric_name), f"{run['tag']} {metric_name}")
            for run in runs
        ]
        median = statistics.median(values)
        deviations = [abs(value - median) for value in values]
        mad = statistics.median(deviations)
        if mad == 0:
            for run, value in zip(runs, values):
                if value == median:
                    continue
                findings.append(
                    {
                        "tag": run["tag"],
                        "metric": metric_name,
                        "value": value,
                        "median": median,
                        "mad": mad,
                        "modified_z": None,
                        "zero_mad_nonmedian": True,
                    }
                )
            continue
        for run, value in zip(runs, values):
            modified_z = 0.6744897501960817 * (value - median) / mad
            if abs(modified_z) <= 3.5:
                continue
            findings.append(
                {
                    "tag": run["tag"],
                    "metric": metric_name,
                    "value": value,
                    "median": median,
                    "mad": mad,
                    "modified_z": modified_z,
                }
            )
    return findings


def resolved(path: str, base: Path) -> Path:
    candidate = Path(path)
    if not candidate.is_absolute():
        candidate = base / candidate
    return candidate.resolve()


def load_telemetry_artifact(
    path: Path | None,
    expected_columns: tuple[str, ...] | None = None,
) -> tuple[list[dict[str, str]], dict[str, Any] | None]:
    if path is None or not path.is_file():
        return [], None
    try:
        payload = path.read_bytes()
        text = payload.decode("utf-8")
        reader = csv.DictReader(io.StringIO(text, newline=""), strict=True)
        if expected_columns is not None:
            require(
                reader.fieldnames == list(expected_columns),
                f"telemetry {path}: header does not match the frozen schema",
            )
        rows = list(reader)
    except (OSError, UnicodeDecodeError, csv.Error) as exc:
        raise SummaryError(f"read telemetry {path}: {exc}") from exc
    if expected_columns is not None:
        require(rows, f"telemetry {path}: no samples")
        timestamps: list[int] = []
        for row_number, row in enumerate(rows, start=2):
            require(
                None not in row and all(value is not None for value in row.values()),
                f"telemetry {path}:{row_number}: malformed CSV row",
            )
            timestamps.append(
                integer(row.get("unix_s"), f"telemetry {path}:{row_number} unix_s")
            )
            for column in expected_columns[1:]:
                value = (
                    integer(
                        row.get(column),
                        f"telemetry {path}:{row_number} {column}",
                    )
                    if column in TELEMETRY_INTEGER_COLUMNS
                    else finite_number(
                        row.get(column), f"telemetry {path}:{row_number} {column}"
                    )
                )
                require(
                    value >= 0,
                    f"telemetry {path}:{row_number}: negative {column}",
                )
        require(
            all(left <= right for left, right in zip(timestamps, timestamps[1:])),
            f"telemetry {path}: timestamps are not monotonic",
        )
    return rows, payload_fingerprint(path, payload)


def load_telemetry(path: Path | None) -> list[dict[str, str]]:
    rows, _ = load_telemetry_artifact(path)
    return rows


def scorer_stability_findings(text: str) -> list[str]:
    """Mirror score.go's G5 stderr signatures for the local audit."""
    lower = text.lower()
    checks = (
        ("unexpected_recovery", "Unexpected error:" in text),
        ("rescue_handler_panic", "Rescue handler panic" in text),
        ("client_driver_panic", "client driver panic:" in text),
        ("evaluation_panic", "evaluation panic:" in text),
        (
            "http_handler_panic",
            "http: panic serving" in lower or "panic recovered" in lower,
        ),
        (
            "runtime_panic",
            any(line.strip().startswith("panic:") for line in text.splitlines()),
        ),
        ("fatal_runtime_error", "fatal error:" in text),
        (
            "out_of_memory",
            "runtime: out of memory" in lower
            or "out of memory: killed process" in lower,
        ),
        (
            "service_restart",
            "service restart" in lower or "restarting service" in lower,
        ),
        (
            "missing_service",
            "service unavailable" in lower or "service missing" in lower,
        ),
        (
            "unclean_drain",
            "unclean drain" in lower or "did not drain within" in lower,
        ),
    )
    return [code for code, present in checks if present]


def telemetry_window(
    rows: list[dict[str, str]], start_ms: int, end_ms: int
) -> list[dict[str, str]]:
    selected = []
    for row in rows:
        try:
            timestamp_ms = int(row["unix_s"]) * 1000
        except (KeyError, TypeError, ValueError) as exc:
            raise SummaryError("telemetry row has an invalid unix_s") from exc
        if start_ms <= timestamp_ms <= end_ms:
            selected.append(row)
    return selected


def numeric_column(rows: list[dict[str, str]], name: str) -> list[float]:
    return [finite_number(row.get(name), f"telemetry {name}") for row in rows]


def audit_run(
    marker_path: Path,
    workdir: Path,
    rss_rows: list[dict[str, str]],
    host_rows: list[dict[str, str]],
    service_rows: list[dict[str, str]] | None = None,
) -> dict[str, Any]:
    name = marker_path.name
    suffix = ".run.json.complete.json"
    require(name.endswith(suffix), f"unexpected marker name: {name}")
    tag = name[: -len(suffix)]
    meta_path = marker_path.with_name(f"{tag}.run.json")
    csv_path = marker_path.with_name(f"{tag}.csv")
    log_path = marker_path.with_name(f"{tag}.log")
    for path in (meta_path, csv_path, log_path):
        require(path.is_file(), f"{tag}: missing {path.name}")

    manifest, manifest_bytes = read_json_bytes(meta_path)
    marker, marker_bytes = read_json_bytes(marker_path)
    require(manifest.get("schema") == 2, f"{tag}: run schema is not 2")
    require(manifest.get("kind") == RUN_KIND, f"{tag}: wrong run kind")
    require(manifest.get("completion_state") == "complete", f"{tag}: incomplete run")
    require(manifest.get("score_schema") == 1, f"{tag}: score schema is not 1")
    require(
        manifest.get("scorer_version") == "sim-latency-score/1",
        f"{tag}: scorer version mismatch",
    )
    require(
        isinstance(manifest.get("evaluation_id"), str)
        and bool(manifest["evaluation_id"]),
        f"{tag}: evaluation id missing",
    )
    require(
        isinstance(manifest.get("config_sha256"), str)
        and re.fullmatch(r"[0-9a-f]{64}", manifest["config_sha256"]) is not None,
        f"{tag}: malformed config hash",
    )
    integer(manifest.get("seed"), f"{tag} seed")
    require(
        isinstance(manifest.get("build_revision"), str)
        and bool(manifest["build_revision"]),
        f"{tag}: build revision missing",
    )
    require(
        isinstance(manifest.get("build_modified"), bool),
        f"{tag}: build-modified identity is not boolean",
    )
    official = manifest.get("official", False)
    require(isinstance(official, bool), f"{tag}: official identity is not boolean")
    require(
        isinstance(manifest.get("hostname"), str)
        and isinstance(manifest.get("os"), str)
        and isinstance(manifest.get("arch"), str)
        and all((manifest["hostname"], manifest["os"], manifest["arch"])),
        f"{tag}: host identity missing",
    )
    require(integer(manifest.get("num_cpu"), f"{tag} CPU count") > 0, f"{tag}: invalid CPU count")
    require(
        isinstance(manifest.get("flags"), dict)
        and all(
            isinstance(key, str) and isinstance(value, str)
            for key, value in manifest["flags"].items()
        ),
        f"{tag}: malformed run flags",
    )
    require(marker.get("schema") == 1, f"{tag}: marker schema is not 1")
    require(marker.get("kind") == MARKER_KIND, f"{tag}: wrong marker kind")
    require(
        marker.get("evaluation_id") == manifest.get("evaluation_id"),
        f"{tag}: marker evaluation id mismatch",
    )
    require(
        marker.get("score_schema") == manifest.get("score_schema")
        and marker.get("scorer_version") == manifest.get("scorer_version"),
        f"{tag}: marker scorer identity mismatch",
    )
    require(
        integer(marker.get("run_manifest_bytes"), f"{tag} manifest bytes")
        == len(manifest_bytes),
        f"{tag}: marker manifest byte count mismatch",
    )
    require(
        marker.get("run_manifest_sha256")
        == hashlib.sha256(manifest_bytes).hexdigest(),
        f"{tag}: marker manifest hash mismatch",
    )
    require(
        marker.get("completed_unix_ms") == manifest.get("completed_unix_ms"),
        f"{tag}: marker completion time mismatch",
    )
    final_marker = resolved(str(manifest.get("final_marker_path", "")), workdir)
    require(final_marker == marker_path.resolve(), f"{tag}: final marker path mismatch")

    csv_size = csv_path.stat().st_size
    csv_sha = file_sha256(csv_path)
    require(
        integer(manifest.get("results_csv_bytes"), f"{tag} CSV bytes") == csv_size,
        f"{tag}: CSV byte count mismatch",
    )
    require(
        manifest.get("results_csv_sha256") == csv_sha,
        f"{tag}: CSV hash mismatch",
    )

    start_ms = integer(manifest.get("measure_start_ms"), f"{tag} start")
    end_ms = integer(manifest.get("measure_end_ms"), f"{tag} end")
    timeout_ms = integer(manifest.get("request_timeout_ms"), f"{tag} timeout")
    require(end_ms > start_ms and timeout_ms > 0, f"{tag}: invalid timing contract")
    total_rows = 0
    rows_in_window = 0
    failures = 0
    received_bytes = 0
    score_observations: list[float] = []
    ttfb_observations: list[float] = []
    total_observations: list[float] = []
    throughput_observations: list[float] = []
    throughput_precision_bound = 0.0
    try:
        with csv_path.open(newline="", encoding="utf-8") as handle:
            reader = csv.DictReader(handle, strict=True)
            require(
                reader.fieldnames == list(EXPECTED_CSV_COLUMNS),
                f"{tag}: CSV header does not match the frozen schema",
            )
            for row_number, row in enumerate(reader, start=2):
                total_rows += 1
                require(
                    None not in row and all(value is not None for value in row.values()),
                    f"{tag}:{row_number}: malformed CSV field count",
                )
                t_start_ms = integer(row.get("t_start_ms"), f"{tag}:{row_number} start")
                require(t_start_ms >= 0, f"{tag}:{row_number}: negative timestamp")
                require(
                    isinstance(row.get("client"), str) and bool(row["client"]),
                    f"{tag}:{row_number}: missing client identity",
                )
                require(
                    isinstance(row.get("path"), str) and bool(row["path"]),
                    f"{tag}:{row_number}: missing request path",
                )
                depth = integer(row.get("depth"), f"{tag}:{row_number} depth")
                require(depth >= 0, f"{tag}:{row_number}: negative depth")
                status = integer(row.get("status"), f"{tag}:{row_number} status")
                require(
                    0 <= status <= 599,
                    f"{tag}:{row_number}: status outside score bounds",
                )
                byte_count = integer(row.get("bytes"), f"{tag}:{row_number} bytes")
                require(byte_count >= 0, f"{tag}:{row_number}: negative bytes")
                ttfb_ms = finite_number(
                    row.get("ttfb_ms"), f"{tag}:{row_number} ttfb_ms"
                )
                total_ms = finite_number(
                    row.get("total_ms"), f"{tag}:{row_number} total_ms"
                )
                require(
                    re.fullmatch(r"(?:0|[1-9][0-9]*)\.[0-9]{3}", row["ttfb_ms"])
                    is not None
                    and re.fullmatch(
                        r"(?:0|[1-9][0-9]*)\.[0-9]{3}", row["total_ms"]
                    )
                    is not None,
                    f"{tag}:{row_number}: timing fields do not use frozen millisecond precision",
                )
                bytes_per_s = integer(
                    row.get("bytes_per_s"), f"{tag}:{row_number} bytes_per_s"
                )
                require(
                    ttfb_ms >= 0 and total_ms >= 0 and bytes_per_s >= 0,
                    f"{tag}:{row_number}: negative timing or throughput",
                )
                if total_ms > 0:
                    lower_total = max(
                        math.nextafter(0.0, 1.0), total_ms - 0.0005
                    )
                    upper_total = total_ms + 0.0005
                    lower_rate = byte_count / (upper_total / 1000)
                    upper_rate = byte_count / (lower_total / 1000)
                    require(
                        lower_rate - 1 <= bytes_per_s <= upper_rate + 1,
                        f"{tag}:{row_number}: bytes_per_s is inconsistent with bytes and total_ms",
                    )
                if not start_ms <= t_start_ms < end_ms:
                    continue
                rows_in_window += 1
                received_bytes += byte_count
                if status == 200:
                    require(
                        0 < total_ms <= timeout_ms,
                        f"{tag}:{row_number}: successful total_ms outside score bounds",
                    )
                    require(
                        0 <= ttfb_ms <= total_ms,
                        f"{tag}:{row_number}: successful ttfb_ms outside timing bounds",
                    )
                    score_observations.append(total_ms)
                    ttfb_observations.append(ttfb_ms)
                    total_observations.append(total_ms)
                    if byte_count >= 1024 * 1024:
                        throughput = byte_count / (total_ms / 1000)
                        throughput_observations.append(throughput)
                        lower_total = max(math.nextafter(0.0, 1.0), total_ms - 0.0005)
                        upper_total = total_ms + 0.0005
                        throughput_precision_bound = max(
                            throughput_precision_bound,
                            abs(byte_count / (lower_total / 1000) - throughput),
                            abs(byte_count / (upper_total / 1000) - throughput),
                        )
                else:
                    failures += 1
                    score_observations.append(float(timeout_ms))
    except (OSError, csv.Error) as exc:
        raise SummaryError(f"read {csv_path}: {exc}") from exc

    require(
        total_rows == integer(manifest.get("rows"), f"{tag} total rows"),
        f"{tag}: total row mismatch",
    )
    require(
        rows_in_window
        == integer(manifest.get("rows_in_window"), f"{tag} measured rows"),
        f"{tag}: measured row mismatch",
    )
    require(
        failures == integer(manifest.get("failures"), f"{tag} failures"),
        f"{tag}: failure mismatch",
    )
    require(rows_in_window > 0, f"{tag}: empty measured window")
    success_rate = (rows_in_window - failures) / rows_in_window
    raw_score = type7_quantile(score_observations, 0.95)
    window_seconds = (end_ms - start_ms) / 1000

    canonical_metrics: dict[str, tuple[float, int]] = {
        "fail_rate": (failures / rows_in_window, rows_in_window),
        "ttfb_p50_ms": (type7_quantile(ttfb_observations, 0.50), len(ttfb_observations)),
        "ttfb_p95_ms": (type7_quantile(ttfb_observations, 0.95), len(ttfb_observations)),
        "total_p50_ms": (type7_quantile(total_observations, 0.50), len(total_observations)),
        "total_p95_ms": (type7_quantile(total_observations, 0.95), len(total_observations)),
        "goodput_bytes_per_s": (received_bytes / window_seconds, rows_in_window),
    }
    if throughput_observations:
        canonical_metrics.update(
            {
                "throughput_p05_bytes_per_s": (
                    type7_quantile(throughput_observations, 0.05),
                    len(throughput_observations),
                ),
                "throughput_p50_bytes_per_s": (
                    type7_quantile(throughput_observations, 0.50),
                    len(throughput_observations),
                ),
                "throughput_p95_bytes_per_s": (
                    type7_quantile(throughput_observations, 0.95),
                    len(throughput_observations),
                ),
            }
        )

    metrics: dict[str, float] = {
        "apex_raw_score_ms": raw_score,
        "success_rate": success_rate,
        "request_count": float(rows_in_window),
        "received_bytes": float(received_bytes),
        **{name: value for name, (value, _) in canonical_metrics.items()},
    }
    manifest_metrics = manifest.get("metrics")
    require(isinstance(manifest_metrics, dict), f"{tag}: metrics missing")
    require(
        set(manifest_metrics) == set(canonical_metrics),
        f"{tag}: sidecar metric set does not match CSV recomputation",
    )
    for metric_name, summary in manifest_metrics.items():
        require(isinstance(summary, dict), f"{tag}: malformed metric {metric_name}")
        stored_value = finite_number(
            summary.get("value"), f"{tag} metric {metric_name} value"
        )
        stored_n = integer(summary.get("n"), f"{tag} metric {metric_name} n")
        expected_value, expected_n = canonical_metrics[metric_name]
        if metric_name.endswith("_ms"):
            absolute_tolerance = 0.001
        elif metric_name.startswith("throughput_"):
            absolute_tolerance = max(0.1, throughput_precision_bound + 1e-6)
        else:
            absolute_tolerance = 1e-8
        require(
            math.isclose(
                stored_value,
                expected_value,
                rel_tol=1e-8,
                abs_tol=absolute_tolerance,
            ),
            f"{tag}: sidecar metric {metric_name} differs from CSV recomputation",
        )
        require(
            stored_n == expected_n,
            f"{tag}: sidecar metric {metric_name} sample count mismatch",
        )
        block_se = finite_number(
            summary.get("block_se", 0), f"{tag} metric {metric_name} block_se"
        )
        require(block_se >= 0, f"{tag}: negative block SE for {metric_name}")

    block_ms = 60_000
    if end_ms - start_ms < 8 * block_ms:
        block_ms = max(1000, (end_ms - start_ms) // 8)
    block_count = (end_ms - start_ms + block_ms - 1) // block_ms
    require(
        manifest.get("block_count") == block_count
        and math.isclose(
            finite_number(manifest.get("block_seconds"), f"{tag} block seconds"),
            block_ms / 1000,
        ),
        f"{tag}: block-bootstrap layout mismatch",
    )

    try:
        log_bytes = log_path.read_bytes()
    except OSError as exc:
        raise SummaryError(f"read {log_path}: {exc}") from exc
    log_text = log_bytes.decode("utf-8", errors="replace")
    tagged_ids = set(re.findall(r"\[sim-latency eval=([^\]]+)\]", log_text))
    require(tagged_ids == {manifest.get("evaluation_id")}, f"{tag}: mixed log identity")
    log_findings = {
        "scorer_stability_findings": scorer_stability_findings(log_text),
        "panic_lines": len(re.findall(r"(?im)^.*\bpanic\b.*$", log_text)),
        "fatal_lines": len(re.findall(r"(?im)^(?:F\d{4}|.*\bfatal\b).*$", log_text)),
        "enobufs_lines": len(
            re.findall(r"(?im)^.*(?:ENOBUFS|no buffer space).*$", log_text)
        ),
        "contract_close_timeouts": len(
            re.findall(r"could not close .* after client close = Timeout", log_text)
        ),
    }
    require(log_findings["panic_lines"] == 0, f"{tag}: panic found in log")
    require(log_findings["fatal_lines"] == 0, f"{tag}: fatal found in log")
    require(log_findings["enobufs_lines"] == 0, f"{tag}: ENOBUFS found in log")
    require(
        not log_findings["scorer_stability_findings"],
        f"{tag}: scorer stability finding: {log_findings['scorer_stability_findings']}",
    )

    clients_pool = integer(manifest.get("clients_pool"), f"{tag} clients pool")
    clients_established = integer(
        manifest.get("clients_established"), f"{tag} clients established"
    )
    require(
        clients_pool > 0 and clients_established == clients_pool,
        f"{tag}: warm client pool incomplete",
    )

    stats_instance_id = manifest.get("stats_instance_id")
    require(
        isinstance(stats_instance_id, str) and bool(stats_instance_id),
        f"{tag}: stats instance identity missing",
    )
    stats_root = Path(str(manifest.get("stats_root", "")))
    require(stats_root.is_absolute(), f"{tag}: stats root is not absolute")
    require(
        stats_root.name.endswith("-" + stats_instance_id),
        f"{tag}: stats root does not bind its instance id",
    )
    stream_dir = stats_root / "findproviders2"
    require(stream_dir.is_dir(), f"{tag}: FindProviders2 stream missing")
    finalized_segments = sorted(stream_dir.glob("*.pb.zst"))
    partial_segments = sorted(stream_dir.glob("*.partial"))
    require(finalized_segments, f"{tag}: no finalized FindProviders2 segments")
    require(not partial_segments, f"{tag}: partial FindProviders2 segment remains")
    segment_manifest = []
    corpus_digest = hashlib.sha256()
    for segment in finalized_segments:
        segment_sha = file_sha256(segment)
        segment_size = segment.stat().st_size
        segment_manifest.append(
            {"name": segment.name, "bytes": segment_size, "sha256": segment_sha}
        )
        corpus_digest.update(segment.name.encode("utf-8"))
        corpus_digest.update(b"\0")
        corpus_digest.update(str(segment_size).encode("ascii"))
        corpus_digest.update(b"\0")
        corpus_digest.update(segment_sha.encode("ascii"))
        corpus_digest.update(b"\n")

    resource: dict[str, Any] = {}
    selected_rss = telemetry_window(rss_rows, start_ms, end_ms)
    if selected_rss:
        rss_kb = numeric_column(selected_rss, "sim_rss_kb")
        resource.update(
            {
                "rss_samples": len(rss_kb),
                "rss_mean_bytes": statistics.fmean(rss_kb) * 1024,
                "rss_peak_bytes": max(rss_kb) * 1024,
            }
        )
        rss_fields = (
            ("docker_rss_kb", "docker_rss", 1024),
            ("pg_mem_mb", "postgres_memory", 1024 * 1024),
            ("redis_mem_mb", "redis_memory", 1024 * 1024),
        )
        for column, label, multiplier in rss_fields:
            if column not in selected_rss[0]:
                continue
            values = numeric_column(selected_rss, column)
            # The legacy sampler queries fixed Docker container names. On a
            # native-service host its zero is "not observed", not evidence of
            # zero database/cache memory.
            if label in {"postgres_memory", "redis_memory"} and max(values) == 0:
                continue
            resource[f"{label}_mean_bytes"] = statistics.fmean(values) * multiplier
            resource[f"{label}_peak_bytes"] = max(values) * multiplier
    selected_host = telemetry_window(host_rows, start_ms, end_ms)
    if selected_host:
        processes = numeric_column(selected_host, "sim_processes")
        cpu = numeric_column(selected_host, "sim_cpu_pct")
        tcp = numeric_column(selected_host, "tcp_established")
        available = numeric_column(selected_host, "mem_available_kb")
        swap = numeric_column(selected_host, "swap_used_kb")
        first_ms = int(selected_host[0]["unix_s"]) * 1000
        last_ms = int(selected_host[-1]["unix_s"]) * 1000
        coverage = min(1.0, max(0.0, (last_ms - first_ms + 30_000) / (end_ms - start_ms)))
        resource.update(
            {
                "host_samples": len(selected_host),
                "host_coverage_fraction": coverage,
                "sim_processes_min": int(min(processes)),
                "sim_processes_max": int(max(processes)),
                "sim_cpu_mean_logical_cores": statistics.fmean(cpu) / 100,
                "sim_cpu_peak_logical_cores": max(cpu) / 100,
                "tcp_established_peak": int(max(tcp)),
                "mem_available_min_bytes": min(available) * 1024,
                "swap_used_peak_bytes": max(swap) * 1024,
            }
        )
    selected_services = telemetry_window(service_rows or [], start_ms, end_ms)
    if selected_services:
        postgres_processes = numeric_column(
            selected_services, "postgres_processes"
        )
        postgres_rss = numeric_column(
            selected_services, "postgres_summed_rss_kb"
        )
        redis_processes = numeric_column(selected_services, "redis_processes")
        redis_rss = numeric_column(selected_services, "redis_summed_rss_kb")
        first_ms = int(selected_services[0]["unix_s"]) * 1000
        last_ms = int(selected_services[-1]["unix_s"]) * 1000
        coverage = min(
            1.0,
            max(0.0, (last_ms - first_ms + 30_000) / (end_ms - start_ms)),
        )
        resource.update(
            {
                "service_samples": len(selected_services),
                "service_coverage_fraction": coverage,
                "postgres_processes_min": int(min(postgres_processes)),
                "postgres_processes_max": int(max(postgres_processes)),
                "postgres_summed_rss_mean_bytes": statistics.fmean(postgres_rss)
                * 1024,
                "postgres_summed_rss_peak_bytes": max(postgres_rss) * 1024,
                "redis_processes_min": int(min(redis_processes)),
                "redis_processes_max": int(max(redis_processes)),
                "redis_summed_rss_mean_bytes": statistics.fmean(redis_rss) * 1024,
                "redis_summed_rss_peak_bytes": max(redis_rss) * 1024,
            }
        )

    return {
        "tag": tag,
        "csv": str(csv_path),
        "manifest": str(meta_path),
        "marker": str(marker_path),
        "log": str(log_path),
        "evaluation_id": manifest.get("evaluation_id"),
        "completed_unix_ms": manifest.get("completed_unix_ms"),
        "measure_start_ms": start_ms,
        "measure_end_ms": end_ms,
        "clients_pool": clients_pool,
        "clients_established": clients_established,
        "csv_sha256": csv_sha,
        "csv_bytes": csv_size,
        "manifest_sha256": hashlib.sha256(manifest_bytes).hexdigest(),
        "manifest_bytes": len(manifest_bytes),
        "marker_sha256": hashlib.sha256(marker_bytes).hexdigest(),
        "marker_bytes": len(marker_bytes),
        "log_sha256": hashlib.sha256(log_bytes).hexdigest(),
        "log_bytes": len(log_bytes),
        "metrics": metrics,
        "sample_segments": len(finalized_segments),
        "sample_segment_bytes": sum(path.stat().st_size for path in finalized_segments),
        "sample_segment_manifest": segment_manifest,
        "sample_corpus_sha256": corpus_digest.hexdigest(),
        "stats_root": str(stats_root),
        "stats_instance_id": stats_instance_id,
        "log_findings": log_findings,
        "resources": resource,
        "identity": {
            "schema": manifest.get("schema"),
            "score_schema": manifest.get("score_schema"),
            "scorer_version": manifest.get("scorer_version"),
            "config_sha256": manifest.get("config_sha256"),
            "seed": manifest.get("seed"),
            "build_revision": manifest.get("build_revision"),
            "build_modified": manifest.get("build_modified"),
            "official": official,
            "hostname": manifest.get("hostname"),
            "os": manifest.get("os"),
            "arch": manifest.get("arch"),
            "num_cpu": manifest.get("num_cpu"),
            "request_timeout_ms": timeout_ms,
            "duration_ms": end_ms - start_ms,
            "flags": manifest.get("flags"),
        },
    }


def same_identity(runs: list[dict[str, Any]]) -> dict[str, Any]:
    identity = runs[0]["identity"]
    for run in runs[1:]:
        require(run["identity"] == identity, f"{run['tag']}: environment identity differs")
    evaluation_ids = [run["evaluation_id"] for run in runs]
    require(len(evaluation_ids) == len(set(evaluation_ids)), "duplicate evaluation id")
    stats_roots = [run["stats_root"] for run in runs]
    require(len(stats_roots) == len(set(stats_roots)), "duplicate stats root")
    stats_instance_ids = [run["stats_instance_id"] for run in runs]
    require(
        len(stats_instance_ids) == len(set(stats_instance_ids)),
        "duplicate stats instance id",
    )
    return identity


def audit_campaign_attempts(
    runs_dir: Path, completed_runs: list[dict[str, Any]]
) -> dict[str, Any]:
    """Inventory every numbered campaign attempt, including failed-closed runs."""
    attempt_paths: dict[str, dict[str, Any]] = {}
    pattern = re.compile(
        r"^(r[0-9]+)\.(csv|log|run\.json|run\.json\.complete\.json)$"
    )
    for directory, location in ((runs_dir, "runs"), (runs_dir / "flagged", "flagged")):
        if not directory.exists():
            continue
        try:
            paths = list(directory.iterdir())
        except OSError as exc:
            raise SummaryError(f"read campaign directory {directory}: {exc}") from exc
        for path in paths:
            match = pattern.fullmatch(path.name)
            if match is None or not path.is_file():
                continue
            tag = match.group(1)
            if tag in attempt_paths:
                require(
                    attempt_paths[tag]["location"] == location,
                    f"campaign tag {tag} exists in both runs and flagged",
                )
            attempt_paths.setdefault(tag, {"location": location})[
                match.group(2)
            ] = path

    completed_tags = {run["tag"] for run in completed_runs}
    require(
        completed_tags.issubset(attempt_paths),
        "an authenticated run is missing from the campaign-attempt inventory",
    )

    def tag_number(tag: str) -> int:
        return int(tag[1:])

    attempt_numbers = sorted(tag_number(tag) for tag in attempt_paths)
    require(attempt_numbers, "campaign-attempt inventory is empty")
    require(
        len(attempt_numbers) == len(set(attempt_numbers)),
        "campaign contains two spellings of the same numeric attempt tag",
    )
    require(
        attempt_numbers
        == list(range(attempt_numbers[0], attempt_numbers[-1] + 1)),
        "campaign attempt tags are not contiguous",
    )

    completed_numbers = sorted(tag_number(tag) for tag in completed_tags)
    longest_streak = 0
    current_streak = 0
    previous: int | None = None
    for number in completed_numbers:
        current_streak = current_streak + 1 if previous is not None and number == previous + 1 else 1
        longest_streak = max(longest_streak, current_streak)
        previous = number

    excluded: list[dict[str, Any]] = []
    for tag in sorted(attempt_paths, key=tag_number):
        if tag in completed_tags:
            continue
        artifacts = attempt_paths[tag]
        location = str(artifacts["location"])
        manifest_path = artifacts.get("run.json")
        manifest: dict[str, Any] | None = None
        manifest_error: str | None = None
        if manifest_path is not None:
            try:
                manifest = read_json(manifest_path)
            except SummaryError as exc:
                manifest_error = str(exc)

        if location == "flagged" and manifest_error is None and manifest is not None:
            if manifest.get("completion_state") == "incomplete":
                reason = str(manifest.get("incomplete_code") or "quarantined_incomplete")
            else:
                reason = "quarantined_completed_run"
        elif manifest_error is not None:
            reason = "malformed_run_manifest"
        elif manifest is None:
            reason = "missing_run_manifest"
        elif manifest.get("completion_state") == "incomplete":
            reason = str(manifest.get("incomplete_code") or "incomplete_evaluation")
        elif manifest.get("completion_state") == "complete":
            reason = "missing_authenticated_completion_marker"
        else:
            reason = "invalid_completion_state"

        require(
            reason != "quarantined_completed_run",
            f"{tag}: an authenticated completed run cannot be excluded by "
            "moving it to flagged/; this local workflow has no signed, "
            "predeclared infrastructure-exclusion record",
        )

        csv_path = artifacts.get("csv")
        log_path = artifacts.get("log")
        marker_path = artifacts.get("run.json.complete.json")
        actual_csv_bytes = csv_path.stat().st_size if csv_path else None
        actual_csv_sha256 = file_sha256(csv_path) if csv_path else None
        recorded_csv_bytes = (
            manifest.get("results_csv_bytes") if manifest is not None else None
        )
        recorded_csv_sha256 = (
            manifest.get("results_csv_sha256") if manifest is not None else None
        )
        csv_identity_matches_manifest = (
            actual_csv_bytes == recorded_csv_bytes
            and actual_csv_sha256 == recorded_csv_sha256
            if csv_path is not None and manifest is not None
            else None
        )
        excluded.append(
            {
                "tag": tag,
                "location": location,
                "reason": reason,
                "completion_state": manifest.get("completion_state") if manifest else None,
                "incomplete_message": manifest.get("incomplete_message") if manifest else None,
                "evaluation_id": manifest.get("evaluation_id") if manifest else None,
                "stats_root": manifest.get("stats_root") if manifest else None,
                "stats_instance_id": manifest.get("stats_instance_id") if manifest else None,
                "completed_unix_ms": manifest.get("completed_unix_ms") if manifest else None,
                "clients_pool": manifest.get("clients_pool") if manifest else None,
                "clients_established": manifest.get("clients_established") if manifest else None,
                "rows_in_window": manifest.get("rows_in_window") if manifest else None,
                "recorded_results_csv_bytes": recorded_csv_bytes,
                "recorded_results_csv_sha256": recorded_csv_sha256,
                "csv_identity_matches_manifest": csv_identity_matches_manifest,
                "manifest_error": manifest_error,
                "artifacts": {
                    "csv": str(csv_path) if csv_path else None,
                    "csv_bytes": actual_csv_bytes,
                    "csv_sha256": actual_csv_sha256,
                    "manifest": str(manifest_path) if manifest_path else None,
                    "manifest_bytes": manifest_path.stat().st_size if manifest_path else None,
                    "manifest_sha256": file_sha256(manifest_path) if manifest_path else None,
                    "marker": str(marker_path) if marker_path else None,
                    "marker_bytes": marker_path.stat().st_size if marker_path else None,
                    "marker_sha256": file_sha256(marker_path) if marker_path else None,
                    "log": str(log_path) if log_path else None,
                    "log_bytes": log_path.stat().st_size if log_path else None,
                    "log_sha256": file_sha256(log_path) if log_path else None,
                },
            }
        )

    evaluation_ids = [run["evaluation_id"] for run in completed_runs]
    evaluation_ids.extend(
        attempt["evaluation_id"]
        for attempt in excluded
        if isinstance(attempt.get("evaluation_id"), str)
        and attempt["evaluation_id"]
    )
    require(
        len(evaluation_ids) == len(set(evaluation_ids)),
        "campaign attempts reuse an evaluation id",
    )
    stats_roots = [run["stats_root"] for run in completed_runs]
    stats_roots.extend(
        attempt["stats_root"]
        for attempt in excluded
        if isinstance(attempt.get("stats_root"), str) and attempt["stats_root"]
    )
    require(len(stats_roots) == len(set(stats_roots)), "campaign attempts reuse a stats root")
    stats_instance_ids = [run["stats_instance_id"] for run in completed_runs]
    stats_instance_ids.extend(
        attempt["stats_instance_id"]
        for attempt in excluded
        if isinstance(attempt.get("stats_instance_id"), str)
        and attempt["stats_instance_id"]
    )
    require(
        len(stats_instance_ids) == len(set(stats_instance_ids)),
        "campaign attempts reuse a stats instance id",
    )
    excluded_reason_counts: dict[str, int] = {}
    for attempt in excluded:
        reason = str(attempt["reason"])
        excluded_reason_counts[reason] = excluded_reason_counts.get(reason, 0) + 1

    return {
        "attempt_count": len(attempt_paths),
        "first_attempt_tag": min(attempt_paths, key=tag_number),
        "last_attempt_tag": max(attempt_paths, key=tag_number),
        "attempt_tags_contiguous": True,
        "authenticated_count": len(completed_runs),
        "excluded_count": len(excluded),
        "excluded_reason_counts": dict(sorted(excluded_reason_counts.items())),
        "excluded_csv_identity_mismatch_count": sum(
            attempt["csv_identity_matches_manifest"] is False
            for attempt in excluded
        ),
        "longest_consecutive_authenticated_tag_streak": longest_streak,
        "has_20_consecutive_authenticated_tags": longest_streak >= 20,
        "excluded": excluded,
    }


def attach_sample_audits(helper: Path, runs: list[dict[str, Any]]) -> str:
    require(helper.is_file(), f"sample-audit helper missing: {helper}")
    command = [str(helper.resolve()), *[run["manifest"] for run in runs]]
    try:
        completed = subprocess.run(
            command,
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
    except OSError as exc:
        raise SummaryError(f"run sample-audit helper: {exc}") from exc
    require(
        completed.returncode == 0,
        "sample-audit helper failed: " + completed.stderr.strip(),
    )
    try:
        value = json.loads(
            completed.stdout,
            object_pairs_hook=strict_object,
            parse_constant=reject_json_constant,
        )
    except (TypeError, ValueError) as exc:
        raise SummaryError(f"sample-audit helper returned malformed JSON: {exc}") from exc
    require(isinstance(value, dict), "sample-audit result is not an object")
    require(value.get("schema") == 1, "sample-audit schema is not 1")
    require(
        value.get("kind") == "sim-latency-baseline-samples",
        "sample-audit kind mismatch",
    )
    audits = value.get("runs")
    require(isinstance(audits, dict), "sample-audit result has no runs")
    require(set(audits) == {run["tag"] for run in runs}, "sample-audit run set mismatch")
    for run in runs:
        audit = audits[run["tag"]]
        require(isinstance(audit, dict), f"{run['tag']}: malformed sample audit")
        require(
            audit.get("evaluation_id") == run["evaluation_id"],
            f"{run['tag']}: sample evaluation id mismatch",
        )
        require(
            Path(str(audit.get("stats_root", ""))).resolve()
            == Path(str(run["stats_root"])).resolve(),
            f"{run['tag']}: sample root mismatch",
        )
        require(
            integer(audit.get("samples"), f"{run['tag']} samples") > 0,
            f"{run['tag']}: no samples",
        )
        sample_count = integer(audit.get("samples"), f"{run['tag']} samples")
        first_sample_ms = integer(
            audit.get("first_sample_ms"), f"{run['tag']} first sample"
        )
        last_sample_ms = integer(
            audit.get("last_sample_ms"), f"{run['tag']} last sample"
        )
        require(
            run["measure_start_ms"] <= first_sample_ms <= last_sample_ms
            and last_sample_ms < run["measure_end_ms"],
            f"{run['tag']}: sample time span is outside the measured window",
        )
        sample_span = finite_number(
            audit.get("sample_span_fraction"), f"{run['tag']} sample span"
        )
        expected_span = (last_sample_ms - first_sample_ms) / (
            run["measure_end_ms"] - run["measure_start_ms"]
        )
        require(
            0 <= sample_span <= 1
            and math.isclose(sample_span, expected_span, rel_tol=1e-12, abs_tol=1e-12),
            f"{run['tag']}: sample span fraction mismatch",
        )
        require(
            integer(audit.get("empty_pools"), f"{run['tag']} empty pools") >= 0,
            f"{run['tag']}: invalid empty-pool count",
        )
        finite_number(audit.get("load_p95_ms"), f"{run['tag']} sample load p95")
        require(
            isinstance(audit.get("report"), dict),
            f"{run['tag']}: sample report missing",
        )
        require(
            integer(audit["report"].get("samples"), f"{run['tag']} report samples")
            == sample_count,
            f"{run['tag']}: sample report count mismatch",
        )
        run["sample_audit"] = audit
    return file_sha256(helper)


def load_heldout_comparison(path: Path) -> dict[str, Any] | None:
    if not path.is_file():
        return None
    result, payload = read_json_bytes(path)
    required_keys = {"alpha", "runs_a", "runs_b", "verdict", "reason", "metrics"}
    require(
        required_keys.issubset(result)
        and set(result).issubset(required_keys | {"warnings"}),
        "held-out A/A comparison has an unexpected schema",
    )
    alpha = finite_number(result.get("alpha"), "held-out A/A alpha")
    require(0 < alpha <= 0.5, "held-out A/A alpha is outside (0, 0.5]")
    for side in ("runs_a", "runs_b"):
        labels = result.get(side)
        require(
            isinstance(labels, list)
            and bool(labels)
            and all(isinstance(label, str) and bool(label) for label in labels),
            f"held-out A/A {side} is malformed",
        )
    require(
        result.get("verdict")
        in {"a_better", "b_better", "mixed", "indistinguishable"},
        "held-out A/A verdict is invalid",
    )
    require(
        isinstance(result.get("reason"), str) and bool(result["reason"]),
        "held-out A/A reason is missing",
    )
    require(
        isinstance(result.get("metrics"), list) and bool(result["metrics"]),
        "held-out A/A metrics are missing",
    )
    require(
        all(isinstance(metric, dict) for metric in result["metrics"]),
        "held-out A/A metric entry is malformed",
    )
    metric_names = [metric.get("name") for metric in result["metrics"]]
    expected_metric_names = set(METRIC_ORDER) - {
        "apex_raw_score_ms",
        "success_rate",
        "request_count",
        "received_bytes",
    }
    require(
        len(metric_names) == len(expected_metric_names)
        and set(metric_names) == expected_metric_names,
        "held-out A/A comparison metric set is incomplete or duplicated",
    )
    warnings = result.get("warnings", [])
    require(
        isinstance(warnings, list)
        and all(isinstance(warning, str) for warning in warnings),
        "held-out A/A warnings are malformed",
    )
    return {
        "artifact": payload_fingerprint(path, payload),
        "result": result,
    }


def attach_heldout_run_audits(
    heldout: dict[str, Any],
    workdir: Path,
    baseline_identity: dict[str, Any],
    baseline_runs: list[dict[str, Any]],
    host_rows: list[dict[str, str]],
    host_artifact: dict[str, Any] | None,
    service_rows: list[dict[str, str]],
    service_artifact: dict[str, Any] | None,
) -> None:
    result = heldout["result"]
    require(
        len(result["runs_a"]) == 1 and len(result["runs_b"]) == 1,
        "held-out A/A workflow requires exactly one run per side",
    )
    require(host_artifact is not None, "held-out host telemetry is missing")
    require(
        service_artifact is not None,
        "held-out native-service telemetry is missing",
    )
    audited: dict[str, list[dict[str, Any]]] = {"a": [], "b": []}
    for side, key in (("a", "runs_a"), ("b", "runs_b")):
        for csv_label in result[key]:
            csv_path = resolved(csv_label, workdir)
            require(csv_path.suffix == ".csv", f"held-out label is not a CSV: {csv_label}")
            marker_path = csv_path.with_name(
                csv_path.stem + ".run.json.complete.json"
            )
            audit = audit_run(marker_path, workdir, [], host_rows, service_rows)
            require(
                Path(audit["csv"]).resolve() == csv_path,
                f"held-out artifact path mismatch: {csv_label}",
            )
            require(
                audit["identity"] == baseline_identity,
                f"held-out run {audit['tag']} does not match the baseline identity",
            )
            require(
                audit["resources"].get("host_samples", 0) > 0,
                f"held-out run {audit['tag']} has no host telemetry",
            )
            require(
                audit["resources"].get("host_coverage_fraction", 0) >= 0.90,
                f"held-out run {audit['tag']} has less than 90% host coverage",
            )
            require(
                audit["resources"].get("sim_processes_min") == 5
                and audit["resources"].get("sim_processes_max") == 5,
                f"held-out run {audit['tag']} lost or overlapped a simulator process",
            )
            require(
                audit["resources"].get("service_coverage_fraction", 0) >= 0.90,
                f"held-out run {audit['tag']} has less than 90% native-service coverage",
            )
            require(
                audit["resources"].get("postgres_processes_min", 0) > 0
                and audit["resources"].get("redis_processes_min", 0) > 0,
                f"held-out run {audit['tag']} lost PostgreSQL or Redis",
            )
            audited[side].append(audit)
    heldout_ids = [run["evaluation_id"] for runs in audited.values() for run in runs]
    require(len(heldout_ids) == len(set(heldout_ids)), "held-out evaluation id reused")
    require(
        not set(heldout_ids).intersection(
            run["evaluation_id"] for run in baseline_runs
        ),
        "held-out evaluation was included in the baseline",
    )
    heldout_stats_roots = {
        run["stats_root"] for runs in audited.values() for run in runs
    }
    heldout_stats_instance_ids = {
        run["stats_instance_id"] for runs in audited.values() for run in runs
    }
    require(
        len(heldout_stats_roots) == len(heldout_ids)
        and not heldout_stats_roots.intersection(
            run["stats_root"] for run in baseline_runs
        ),
        "held-out stats root was reused",
    )
    require(
        len(heldout_stats_instance_ids) == len(heldout_ids)
        and not heldout_stats_instance_ids.intersection(
            run["stats_instance_id"] for run in baseline_runs
        ),
        "held-out stats instance id was reused",
    )
    a_raw = audited["a"][0]["metrics"]["apex_raw_score_ms"]
    b_raw = audited["b"][0]["metrics"]["apex_raw_score_ms"]
    heldout["host_telemetry_artifact"] = host_artifact
    heldout["service_telemetry_artifact"] = service_artifact
    heldout["audited_runs"] = audited
    heldout["apex_raw_score_delta_b_minus_a_ms"] = b_raw - a_raw
    heldout["apex_raw_score_relative_delta_b_vs_a"] = (b_raw - a_raw) / a_raw


def cross_check_baseline(
    baseline_path: Path, runs: list[dict[str, Any]], aggregate: dict[str, Any]
) -> dict[str, Any]:
    baseline = read_json(baseline_path)
    require(
        set(baseline)
        == {
            "schema",
            "kind",
            "alpha",
            "replicates",
            "config_sha256",
            "duration_s",
            "hostname",
            "runs",
            "metrics",
        },
        "baseline artifact has an unexpected schema",
    )
    require(baseline.get("schema") == 1, "baseline schema is not 1")
    require(baseline.get("kind") == BASELINE_KIND, "baseline kind mismatch")
    alpha = finite_number(baseline.get("alpha"), "baseline alpha")
    require(0 < alpha <= 0.5, "baseline alpha is outside (0, 0.5]")
    require(baseline.get("replicates") == len(runs), "baseline replicate count mismatch")
    require(
        baseline.get("config_sha256") == runs[0]["identity"]["config_sha256"],
        "baseline config hash mismatch",
    )
    require(
        math.isclose(
            finite_number(baseline.get("duration_s"), "baseline duration"),
            runs[0]["identity"]["duration_ms"] / 1000,
        ),
        "baseline duration mismatch",
    )
    require(baseline.get("hostname") == runs[0]["identity"]["hostname"], "baseline host mismatch")
    baseline_tags = [Path(str(path)).stem for path in baseline.get("runs", [])]
    require(baseline_tags == [run["tag"] for run in runs], "baseline run order mismatch")
    baseline_metrics = baseline.get("metrics")
    require(isinstance(baseline_metrics, dict), "baseline metrics missing")
    expected_metric_names = set(METRIC_ORDER) - {
        "apex_raw_score_ms",
        "success_rate",
        "request_count",
        "received_bytes",
    }
    require(
        set(baseline_metrics) == expected_metric_names,
        "baseline metric set does not match the frozen diagnostic set",
    )
    for metric_name, stored in baseline_metrics.items():
        require(metric_name in aggregate, f"baseline metric {metric_name} not recomputed")
        require(isinstance(stored, dict), f"malformed baseline metric {metric_name}")
        require(
            set(stored)
            == {
                "mean",
                "sd",
                "cv",
                "sd_rel_error",
                "min_detectable_delta",
                "sd_by_replicates",
                "min_detectable_delta_by_runs_per_side",
            },
            f"baseline metric {metric_name} has an unexpected schema",
        )
        for field in ("mean", "sd", "cv"):
            require(
                math.isclose(
                    finite_number(stored.get(field, 0), f"baseline {metric_name}.{field}"),
                    finite_number(aggregate[metric_name][field], f"aggregate {metric_name}.{field}"),
                    rel_tol=1e-10,
                    abs_tol=1e-8,
                ),
                f"baseline metric mismatch: {metric_name}.{field}",
            )

        values = [
            finite_number(run["metrics"][metric_name], f"{run['tag']} {metric_name}")
            for run in runs
            if metric_name in run["metrics"]
        ]
        require(len(values) >= 3, f"baseline metric {metric_name} has fewer than 3 values")
        expected_sd_relative_error = 1 / math.sqrt(2 * (len(values) - 1))
        require(
            math.isclose(
                finite_number(
                    stored.get("sd_rel_error", 0),
                    f"baseline {metric_name} SD relative error",
                ),
                expected_sd_relative_error,
                rel_tol=1e-10,
                abs_tol=1e-12,
            ),
            f"baseline metric mismatch: {metric_name}.sd_rel_error",
        )
        expected_sd_by_k = [statistics.stdev(values[:k]) for k in range(2, len(values) + 1)]
        stored_sd_by_k = stored.get("sd_by_replicates")
        require(
            isinstance(stored_sd_by_k, list)
            and len(stored_sd_by_k) == len(expected_sd_by_k),
            f"baseline metric {metric_name} has malformed sd_by_replicates",
        )
        for k, (actual, expected) in enumerate(
            zip(stored_sd_by_k, expected_sd_by_k), start=2
        ):
            require(
                math.isclose(
                    finite_number(actual, f"baseline {metric_name} sd at k={k}"),
                    expected,
                    rel_tol=1e-10,
                    abs_tol=1e-8,
                ),
                f"baseline metric mismatch: {metric_name}.sd_by_replicates k={k}",
            )

        stored_mde = stored.get("min_detectable_delta_by_runs_per_side")
        require(
            isinstance(stored_mde, list) and len(stored_mde) == len(values),
            f"baseline metric {metric_name} has malformed detection-limit series",
        )
        first_mde = finite_number(stored_mde[0], f"baseline {metric_name} MDE m=1")
        require(first_mde >= 0, f"baseline metric {metric_name} has negative MDE")
        expected_first_mde = (
            student_t_critical(alpha, len(values) - 1)
            * statistics.stdev(values)
            * math.sqrt(2)
        )
        require(
            math.isclose(
                first_mde,
                expected_first_mde,
                rel_tol=1e-10,
                abs_tol=1e-8,
            ),
            f"baseline metric mismatch: {metric_name} independently computed MDE",
        )
        require(
            math.isclose(
                finite_number(
                    stored.get("min_detectable_delta", 0),
                    f"baseline {metric_name} min_detectable_delta",
                ),
                first_mde,
                rel_tol=1e-10,
                abs_tol=1e-8,
            ),
            f"baseline metric mismatch: {metric_name}.min_detectable_delta",
        )
        for runs_per_side, actual in enumerate(stored_mde, start=1):
            require(
                math.isclose(
                    finite_number(
                        actual,
                        f"baseline {metric_name} MDE m={runs_per_side}",
                    ),
                    first_mde / math.sqrt(runs_per_side),
                    rel_tol=1e-10,
                    abs_tol=1e-8,
                ),
                f"baseline metric mismatch: {metric_name} MDE m={runs_per_side}",
            )
    return baseline


def extract_baseline_convergence(baseline: dict[str, Any]) -> dict[str, Any]:
    metrics = baseline.get("metrics")
    require(isinstance(metrics, dict), "baseline metrics missing for convergence")
    result: dict[str, Any] = {}
    for metric_name, _, _, _ in STABILITY_METRICS:
        stored = metrics.get(metric_name)
        require(isinstance(stored, dict), f"baseline is missing {metric_name}")
        sd_series = [
            finite_number(value, f"baseline {metric_name} convergence SD")
            for value in stored.get("sd_by_replicates", [])
        ]
        require(sd_series, f"baseline {metric_name} has no convergence SD series")
        first_index = max(0, len(sd_series) - 5)
        tail = sd_series[first_index:]
        final_sd = finite_number(stored.get("sd"), f"baseline {metric_name} SD")
        relative_span = (
            (max(tail) - min(tail)) / abs(final_sd) if final_sd != 0 else 0.0
        )
        mde = [
            finite_number(value, f"baseline {metric_name} MDE")
            for value in stored.get("min_detectable_delta_by_runs_per_side", [])[:5]
        ]
        require(len(mde) == 5, f"baseline {metric_name} has fewer than five MDE values")
        result[metric_name] = {
            "sd_last_five_replicate_counts": list(
                range(first_index + 2, first_index + 2 + len(tail))
            ),
            "sd_last_five": tail,
            "sd_last_five_relative_span": relative_span,
            "sd_last_five_within_10_percent_span": relative_span <= 0.10,
            "min_detectable_delta_runs_per_side_1_to_5": mde,
        }
    return result


def aggregate_resources(runs: list[dict[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    fields = (
        "rss_mean_bytes",
        "rss_peak_bytes",
        "docker_rss_mean_bytes",
        "docker_rss_peak_bytes",
        "postgres_memory_mean_bytes",
        "postgres_memory_peak_bytes",
        "redis_memory_mean_bytes",
        "redis_memory_peak_bytes",
        "sim_processes_min",
        "sim_processes_max",
        "sim_cpu_mean_logical_cores",
        "sim_cpu_peak_logical_cores",
        "tcp_established_peak",
        "mem_available_min_bytes",
        "swap_used_peak_bytes",
        "postgres_processes_min",
        "postgres_processes_max",
        "postgres_summed_rss_mean_bytes",
        "postgres_summed_rss_peak_bytes",
        "redis_processes_min",
        "redis_processes_max",
        "redis_summed_rss_mean_bytes",
        "redis_summed_rss_peak_bytes",
    )
    for field in fields:
        values = [
            finite_number(run["resources"][field], f"{run['tag']} resource {field}")
            for run in runs
            if field in run["resources"]
        ]
        if values:
            result[field] = describe(values)
    result["all_runs_have_rss_samples"] = all(
        run["resources"].get("rss_samples", 0) > 0 for run in runs
    )
    result["all_runs_have_full_host_coverage"] = all(
        run["resources"].get("host_coverage_fraction", 0) >= 0.95 for run in runs
    )
    result["all_runs_have_exactly_five_sim_processes"] = all(
        run["resources"].get("sim_processes_min") == 5
        and run["resources"].get("sim_processes_max") == 5
        for run in runs
    )
    result["all_runs_zero_swap"] = all(
        run["resources"].get("swap_used_peak_bytes") == 0 for run in runs
    )
    covered = [
        run for run in runs if run["resources"].get("service_samples", 0) > 0
    ]
    result["service_telemetry_covered_runs"] = len(covered)
    full_coverage = [
        run
        for run in covered
        if run["resources"].get("service_coverage_fraction", 0) >= 0.90
    ]
    result["service_telemetry_full_coverage_runs"] = len(full_coverage)
    result["service_telemetry_partial_coverage_tags"] = [
        run["tag"] for run in covered if run not in full_coverage
    ]
    result["service_telemetry_missing_tags"] = [
        run["tag"] for run in runs if run not in covered
    ]
    result["all_runs_have_service_samples"] = len(covered) == len(runs)
    result["all_service_covered_runs_have_postgres_and_redis"] = bool(covered) and all(
        run["resources"].get("postgres_processes_min", 0) > 0
        and run["resources"].get("redis_processes_min", 0) > 0
        for run in covered
    )
    return result


def aggregate_samples(runs: list[dict[str, Any]]) -> dict[str, Any]:
    if not all("sample_audit" in run for run in runs):
        return {}
    fields = {
        "samples": lambda audit: audit["samples"],
        "empty_pools": lambda audit: audit["empty_pools"],
        "sample_span_fraction": lambda audit: audit["sample_span_fraction"],
        "load_p95_ms": lambda audit: audit["load_p95_ms"],
        "pool_count_mean": lambda audit: audit["report"]["pool_count_mean"],
        "pool_count_p50": lambda audit: audit["report"]["pool_count_p50"],
        "pool_count_p95": lambda audit: audit["report"]["pool_count_p95"],
        "load_millis_mean": lambda audit: audit["report"]["load_millis_mean"],
        "selection_lift": lambda audit: audit["report"]["selection_lift"],
        "chosen_reliability_mean": lambda audit: audit["report"][
            "chosen_reliability_mean"
        ],
        "chosen_rel_latency_ms_mean": lambda audit: audit["report"][
            "chosen_rel_latency_ms_mean"
        ],
        "chosen_speed_mbps_mean": lambda audit: audit["report"][
            "chosen_speed_mbps_mean"
        ],
    }
    result = {
        name: describe(
            [
                finite_number(
                    getter(run["sample_audit"]), f"{run['tag']} samples {name}"
                )
                for run in runs
            ]
        )
        for name, getter in fields.items()
    }
    result["total_samples"] = sum(run["sample_audit"]["samples"] for run in runs)
    result["total_empty_pools"] = sum(
        run["sample_audit"]["empty_pools"] for run in runs
    )
    result["all_pools_nonempty"] = result["total_empty_pools"] == 0
    result["all_runs_sample_span_at_least_90_percent"] = all(
        run["sample_audit"]["sample_span_fraction"] >= 0.90 for run in runs
    )
    return result


def format_metric(name: str, value: float) -> str:
    if name in {"success_rate", "fail_rate"}:
        return f"{100 * value:.3f}%"
    if name == "received_bytes":
        return f"{value / (1024**3):.2f} GiB"
    if name.endswith("bytes_per_s"):
        return f"{value / 1000:.1f} kB/s"
    if name == "request_count":
        return f"{value:,.0f}"
    return f"{value:,.2f} ms"


def format_drift_t(value: dict[str, Any]) -> str:
    t_stat = value.get("t_stat")
    if t_stat is None:
        return "+∞" if value.get("perfect_fit_infinite_t_direction") == 1 else "−∞"
    return f"{finite_number(t_stat, 'drift t statistic'):.2f}"


def write_stability_svg(
    path: Path,
    runs: list[dict[str, Any]],
    baseline: dict[str, Any],
    campaign: dict[str, Any],
) -> None:
    """Render the required per-run stability series without external packages."""
    width = 1200
    height = 990
    left = 105.0
    right = 45.0
    plot_width = width - left - right
    panel_top = 115.0
    panel_height = 205.0
    panel_gap = 80.0

    tag_numbers = [int(run["tag"][1:]) for run in runs]
    excluded_numbers = [int(item["tag"][1:]) for item in campaign["excluded"]]
    all_numbers = tag_numbers + excluded_numbers
    require(bool(all_numbers), "cannot plot an empty campaign")
    first_attempt = min(all_numbers)
    last_attempt = max(all_numbers)
    attempt_span = max(1, last_attempt - first_attempt)

    def x_for(number: int) -> float:
        return left + plot_width * (number - first_attempt) / attempt_span

    def svg_text(x: float, y: float, value: str, **attrs: Any) -> str:
        attributes = " ".join(
            f'{name.replace("_", "-")}="{escape(str(attribute))}"'
            for name, attribute in attrs.items()
        )
        return (
            f'<text x="{x:.1f}" y="{y:.1f}" {attributes}>'
            f"{escape(value)}</text>"
        )

    parts = [
        '<?xml version="1.0" encoding="UTF-8"?>',
        (
            f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" '
            f'height="{height}" viewBox="0 0 {width} {height}" '
            'role="img" aria-labelledby="title desc">'
        ),
        '<title id="title">eval-48d baseline stability</title>',
        (
            '<desc id="desc">Authenticated per-run TTFB and throughput metrics, '
            'with baseline mean and one-run minimum detectable delta thresholds.</desc>'
        ),
        '<rect width="100%" height="100%" fill="#ffffff"/>',
        svg_text(
            left,
            38,
            "eval-48d local directional baseline stability",
            font_family="sans-serif",
            font_size="24",
            font_weight="700",
            fill="#172033",
        ),
        svg_text(
            left,
            66,
            (
                f"{len(runs)} authenticated runs; {campaign['excluded_count']} excluded "
                "attempt(s); local 24-logical-CPU host"
            ),
            font_family="sans-serif",
            font_size="14",
            fill="#4b5563",
        ),
        '<line x1="705" y1="62" x2="745" y2="62" stroke="#1677b8" stroke-width="2.5"/>',
        svg_text(753, 67, "authenticated run", font_family="sans-serif", font_size="12", fill="#374151"),
        '<line x1="880" y1="62" x2="920" y2="62" stroke="#737b88" stroke-width="1.5" stroke-dasharray="5 4"/>',
        svg_text(928, 67, "mean", font_family="sans-serif", font_size="12", fill="#374151"),
        '<line x1="1010" y1="62" x2="1050" y2="62" stroke="#c43d3d" stroke-width="1.5" stroke-dasharray="8 5"/>',
        svg_text(1058, 67, "mean ± MDE₁", font_family="sans-serif", font_size="12", fill="#374151"),
    ]

    baseline_metrics = baseline.get("metrics")
    require(isinstance(baseline_metrics, dict), "baseline metrics missing for plot")
    baseline_alpha = finite_number(baseline.get("alpha"), "baseline plot alpha")
    for panel, (metric_name, title, unit, divisor) in enumerate(STABILITY_METRICS):
        top = panel_top + panel * (panel_height + panel_gap)
        bottom = top + panel_height
        values = [
            finite_number(run["metrics"].get(metric_name), f"{run['tag']} {metric_name}")
            for run in runs
        ]
        stored = baseline_metrics.get(metric_name)
        require(isinstance(stored, dict), f"baseline is missing plot metric {metric_name}")
        mean = finite_number(stored.get("mean"), f"baseline {metric_name} mean")
        mde = finite_number(
            stored.get("min_detectable_delta", 0), f"baseline {metric_name} MDE"
        )
        lower_threshold = mean - mde
        upper_threshold = mean + mde
        y_min = min(*values, lower_threshold, mean)
        y_max = max(*values, upper_threshold, mean)
        y_span = y_max - y_min
        padding = max(y_span * 0.10, abs(mean) * 0.01, 1.0)
        y_min -= padding
        y_max += padding
        y_span = y_max - y_min

        def y_for(value: float) -> float:
            return bottom - panel_height * (value - y_min) / y_span

        parts.append(
            f'<rect x="{left:.1f}" y="{top:.1f}" width="{plot_width:.1f}" '
            f'height="{panel_height:.1f}" fill="#fbfcfe" stroke="#cfd6df"/>'
        )
        parts.append(
            svg_text(
                left,
                top - 17,
                f"{title} ({unit})",
                font_family="sans-serif",
                font_size="16",
                font_weight="600",
                fill="#172033",
            )
        )
        for grid_index in range(5):
            fraction = grid_index / 4
            y = bottom - fraction * panel_height
            displayed = (y_min + fraction * y_span) / divisor
            parts.append(
                f'<line x1="{left:.1f}" y1="{y:.1f}" x2="{left + plot_width:.1f}" '
                f'y2="{y:.1f}" stroke="#e4e8ee" stroke-width="1"/>'
            )
            parts.append(
                svg_text(
                    left - 10,
                    y + 4,
                    f"{displayed:,.1f}",
                    text_anchor="end",
                    font_family="sans-serif",
                    font_size="11",
                    fill="#5f6875",
                )
            )

        for excluded in excluded_numbers:
            x = x_for(excluded)
            parts.append(
                f'<line x1="{x:.1f}" y1="{top:.1f}" x2="{x:.1f}" y2="{bottom:.1f}" '
                'stroke="#d88989" stroke-width="1" stroke-dasharray="3 5"/>'
            )

        for value, color, dash, line_width in (
            (mean, "#737b88", "5 4", 1.5),
            (lower_threshold, "#c43d3d", "8 5", 1.5),
            (upper_threshold, "#c43d3d", "8 5", 1.5),
        ):
            y = y_for(value)
            parts.append(
                f'<line x1="{left:.1f}" y1="{y:.1f}" x2="{left + plot_width:.1f}" '
                f'y2="{y:.1f}" stroke="{color}" stroke-width="{line_width}" '
                f'stroke-dasharray="{dash}"/>'
            )

        points = " ".join(
            f"{x_for(number):.1f},{y_for(value):.1f}"
            for number, value in zip(tag_numbers, values)
        )
        parts.append(
            f'<polyline points="{points}" fill="none" stroke="#1677b8" '
            'stroke-width="2.5" stroke-linejoin="round" stroke-linecap="round"/>'
        )
        for run, number, value in zip(runs, tag_numbers, values):
            x = x_for(number)
            y = y_for(value)
            parts.append(
                f'<circle cx="{x:.1f}" cy="{y:.1f}" r="4" fill="#1677b8" '
                'stroke="#ffffff" stroke-width="1.5">'
                f'<title>{escape(run["tag"])}: {value / divisor:,.3f} {escape(unit)}</title>'
                '</circle>'
            )

        if panel == len(STABILITY_METRICS) - 1:
            for number in range(first_attempt, last_attempt + 1):
                x = x_for(number)
                parts.append(
                    f'<line x1="{x:.1f}" y1="{bottom:.1f}" x2="{x:.1f}" '
                    f'y2="{bottom + 5:.1f}" stroke="#68717e"/>'
                )
                parts.append(
                    svg_text(
                        x,
                        bottom + 20,
                        f"r{number:03d}",
                        text_anchor="middle",
                        font_family="sans-serif",
                        font_size="10",
                        fill="#5f6875",
                    )
                )

    excluded_text = ", ".join(item["tag"] for item in campaign["excluded"]) or "none"
    parts.extend(
        [
            svg_text(
                left,
                height - 34,
                f"Excluded attempt tags: {excluded_text}",
                font_family="sans-serif",
                font_size="12",
                fill="#5f6875",
            ),
            svg_text(
                width - right,
                height - 34,
                f"Threshold = baseline mean ± one-run-per-side minimum detectable delta (α={baseline_alpha:g})",
                text_anchor="end",
                font_family="sans-serif",
                font_size="12",
                fill="#5f6875",
            ),
            "</svg>",
        ]
    )
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("\n".join(parts) + "\n", encoding="utf-8")


def markdown(summary: dict[str, Any]) -> str:
    identity = summary["identity"]
    aggregate = summary["aggregate"]["metrics"]
    campaign = summary["campaign_attempts"]
    lines = [
        "# eval-48d local directional baseline",
        "",
        f"Status: **{summary['status'].upper()}**  ",
        f"Replicates: **{summary['replicate_count']} authenticated 30-minute runs**  ",
        f"Campaign attempts: **{campaign['attempt_count']} total; "
        f"{campaign['excluded_count']} excluded before aggregation**  ",
        f"Longest consecutive authenticated tag streak: "
        f"**{campaign['longest_consecutive_authenticated_tag_streak']}**  ",
        "Classification: **local/directional — not official Apex calibration**",
        "",
        "## Frozen identity",
        "",
        "| Field | Value |",
        "|---|---|",
        f"| providers SHA-256 | `{identity['config_sha256']}` |",
        f"| seed | `{identity['seed']}` |",
        f"| build revision | `{identity['build_revision']}` |",
        f"| build modified | `{str(identity['build_modified']).lower()}` |",
        f"| simulator binary SHA-256 | `{summary['binary_sha256']}` |",
        f"| host | `{identity['hostname']}` ({identity['os']}/{identity['arch']}, {identity['num_cpu']} logical CPUs) |",
        f"| request timeout | `{identity['request_timeout_ms']} ms` |",
        f"| measured duration | `{identity['duration_ms'] / 1000:.0f} s` per run |",
        "",
        "## Noise and drift",
        "",
        "| Metric | Mean | SD | CV | Min–max | Drift t |",
        "|---|---:|---:|---:|---:|---:|",
    ]
    for metric_name in METRIC_ORDER:
        if metric_name not in aggregate:
            continue
        item = aggregate[metric_name]
        lines.append(
            "| `{}` | {} | {} | {:.2f}% | {}–{} | {} |".format(
                metric_name,
                format_metric(metric_name, item["mean"]),
                format_metric(metric_name, item["sd"]),
                100 * item["cv"],
                format_metric(metric_name, item["min"]),
                format_metric(metric_name, item["max"]),
                format_drift_t(item["drift"]),
            )
        )
    lines.extend(
        [
            "",
            "Drift is an OLS slope against authenticated measurement-start elapsed",
            "hours (not ordinal run number); per-hour slopes and t statistics are in",
            "the JSON artifact.",
            "",
            "The Apex raw score is type-7 p95 total latency with every failed",
            "attempt charged at the frozen two-minute request-timeout ceiling.",
        ]
    )
    convergence = summary.get("baseline_convergence", {})
    if convergence:
        lines.extend(
            [
                "",
                "## Convergence and decision limits",
                "",
                "The SD sequences below are independently recomputed for the last five",
                "replicate counts. The 10% span flag is a directional stability heuristic,",
                "not an official Apex qualification threshold. Detection limits use the",
                f"baseline's one-sided α={summary['baseline_alpha']:.3g}.",
                "",
                "| Metric | Last five SD estimates (k:value) | Span/final SD | ≤10% span | Minimum detectable Δ, m=1…5 per side |",
                "|---|---|---:|---:|---|",
            ]
        )
        for metric_name, _, _, _ in STABILITY_METRICS:
            item = convergence[metric_name]
            sd_text = ", ".join(
                f"{k}:{format_metric(metric_name, value)}"
                for k, value in zip(
                    item["sd_last_five_replicate_counts"],
                    item["sd_last_five"],
                )
            )
            mde_text = ", ".join(
                format_metric(metric_name, value)
                for value in item["min_detectable_delta_runs_per_side_1_to_5"]
            )
            lines.append(
                f"| `{metric_name}` | {sd_text} | "
                f"{100 * item['sd_last_five_relative_span']:.2f}% | "
                f"`{str(item['sd_last_five_within_10_percent_span']).lower()}` | "
                f"{mde_text} |"
            )
        plot_name = Path(summary["stability_plot"]).name
        lines.extend(
            [
                "",
                f"Per-run series and decision thresholds: [{plot_name}]({plot_name}).",
            ]
        )
    raw = aggregate["apex_raw_score_ms"]
    bootstrap = summary["aggregate"]["apex_median_bootstrap_by_replicates"]
    raw_convergence = summary["aggregate"]["apex_raw_score_sd_convergence"]
    raw_sd_tail = ", ".join(
        f"k={k}: {value:,.2f} ms"
        for k, value in zip(
            raw_convergence["last_five_replicate_counts"],
            raw_convergence["last_five_sd"],
        )
    )
    lines.extend(
        [
            "",
            "## Apex scalar noise interpretation",
            "",
            f"- Single-run raw-score CV (`sigma_run / mean`): **{100 * raw['cv']:.3f}%**.",
            f"- Smallest takeover margin supported by the frozen quarter-margin rule: "
            f"**{100 * summary['aggregate']['minimum_relative_takeover_margin_supported_by_raw_cv']:.3f}%**.",
            f"- Single-run noise gate for a 1% takeover margin (`CV ≤ 0.25%`): "
            f"**{str(summary['aggregate']['one_percent_takeover_single_run_noise_gate']).upper()}**.",
            f"- Raw-score SD convergence over the last five k: {raw_sd_tail}; "
            f"span/final SD **{100 * raw_convergence['last_five_relative_span']:.2f}%** "
            f"(≤10% heuristic: `{str(raw_convergence['last_five_within_10_percent_span']).lower()}`).",
            "",
            "Deterministic bootstrap estimate for median-of-R candidate aggregation:",
            "",
            "| R | Median-score mean | SD | CV | Meets 0.25% CV |",
            "|---:|---:|---:|---:|---:|",
        ]
    )
    for replicate_count in ("1", "3", "5", "7"):
        if replicate_count not in bootstrap:
            continue
        item = bootstrap[replicate_count]
        lines.append(
            f"| {replicate_count} | {item['mean']:,.2f} ms | "
            f"{item['sd']:,.2f} ms | {100 * item['cv']:.3f}% | "
            f"`{str(item['cv'] <= 0.0025).lower()}` |"
        )
    lines.extend(
        [
            "",
            "These values size a local directional policy only. The official R and",
            "takeover margin remain open until independent-seed and reference-patch",
            "separability pass on the production hardware.",
        ]
    )
    lines.extend(
        [
            "",
            "## Per-run results",
            "",
            "| Run | Requests | Success | Raw score | TTFB p95 | Total p95 (successful) | Peak RSS |",
            "|---|---:|---:|---:|---:|---:|---:|",
        ]
    )
    for run in summary["runs"]:
        metrics = run["metrics"]
        peak_rss = run["resources"].get("rss_peak_bytes")
        peak_text = f"{peak_rss / (1024**3):.2f} GiB" if peak_rss else "n/a"
        lines.append(
            f"| `{run['tag']}` | {metrics['request_count']:,.0f} | "
            f"{100 * metrics['success_rate']:.3f}% | "
            f"{metrics['apex_raw_score_ms']:,.1f} ms | "
            f"{metrics['ttfb_p95_ms']:,.1f} ms | "
            f"{metrics['total_p95_ms']:,.1f} ms | {peak_text} |"
        )
    outliers = summary["aggregate"]["robust_outlier_candidates"]
    lines.extend(["", "## Robust outlier triage", ""])
    if not outliers:
        lines.append(
            "No authenticated run exceeded |modified z| = 3.5 on the frozen triage metrics."
        )
    else:
        lines.extend(
            [
                "Candidates are reported for contamination review and are not automatically",
                "excluded. One run may appear on more than one metric.",
                "",
                "| Run | Metric | Value | Median | Modified z |",
                "|---|---|---:|---:|---:|",
            ]
        )
        for finding in outliers:
            metric_name = finding["metric"]
            modified_z = finding["modified_z"]
            z_text = (
                f"{modified_z:.2f}"
                if modified_z is not None
                else ("+∞" if finding["value"] > finding["median"] else "−∞")
            )
            lines.append(
                f"| `{finding['tag']}` | `{metric_name}` | "
                f"{format_metric(metric_name, finding['value'])} | "
                f"{format_metric(metric_name, finding['median'])} | "
                f"{z_text} |"
            )
    if campaign["excluded"]:
        lines.extend(
            [
                "",
                "## Excluded attempts",
                "",
                "These attempts had no authenticated completion marker and were not used",
                "in the baseline or any aggregate metric. CSV identity mismatches are",
                "reported rather than repaired; they are additional evidence that an",
                "incomplete attempt is not scoreable.",
                "",
                "| Attempt | Reason | Warm clients | Measured rows | CSV identity |",
                "|---|---|---:|---:|---|",
            ]
        )
        for attempt in campaign["excluded"]:
            established = attempt.get("clients_established")
            pool = attempt.get("clients_pool")
            warm = (
                f"{established}/{pool}"
                if established is not None and pool is not None
                else "n/a"
            )
            rows = attempt.get("rows_in_window")
            rows_text = f"{rows:,}" if isinstance(rows, int) else "n/a"
            csv_match = attempt.get("csv_identity_matches_manifest")
            csv_identity_text = (
                "match"
                if csv_match is True
                else ("mismatch" if csv_match is False else "n/a")
            )
            lines.append(
                f"| `{attempt['tag']}` | `{attempt['reason']}` | {warm} | "
                f"{rows_text} | `{csv_identity_text}` |"
            )
    resources = summary["aggregate"]["resources"]
    lines.extend(["", "## Local resource envelope", ""])
    if "rss_peak_bytes" in resources:
        lines.append(
            f"- Per-run peak RSS: mean {resources['rss_peak_bytes']['mean'] / (1024**3):.2f} GiB; "
            f"maximum {resources['rss_peak_bytes']['max'] / (1024**3):.2f} GiB."
        )
    if "sim_cpu_mean_logical_cores" in resources:
        lines.append(
            f"- Simulator CPU: mean {resources['sim_cpu_mean_logical_cores']['mean']:.2f} "
            f"logical-core equivalents; maximum observed per-run mean "
            f"{resources['sim_cpu_mean_logical_cores']['max']:.2f}."
        )
    if "tcp_established_peak" in resources:
        lines.append(
            f"- Established TCP sockets: campaign run-peak maximum "
            f"{resources['tcp_established_peak']['max']:,.0f}."
        )
    covered_services = resources.get("service_telemetry_covered_runs", 0)
    fully_covered_services = resources.get(
        "service_telemetry_full_coverage_runs", 0
    )
    lines.append(
        f"- Native PostgreSQL/Redis process telemetry had samples for "
        f"{covered_services}/{summary['replicate_count']} runs and ≥90% window coverage "
        f"for {fully_covered_services}/{summary['replicate_count']}; partial tags: "
        f"{', '.join(resources.get('service_telemetry_partial_coverage_tags', [])) or 'none'}; "
        f"uncovered tags: "
        f"{', '.join(resources.get('service_telemetry_missing_tags', [])) or 'none'}."
    )
    if "postgres_summed_rss_peak_bytes" in resources:
        lines.append(
            f"- PostgreSQL summed process RSS: maximum recorded run peak "
            f"{resources['postgres_summed_rss_peak_bytes']['max'] / (1024**3):.2f} GiB. "
            "This double-counts shared pages across backends and is a trend diagnostic, "
            "not unique memory or a cgroup measurement."
        )
    if "redis_summed_rss_peak_bytes" in resources:
        lines.append(
            f"- Redis process RSS: maximum recorded run peak "
            f"{resources['redis_summed_rss_peak_bytes']['max'] / (1024**2):.1f} MiB."
        )
    if "mem_available_min_bytes" in resources:
        lines.append(
            f"- Minimum host memory available in any run: "
            f"{resources['mem_available_min_bytes']['min'] / (1024**3):.2f} GiB."
        )
    lines.extend(
        [
            f"- All runs had RSS coverage: `{str(resources['all_runs_have_rss_samples']).lower()}`.",
            f"- All runs had ≥95% expanded host-telemetry coverage: "
            f"`{str(resources['all_runs_have_full_host_coverage']).lower()}`.",
            f"- Every in-window host sample saw exactly one runner plus four fleet shards: "
            f"`{str(resources['all_runs_have_exactly_five_sim_processes']).lower()}`.",
            f"- Every sampled run remained at zero swap: "
            f"`{str(resources['all_runs_zero_swap']).lower()}`.",
            f"- Every service-covered run retained both PostgreSQL and Redis processes: "
            f"`{str(resources['all_service_covered_runs_have_postgres_and_redis']).lower()}`.",
        ]
    )
    samples = summary["aggregate"].get("samples", {})
    if samples:
        lines.extend(
            [
                "",
                "## Matchmaking evidence",
                "",
                f"- In-window FindProviders2 samples: {samples['total_samples']:,}.",
                f"- Empty candidate pools: {samples['total_empty_pools']:,}; all pools non-empty: "
                f"`{str(samples['all_pools_nonempty']).lower()}`.",
                f"- Minimum first-to-last sample span across runs: "
                f"{100 * samples['sample_span_fraction']['min']:.2f}% of the measured window; "
                f"every run covered at least 90%: "
                f"`{str(samples['all_runs_sample_span_at_least_90_percent']).lower()}`.",
                f"- Per-run load p95: mean {samples['load_p95_ms']['mean']:.2f} ms; "
                f"maximum {samples['load_p95_ms']['max']:.2f} ms.",
                f"- Pool count mean across runs: {samples['pool_count_mean']['mean']:.1f}; "
                f"selection lift mean {samples['selection_lift']['mean']:.3f}.",
            ]
        )
    heldout_aa = summary.get("heldout_aa")
    if heldout_aa:
        result = heldout_aa["result"]
        a_audit = heldout_aa["audited_runs"]["a"][0]
        b_audit = heldout_aa["audited_runs"]["b"][0]
        a_labels = ", ".join(Path(label).name for label in result["runs_a"])
        b_labels = ", ".join(Path(label).name for label in result["runs_b"])
        lines.extend(
            [
                "",
                "## Held-out A/A workflow check",
                "",
                f"- Verdict: **{result['verdict']}** at "
                f"α={finite_number(result['alpha'], 'held-out A/A alpha'):.3g}.",
                f"- Side A: `{a_labels}`; side B: `{b_labels}`.",
                f"- Reason: {result['reason']}",
                f"- Independently recomputed Apex raw scores: "
                f"A {a_audit['metrics']['apex_raw_score_ms']:,.2f} ms; "
                f"B {b_audit['metrics']['apex_raw_score_ms']:,.2f} ms; "
                f"B-vs-A delta "
                f"{100 * heldout_aa['apex_raw_score_relative_delta_b_vs_a']:+.3f}%.",
                f"- Authenticated comparison artifact: "
                f"`{heldout_aa['artifact']['path']}` "
                f"(`{heldout_aa['artifact']['sha256']}`).",
            ]
        )
    lines.extend(
        [
            "",
            "## Integrity and interpretation",
            "",
            f"- All {summary['replicate_count']} CSV/manifest/marker chains authenticated and matched their recorded hashes.",
            "- The campaign wrapper plus RSS, host-resource, and native-service telemetry are fingerprinted by byte length and SHA-256 in the JSON artifact.",
            "- Every run established its complete warm-client pool and retained finalized, individually SHA-256-fingerprinted FindProviders2 segments.",
            "- No audited stderr contained scorer G5 stability, panic, fatal, or `ENOBUFS` signatures.",
            "- Contract-close timeout notices during cancellation drain are reported per run as diagnostics; they did not prevent joined teardown or authenticated completion.",
            "- `baseline.json` and this report are noise-study artifacts, not a signed `sim-latency-score-baseline` manifest.",
            "- This host exposes 24 logical CPUs and the build is locally modified. These measurements must not be copied into an official signed Apex round baseline.",
            "- Native PostgreSQL/Redis sampling is observational; summed PostgreSQL process RSS double-counts shared pages and is not unique-memory accounting.",
            "- Official qualification still requires the exact 12-core hardware, independent hidden seeds, reference-patch separability, accounting/cgroup reports, and Macrocosmos acceptance.",
            "",
        ]
    )
    return "\n".join(lines)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--runs-dir", type=Path, default=Path("eval-48g/runs"))
    parser.add_argument("--baseline", type=Path, default=Path("eval-48g/baseline.json"))
    parser.add_argument("--rss", type=Path, default=Path("eval-48g/campaign-rss.csv"))
    parser.add_argument(
        "--host-resources",
        type=Path,
        default=Path("eval-48g/campaign-host-resources.csv"),
    )
    parser.add_argument(
        "--service-resources",
        type=Path,
        default=Path("eval-48g/campaign-service-resources.csv"),
    )
    parser.add_argument("--binary", type=Path, default=Path("sim-latency"))
    parser.add_argument(
        "--campaign-script", type=Path, default=Path("eval-48.sh")
    )
    parser.add_argument(
        "--sample-audit", type=Path, default=Path("eval-48g/baseline-samples")
    )
    parser.add_argument(
        "--heldout-compare",
        type=Path,
        default=Path("eval-48g/heldout-aa-compare.json"),
    )
    parser.add_argument(
        "--heldout-host-resources",
        type=Path,
        default=Path("eval-48g/heldout-host-resources.csv"),
    )
    parser.add_argument("--min-runs", type=int, default=20)
    parser.add_argument(
        "--allow-missing-baseline", action="store_true", help=argparse.SUPPRESS
    )
    parser.add_argument("--skip-sample-details", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument(
        "--out-json", type=Path, default=Path("eval-48g/baseline-summary.json")
    )
    parser.add_argument(
        "--out-md", type=Path, default=Path("eval-48g/baseline-summary.md")
    )
    parser.add_argument(
        "--out-svg", type=Path, default=Path("eval-48g/baseline-stability.svg")
    )
    args = parser.parse_args()

    workdir = Path.cwd().resolve()
    require(args.campaign_script.is_file(), "campaign wrapper is missing")
    try:
        campaign_script_payload = args.campaign_script.read_bytes()
    except OSError as exc:
        raise SummaryError(f"read campaign wrapper: {exc}") from exc
    campaign_script_artifact = payload_fingerprint(
        args.campaign_script, campaign_script_payload
    )
    marker_paths = sorted(args.runs_dir.glob("r*.run.json.complete.json"))
    require(
        len(marker_paths) >= args.min_runs,
        f"need at least {args.min_runs} authenticated runs; found {len(marker_paths)}",
    )
    rss_rows, rss_artifact = load_telemetry_artifact(
        args.rss, RSS_TELEMETRY_COLUMNS
    )
    host_rows, host_artifact = load_telemetry_artifact(
        args.host_resources, HOST_TELEMETRY_COLUMNS
    )
    service_rows, service_artifact = load_telemetry_artifact(
        args.service_resources, SERVICE_TELEMETRY_COLUMNS
    )
    runs = [
        audit_run(path, workdir, rss_rows, host_rows, service_rows)
        for path in marker_paths
    ]
    identity = same_identity(runs)
    campaign_attempts = audit_campaign_attempts(args.runs_dir, runs)
    sample_audit_sha256 = None
    if not args.skip_sample_details:
        sample_audit_sha256 = attach_sample_audits(args.sample_audit, runs)
    heldout_aa = load_heldout_comparison(args.heldout_compare)
    if heldout_aa is not None:
        heldout_host_rows, heldout_host_artifact = load_telemetry_artifact(
            args.heldout_host_resources, HOST_TELEMETRY_COLUMNS
        )
        attach_heldout_run_audits(
            heldout_aa,
            workdir,
            identity,
            runs,
            heldout_host_rows,
            heldout_host_artifact,
            service_rows,
            service_artifact,
        )

    measure_start_times = [
        finite_number(run["measure_start_ms"], f"{run['tag']} measure start")
        for run in runs
    ]
    metric_names = set.intersection(*(set(run["metrics"]) for run in runs))
    require(
        metric_names == set(METRIC_ORDER),
        "authenticated runs do not all expose the frozen summary metric set",
    )
    aggregate_metrics = {
        name: describe(
            [finite_number(run["metrics"][name], name) for run in runs],
            measure_start_times,
        )
        for name in sorted(metric_names)
    }
    raw_score_values = [run["metrics"]["apex_raw_score_ms"] for run in runs]
    aggregate = {
        "metrics": aggregate_metrics,
        "metric_drift_time_basis": "authenticated measure_start_ms elapsed hours",
        "robust_outlier_candidates": robust_outlier_candidates(runs),
        "resources": aggregate_resources(runs),
        "samples": aggregate_samples(runs),
        "apex_raw_score_sd_convergence": sd_convergence(raw_score_values),
        "apex_median_bootstrap_by_replicates": bootstrap_median_noise(
            raw_score_values
        ),
        "minimum_relative_takeover_margin_supported_by_raw_cv": 4
        * aggregate_metrics["apex_raw_score_ms"]["cv"],
        "one_percent_takeover_single_run_noise_gate": aggregate_metrics[
            "apex_raw_score_ms"
        ]["cv"]
        <= 0.0025,
        "all_success_rates_pass_97_percent": all(
            run["metrics"]["success_rate"] >= 0.97 for run in runs
        ),
        "all_warm_pools_complete": all(
            run["clients_established"] == run["clients_pool"] for run in runs
        ),
        "all_logs_clean": all(
            not run["log_findings"]["scorer_stability_findings"]
            and run["log_findings"][key] == 0
            for run in runs
            for key in ("panic_lines", "fatal_lines", "enobufs_lines")
        ),
    }

    baseline = None
    baseline_convergence: dict[str, Any] = {}
    if args.baseline.is_file():
        baseline = cross_check_baseline(args.baseline, runs, aggregate_metrics)
        baseline_convergence = extract_baseline_convergence(baseline)
        if heldout_aa is not None:
            require(
                math.isclose(
                    finite_number(
                        heldout_aa["result"].get("alpha"),
                        "held-out A/A alpha",
                    ),
                    finite_number(baseline.get("alpha"), "baseline alpha"),
                    rel_tol=0,
                    abs_tol=1e-15,
                ),
                "held-out A/A alpha differs from the baseline",
            )
    elif not args.allow_missing_baseline:
        raise SummaryError(f"baseline artifact missing: {args.baseline}")

    stability_plot_sha256 = None
    if baseline is not None:
        write_stability_svg(args.out_svg, runs, baseline, campaign_attempts)
        stability_plot_sha256 = file_sha256(args.out_svg)

    completed_ms = max(integer(run["completed_unix_ms"], "completion time") for run in runs)
    summary = {
        "schema": SUMMARY_SCHEMA,
        "kind": SUMMARY_KIND,
        "status": "complete",
        "classification": "local_directional",
        "generated_from_completed_unix_ms": completed_ms,
        "generated_from_completed_utc": datetime.fromtimestamp(
            completed_ms / 1000, tz=timezone.utc
        ).isoformat(),
        "replicate_count": len(runs),
        "campaign_attempts": campaign_attempts,
        "identity": identity,
        "binary_sha256": file_sha256(args.binary),
        "campaign_script": campaign_script_artifact,
        "telemetry_artifacts": {
            "rss": rss_artifact,
            "host_resources": host_artifact,
            "native_services": service_artifact,
        },
        "sample_audit_sha256": sample_audit_sha256,
        "heldout_aa": heldout_aa,
        "baseline_sha256": file_sha256(args.baseline) if baseline is not None else None,
        "baseline_alpha": (
            finite_number(baseline.get("alpha"), "baseline alpha")
            if baseline is not None
            else None
        ),
        "baseline_convergence": baseline_convergence,
        "stability_plot": str(args.out_svg) if baseline is not None else None,
        "stability_plot_sha256": stability_plot_sha256,
        "aggregate": aggregate,
        "runs": runs,
        "caveats": [
            "local directional campaign on a 24-logical-CPU host, not official 12-core hardware",
            "locally modified build; official mode was intentionally not asserted",
            "same seed only; independent-seed and reference-separability calibration remain open",
            "local telemetry is observational rather than an official cgroup resource report",
            "native PostgreSQL/Redis telemetry is observational and PostgreSQL summed RSS double-counts shared pages",
            "FindProviders2 protobuf metrics are independently recomputed by the recorded sample-audit helper",
            "baseline.json and this summary are noise-study artifacts, not a signed sim-latency-score-baseline manifest",
        ],
    }

    args.out_json.parent.mkdir(parents=True, exist_ok=True)
    args.out_md.parent.mkdir(parents=True, exist_ok=True)
    args.out_json.write_text(
        json.dumps(summary, indent=2, sort_keys=True, allow_nan=False) + "\n"
    )
    args.out_md.write_text(markdown(summary), encoding="utf-8")
    print(
        f"wrote {args.out_json} and {args.out_md}: "
        f"{len(runs)} authenticated local/directional replicates"
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except SummaryError as exc:
        raise SystemExit(f"baseline summary: {exc}") from exc
