#!/usr/bin/env python3
"""Create deterministic JSON/Markdown evidence from eval-frontier-12c runs."""

from __future__ import annotations

import argparse
import json
import math
import statistics
from pathlib import Path
from typing import Any


def load_json(path: Path) -> dict[str, Any]:
    return json.loads(path.read_text(encoding="utf-8"))


def quantile_type7(values: list[float], probability: float) -> float:
    """Return the score-contract's linear (R/type-7) sample quantile."""
    if not values:
        raise ValueError("quantile requires at least one value")
    if not 0.0 <= probability <= 1.0:
        raise ValueError("quantile probability must be within [0, 1]")
    ordered = sorted(values)
    if len(ordered) == 1:
        return ordered[0]
    position = (len(ordered) - 1) * probability
    lower = math.floor(position)
    upper = math.ceil(position)
    fraction = position - lower
    return ordered[lower] + fraction * (ordered[upper] - ordered[lower])


def linear_trend(values: list[float]) -> tuple[float | None, float | None]:
    """Return least-squares slope per observation and its two-sided t statistic."""
    if len(values) < 3:
        return None, None
    x_values = [float(index) for index in range(len(values))]
    x_mean = statistics.fmean(x_values)
    y_mean = statistics.fmean(values)
    x_sum_squares = sum((value - x_mean) ** 2 for value in x_values)
    slope = sum(
        (x_value - x_mean) * (y_value - y_mean)
        for x_value, y_value in zip(x_values, values, strict=True)
    ) / x_sum_squares
    intercept = y_mean - slope * x_mean
    residual_sum_squares = sum(
        (y_value - (intercept + slope * x_value)) ** 2
        for x_value, y_value in zip(x_values, values, strict=True)
    )
    slope_standard_error = math.sqrt(
        residual_sum_squares / (len(values) - 2) / x_sum_squares
    )
    if slope_standard_error == 0.0:
        return slope, None
    return slope, slope / slope_standard_error


def pearson_correlation(left: list[float], right: list[float]) -> float | None:
    if len(left) != len(right) or len(left) < 2:
        raise ValueError("correlation inputs must have equal length of at least 2")
    left_mean = statistics.fmean(left)
    right_mean = statistics.fmean(right)
    numerator = sum(
        (left_value - left_mean) * (right_value - right_mean)
        for left_value, right_value in zip(left, right, strict=True)
    )
    left_sum_squares = sum((value - left_mean) ** 2 for value in left)
    right_sum_squares = sum((value - right_mean) ** 2 for value in right)
    denominator = math.sqrt(left_sum_squares * right_sum_squares)
    if denominator == 0.0:
        return None
    return numerator / denominator


def optional_correlation(
    left: list[float], right: list[float | None]
) -> float | None:
    if any(value is None for value in right):
        return None
    return pearson_correlation(left, [float(value) for value in right if value is not None])


def optional_max(values: list[float | None]) -> float | None:
    if any(value is None for value in values):
        return None
    return max(float(value) for value in values if value is not None)


def pair_key(tag: str) -> str:
    for suffix in ("-no-impair", "-noimpair", "-impair"):
        if tag.endswith(suffix):
            return f"{tag.removesuffix(suffix)}-mode"
    return tag


def count_log_lines(path: Path, needles: tuple[str, ...]) -> int:
    if not path.is_file():
        return 0
    count = 0
    with path.open("r", encoding="utf-8", errors="replace") as source:
        for line in source:
            lowered = line.lower()
            if any(needle in lowered for needle in needles):
                count += 1
    return count


def run_row(summary_path: Path) -> dict[str, Any]:
    summary = load_json(summary_path)
    run_path = summary_path.with_name("run.json")
    run = load_json(run_path)
    profile = summary["profile"]
    metrics = summary["metrics"]
    resources = summary["resources"]
    samples = summary["samples"]
    stderr_path = summary_path.with_name("stderr.log")
    mode = "impair" if run["flags"]["impair"] == "true" else "no-impair"
    return {
        "profile": profile["profile"],
        "run_tag": summary_path.parent.name,
        "pair_key": pair_key(summary_path.parent.name),
        "mode": mode,
        "providers": profile["providers"],
        "clients": profile["clients"],
        "arrivals_per_minute": profile["rate_per_minute"],
        "quality_window_size": profile.get("quality_window_size", 0),
        "duration_seconds": summary["identity"]["measure_duration_s"],
        "measure_start_ms": run["measure_start_ms"],
        "completed_unix_ms": run["completed_unix_ms"],
        "seed": profile["seed"],
        "frontier_eligible": summary["frontier_eligible"],
        "raw_score_ms": metrics["apex_raw_score_ms"],
        "success_rate": metrics["success_rate"],
        "request_count": metrics["request_count"],
        "received_bytes": metrics["received_bytes"],
        "goodput_bytes_per_s": metrics["goodput_bytes_per_s"],
        "ttfb_p95_ms": metrics["ttfb_p95_ms"],
        "throughput_p50_bytes_per_s": metrics["throughput_p50_bytes_per_s"],
        "findproviders_load_p95_ms": samples["load_p95_ms"],
        "findproviders_pool_mean": samples["pool_count_mean"],
        "findproviders_empty_pools": samples["empty_pools"],
        "cpu_mean_cores": resources["sim_cpu_mean_cores"],
        "cpu_p95_cores": resources["sim_cpu_p95_cores"],
        "cpu_peak_cores": resources["sim_cpu_peak_cores"],
        "rss_peak_gib": resources["sim_rss_peak_gib"],
        "host_load1_mean": resources.get("host_load1_mean"),
        "host_load1_p95": resources.get("host_load1_p95"),
        "host_load1_peak": resources.get("host_load1_peak"),
        "postgres_rss_peak_gib": resources.get("postgres_rss_peak_gib"),
        "redis_rss_peak_gib": resources.get("redis_rss_peak_gib"),
        "swap_peak_kib": resources["swap_used_peak_kib"],
        "tcp_established_peak": resources["tcp_established_peak"],
        "contract_cleanup_unauthorized_count": count_log_lines(
            stderr_path,
            ("after client close = 401 unauthorized",),
        ),
        "warm_client_retry_count": count_log_lines(
            stderr_path,
            ("warm client pool attempt",),
        ),
        "enobufs_count": count_log_lines(
            stderr_path,
            ("enobufs", "no buffer space available"),
        ),
        "config_sha256": summary["identity"]["config_sha256"],
        "simulator_sha256": profile["simulator_sha256"],
        "summary_path": str(summary_path),
        "production_qualified": summary["production_qualified"],
        "failed_gates": sorted(name for name, passed in summary["gates"].items() if not passed),
    }


def build_report(root: Path, simulator_sha256: str | None = None) -> dict[str, Any]:
    summary_paths = sorted(root.glob("profiles/*/runs/*/summary.json"))
    if not summary_paths:
        raise ValueError(f"no frontier summaries found below {root}")
    rows = [run_row(path) for path in summary_paths]
    if simulator_sha256 is not None:
        rows = [row for row in rows if row["simulator_sha256"] == simulator_sha256]
        if not rows:
            raise ValueError(f"no frontier summaries match simulator SHA-256 {simulator_sha256}")
    groups: dict[tuple[str, str], dict[str, dict[str, Any]]] = {}
    for row in rows:
        groups.setdefault((row["profile"], row["pair_key"]), {})[row["mode"]] = row

    pairs: list[dict[str, Any]] = []
    paired_runs: set[tuple[str, str]] = set()

    def add_pair(profile: str, key: str, impaired: dict[str, Any], clean: dict[str, Any]) -> None:
        if impaired["duration_seconds"] != clean["duration_seconds"]:
            return
        paired_runs.add((profile, impaired["run_tag"]))
        paired_runs.add((profile, clean["run_tag"]))
        pairs.append(
            {
                "profile": profile,
                "pair_key": key,
                "impaired_run_tag": impaired["run_tag"],
                "no_impair_run_tag": clean["run_tag"],
                "both_frontier_eligible": impaired["frontier_eligible"] and clean["frontier_eligible"],
                "raw_impair_ms": impaired["raw_score_ms"],
                "raw_no_impair_ms": clean["raw_score_ms"],
                "impairment_raw_delta_percent": 100.0
                * (impaired["raw_score_ms"] / clean["raw_score_ms"] - 1.0),
                "success_impair": impaired["success_rate"],
                "success_no_impair": clean["success_rate"],
                "impairment_success_delta_percentage_points": 100.0
                * (impaired["success_rate"] - clean["success_rate"]),
            }
        )

    for (profile, key), modes in sorted(groups.items()):
        if set(modes) != {"impair", "no-impair"}:
            continue
        add_pair(profile, key, modes["impair"], modes["no-impair"])

    # A deliberately order-reversed sweep may number its two observations
    # independently (for example r001-no-impair then r002-impair). Pair the
    # remaining singleton modes only when that association is unambiguous.
    rows_by_profile: dict[str, list[dict[str, Any]]] = {}
    for row in rows:
        if (row["profile"], row["run_tag"]) not in paired_runs:
            rows_by_profile.setdefault(row["profile"], []).append(row)
    for profile, unmatched in sorted(rows_by_profile.items()):
        impaired = [row for row in unmatched if row["mode"] == "impair"]
        clean = [row for row in unmatched if row["mode"] == "no-impair"]
        if len(impaired) == 1 and len(clean) == 1:
            add_pair(profile, "order-reversed-singleton", impaired[0], clean[0])

    pairs.sort(key=lambda pair: (pair["profile"], pair["pair_key"]))

    campaign_groups: dict[tuple[str, str], list[dict[str, Any]]] = {}
    for row in rows:
        campaign_groups.setdefault((row["profile"], row["mode"]), []).append(row)
    campaigns: list[dict[str, Any]] = []
    for (profile, mode), campaign_rows in sorted(campaign_groups.items()):
        if len(campaign_rows) < 2:
            continue
        campaign_rows.sort(key=lambda row: (row["measure_start_ms"], row["run_tag"]))
        raw_scores = [row["raw_score_ms"] for row in campaign_rows]
        success_rates = [row["success_rate"] for row in campaign_rows]
        request_counts = [row["request_count"] for row in campaign_rows]
        ttfb_p95_values = [row["ttfb_p95_ms"] for row in campaign_rows]
        throughput_p50_values = [row["throughput_p50_bytes_per_s"] for row in campaign_rows]
        goodput_values = [row["goodput_bytes_per_s"] for row in campaign_rows]
        findproviders_load_p95_values = [row["findproviders_load_p95_ms"] for row in campaign_rows]
        cpu_mean_values = [row["cpu_mean_cores"] for row in campaign_rows]
        rss_peak_values = [row["rss_peak_gib"] for row in campaign_rows]
        host_load1_mean_values = [row["host_load1_mean"] for row in campaign_rows]
        postgres_rss_peak_values = [row["postgres_rss_peak_gib"] for row in campaign_rows]
        redis_rss_peak_values = [row["redis_rss_peak_gib"] for row in campaign_rows]
        raw_mean = statistics.fmean(raw_scores)
        raw_sd = statistics.stdev(raw_scores)
        raw_cv = raw_sd / raw_mean
        supported_margin = 4.0 * raw_cv
        raw_trend_slope, raw_trend_t = linear_trend(raw_scores)
        raw_score_prefix_convergence = []
        for prefix_count in range(2, len(raw_scores) + 1):
            prefix_scores = raw_scores[:prefix_count]
            prefix_mean = statistics.fmean(prefix_scores)
            prefix_sd = statistics.stdev(prefix_scores)
            prefix_cv = prefix_sd / prefix_mean
            prefix_margin = 4.0 * prefix_cv
            raw_score_prefix_convergence.append(
                {
                    "run_count": prefix_count,
                    "mean_ms": prefix_mean,
                    "sample_sd_ms": prefix_sd,
                    "cv": prefix_cv,
                    "minimum_relative_takeover_margin_supported_by_raw_cv": prefix_margin,
                    "takeover_threshold_ms": prefix_mean * (1.0 - prefix_margin),
                }
            )
        raw_sd_last_five_relative_span = None
        raw_sd_converged_within_10_percent = None
        if len(raw_score_prefix_convergence) >= 5:
            last_five_sd = [
                point["sample_sd_ms"] for point in raw_score_prefix_convergence[-5:]
            ]
            raw_sd_last_five_relative_span = (max(last_five_sd) - min(last_five_sd)) / raw_sd
            raw_sd_converged_within_10_percent = raw_sd_last_five_relative_span <= 0.10
        campaigns.append(
            {
                "profile": profile,
                "mode": mode,
                "simulator_sha256": campaign_rows[0]["simulator_sha256"],
                "config_sha256": campaign_rows[0]["config_sha256"],
                "seed": campaign_rows[0]["seed"],
                "providers": campaign_rows[0]["providers"],
                "clients": campaign_rows[0]["clients"],
                "arrivals_per_minute": campaign_rows[0]["arrivals_per_minute"],
                "quality_window_size": campaign_rows[0]["quality_window_size"],
                "duration_seconds": campaign_rows[0]["duration_seconds"],
                "run_count": len(campaign_rows),
                "all_frontier_eligible": all(row["frontier_eligible"] for row in campaign_rows),
                "raw_score_mean_ms": raw_mean,
                "raw_score_sample_sd_ms": raw_sd,
                "raw_score_sample_sd_relative_standard_error_estimate": 1.0
                / math.sqrt(2.0 * (len(raw_scores) - 1)),
                "raw_score_sample_sd_last_five_prefix_relative_span": raw_sd_last_five_relative_span,
                "raw_score_sample_sd_converged_within_10_percent": raw_sd_converged_within_10_percent,
                "raw_score_cv": raw_cv,
                "raw_score_min_ms": min(raw_scores),
                "raw_score_p05_ms": quantile_type7(raw_scores, 0.05),
                "raw_score_median_ms": quantile_type7(raw_scores, 0.5),
                "raw_score_p95_ms": quantile_type7(raw_scores, 0.95),
                "raw_score_max_ms": max(raw_scores),
                "raw_score_linear_slope_ms_per_run": raw_trend_slope,
                "raw_score_linear_slope_t_statistic": raw_trend_t,
                "minimum_relative_takeover_margin_supported_by_raw_cv": supported_margin,
                "raw_takeover_threshold_ms": raw_mean * (1.0 - supported_margin),
                "success_rate_mean": statistics.fmean(success_rates),
                "success_rate_sample_sd": statistics.stdev(success_rates),
                "success_rate_min": min(success_rates),
                "success_rate_max": max(success_rates),
                "request_count_min": min(request_counts),
                "request_count_max": max(request_counts),
                "ttfb_p95_mean_ms": statistics.fmean(ttfb_p95_values),
                "ttfb_p95_sample_sd_ms": statistics.stdev(ttfb_p95_values),
                "throughput_p50_mean_bytes_per_s": statistics.fmean(throughput_p50_values),
                "throughput_p50_sample_sd_bytes_per_s": statistics.stdev(throughput_p50_values),
                "goodput_mean_bytes_per_s": statistics.fmean(goodput_values),
                "goodput_sample_sd_bytes_per_s": statistics.stdev(goodput_values),
                "findproviders_load_p95_max_ms": max(findproviders_load_p95_values),
                "raw_score_diagnostic_correlations": {
                    "success_rate": pearson_correlation(raw_scores, success_rates),
                    "request_count": pearson_correlation(raw_scores, request_counts),
                    "ttfb_p95_ms": pearson_correlation(raw_scores, ttfb_p95_values),
                    "throughput_p50_bytes_per_s": pearson_correlation(
                        raw_scores, throughput_p50_values
                    ),
                    "goodput_bytes_per_s": pearson_correlation(raw_scores, goodput_values),
                    "findproviders_load_p95_ms": pearson_correlation(
                        raw_scores, findproviders_load_p95_values
                    ),
                    "cpu_mean_cores": pearson_correlation(raw_scores, cpu_mean_values),
                    "rss_peak_gib": pearson_correlation(raw_scores, rss_peak_values),
                    "host_load1_mean": optional_correlation(
                        raw_scores, host_load1_mean_values
                    ),
                    "postgres_rss_peak_gib": optional_correlation(
                        raw_scores, postgres_rss_peak_values
                    ),
                    "redis_rss_peak_gib": optional_correlation(
                        raw_scores, redis_rss_peak_values
                    ),
                },
                "cpu_mean_cores_max": max(row["cpu_mean_cores"] for row in campaign_rows),
                "rss_peak_gib_max": max(row["rss_peak_gib"] for row in campaign_rows),
                "host_load1_mean_max": optional_max(host_load1_mean_values),
                "postgres_rss_peak_gib_max": optional_max(postgres_rss_peak_values),
                "redis_rss_peak_gib_max": optional_max(redis_rss_peak_values),
                "empty_pool_count": sum(row["findproviders_empty_pools"] for row in campaign_rows),
                "cleanup_401_count": sum(row["contract_cleanup_unauthorized_count"] for row in campaign_rows),
                "warm_client_retry_count": sum(row["warm_client_retry_count"] for row in campaign_rows),
                "runs_with_warm_client_retry": sum(
                    row["warm_client_retry_count"] > 0 for row in campaign_rows
                ),
                "run_tags": [row["run_tag"] for row in campaign_rows],
                "measure_start_ms": [row["measure_start_ms"] for row in campaign_rows],
                "raw_score_sequence_ms": raw_scores,
                "raw_score_prefix_convergence": raw_score_prefix_convergence,
                "success_rate_sequence": [row["success_rate"] for row in campaign_rows],
                "request_count_sequence": request_counts,
                "ttfb_p95_sequence_ms": ttfb_p95_values,
                "throughput_p50_sequence_bytes_per_s": throughput_p50_values,
                "goodput_sequence_bytes_per_s": goodput_values,
                "findproviders_load_p95_sequence_ms": findproviders_load_p95_values,
                "host_load1_mean_sequence": host_load1_mean_values,
                "postgres_rss_peak_gib_sequence": postgres_rss_peak_values,
                "redis_rss_peak_gib_sequence": redis_rss_peak_values,
            }
        )

    identities = sorted({row["simulator_sha256"] for row in rows})
    config_identities = sorted({row["config_sha256"] for row in rows})
    seeds = sorted({row["seed"] for row in rows})
    durations = sorted({row["duration_seconds"] for row in rows})
    profiles = sorted({row["profile"] for row in rows})
    modes = sorted({row["mode"] for row in rows})
    return {
        "schema": 1,
        "kind": "sim-latency-frontier-summary",
        "classification": "production_candidate_unprivileged",
        "production_qualified": bool(rows) and all(row["production_qualified"] for row in rows),
        "run_count": len(rows),
        "eligible_run_count": sum(row["frontier_eligible"] for row in rows),
        "all_frontier_eligible": all(row["frontier_eligible"] for row in rows),
        "empty_pool_count": sum(row["findproviders_empty_pools"] for row in rows),
        "cleanup_401_count": sum(row["contract_cleanup_unauthorized_count"] for row in rows),
        "warm_client_retry_count": sum(row["warm_client_retry_count"] for row in rows),
        "runs_with_warm_client_retry": sum(row["warm_client_retry_count"] > 0 for row in rows),
        "enobufs_count": sum(row["enobufs_count"] for row in rows),
        "diagnostic_telemetry_complete": all(
            row[field] is not None
            for row in rows
            for field in (
                "host_load1_mean",
                "host_load1_p95",
                "host_load1_peak",
                "postgres_rss_peak_gib",
                "redis_rss_peak_gib",
            )
        ),
        "simulator_sha256_values": identities,
        "config_sha256_values": config_identities,
        "seed_values": seeds,
        "duration_seconds_values": durations,
        "profile_values": profiles,
        "mode_values": modes,
        "single_campaign_identity": all(
            len(values) == 1
            for values in (identities, config_identities, seeds, durations, profiles, modes)
        ),
        "runs": rows,
        "impairment_pairs": pairs,
        "campaigns": campaigns,
    }


def markdown(report: dict[str, Any]) -> str:
    lines = [
        "# 12-core frontier summary",
        "",
        f"Classification: `{report['classification']}`; production qualified: "
        f"`{str(report['production_qualified']).lower()}`.",
        "",
        "| profile | tag | mode | eligible | raw ms | success | CPU mean | RSS GiB | load p95 ms | empty | warm retries | cleanup 401 |",
        "|---|---|---|:---:|---:|---:|---:|---:|---:|---:|---:|---:|",
    ]
    for row in report["runs"]:
        lines.append(
            f"| `{row['profile']}` | `{row['run_tag']}` | {row['mode']} | "
            f"{'yes' if row['frontier_eligible'] else 'no'} | {row['raw_score_ms']:.2f} | "
            f"{100 * row['success_rate']:.3f}% | {row['cpu_mean_cores']:.2f} | "
            f"{row['rss_peak_gib']:.2f} | {row['findproviders_load_p95_ms']:.2f} | "
            f"{row['findproviders_empty_pools']} | {row['warm_client_retry_count']} | "
            f"{row['contract_cleanup_unauthorized_count']} |"
        )
    lines.extend(
        [
            "",
            "## Impairment pairs",
            "",
            "Positive raw delta means impairment was slower (lower is better).",
            "",
            "| profile | pair | both eligible | impair ms | no-impair ms | raw delta | success delta |",
            "|---|---|:---:|---:|---:|---:|---:|",
        ]
    )
    for pair in report["impairment_pairs"]:
        lines.append(
            f"| `{pair['profile']}` | `{pair['pair_key']}` | "
            f"{'yes' if pair['both_frontier_eligible'] else 'no'} | "
            f"{pair['raw_impair_ms']:.2f} | {pair['raw_no_impair_ms']:.2f} | "
            f"{pair['impairment_raw_delta_percent']:+.3f}% | "
            f"{pair['impairment_success_delta_percentage_points']:+.3f} pp |"
        )
    lines.extend(
        [
            "",
            "## Repeated-run campaigns",
            "",
            "The threshold uses the frozen local quarter-margin diagnostic: mean × (1 − 4 × raw-score CV). It is directional until official calibration.",
            "",
            "| profile | mode | runs | eligible | raw mean ms | raw median ms | raw p05–p95 ms | raw CV | threshold ms | success range | max CPU | max RSS GiB | warm retries | cleanup 401 |",
            "|---|---|---:|:---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|",
        ]
    )
    for campaign in report["campaigns"]:
        lines.append(
            f"| `{campaign['profile']}` | {campaign['mode']} | {campaign['run_count']} | "
            f"{'yes' if campaign['all_frontier_eligible'] else 'no'} | "
            f"{campaign['raw_score_mean_ms']:.2f} | {campaign['raw_score_median_ms']:.2f} | "
            f"{campaign['raw_score_p05_ms']:.2f}–{campaign['raw_score_p95_ms']:.2f} | "
            f"{100 * campaign['raw_score_cv']:.3f}% | "
            f"{campaign['raw_takeover_threshold_ms']:.2f} | "
            f"{100 * campaign['success_rate_min']:.3f}–{100 * campaign['success_rate_max']:.3f}% | "
            f"{campaign['cpu_mean_cores_max']:.2f} | {campaign['rss_peak_gib_max']:.2f} | "
            f"{campaign['warm_client_retry_count']} | {campaign['cleanup_401_count']} |"
        )
    lines.extend(
        [
            "",
            "These runs constrain simulator children to 12 physical-core siblings but do not "
            "qualify the production host: SMT/governor/turbo and the PostgreSQL/Redis cgroup "
            "boundary are not frozen, and only one host is available.",
            "",
        ]
    )
    return "\n".join(lines)


def write_atomic(path: Path, content: bytes) -> None:
    temporary = path.with_name(f".{path.name}.tmp")
    temporary.write_bytes(content)
    temporary.replace(path)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", type=Path, default=Path(__file__).with_name("eval-12c"))
    parser.add_argument("--out-json", type=Path)
    parser.add_argument("--out-md", type=Path)
    parser.add_argument("--simulator-sha256")
    parser.add_argument("--expect-runs", type=int)
    parser.add_argument("--require-clean-campaign", action="store_true")
    args = parser.parse_args()
    if args.expect_runs is not None and args.expect_runs < 2:
        parser.error("--expect-runs must be at least 2")
    if args.simulator_sha256 is not None and (
        len(args.simulator_sha256) != 64
        or any(character not in "0123456789abcdef" for character in args.simulator_sha256)
    ):
        parser.error("--simulator-sha256 must be 64 lowercase hexadecimal characters")
    try:
        report = build_report(args.root.resolve(), args.simulator_sha256)
    except ValueError as error:
        parser.error(str(error))
    if args.expect_runs is not None and report["run_count"] != args.expect_runs:
        parser.error(
            f"expected exactly {args.expect_runs} runs, found {report['run_count']}"
        )
    if args.require_clean_campaign:
        clean = (
            report["all_frontier_eligible"]
            and report["single_campaign_identity"]
            and report["empty_pool_count"] == 0
            and report["cleanup_401_count"] == 0
            and report["enobufs_count"] == 0
            and report["diagnostic_telemetry_complete"]
            and len(report["campaigns"]) == 1
            and report["campaigns"][0]["run_count"] == report["run_count"]
        )
        if not clean:
            parser.error("campaign is not a single clean, fully eligible run series")
    encoded = (json.dumps(report, indent=2, sort_keys=True, allow_nan=False) + "\n").encode()
    if args.out_json:
        write_atomic(args.out_json, encoded)
    if args.out_md:
        write_atomic(args.out_md, markdown(report).encode())
    if not args.out_json:
        print(encoded.decode(), end="")


if __name__ == "__main__":
    main()
