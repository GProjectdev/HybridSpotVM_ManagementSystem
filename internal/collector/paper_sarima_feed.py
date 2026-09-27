#!/usr/bin/env python3
"""Paper-method SARIMA risk feed for SpotRiskProfile paperEstimator.

Implements the preprocessing described in Cost-Efficient Training and
Checkpointing for Large Models on Preemptible Cloud VMs, Section 4.1: sample
spot VM availability counts every g seconds, convert downward availability
changes to raw preemption counts, aggregate those counts into hourly rates,
smooth with a causal rolling mean, and fit SARIMA(1,1,1)(1,0,1)[24].
"""

from __future__ import annotations

import argparse
import json
import math
import ssl
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from typing import Any, Iterable

import pandas as pd


DEFAULT_ROLLING_MEAN_HOURS = 3
DEFAULT_RETRAIN_WINDOW_WEEKS = 4
DEFAULT_SEASONAL_PERIOD_HOURS = 24
DEFAULT_MAX_LAMBDA_PER_HOUR = 100.0
MAX_ROLLING_MEAN_HOURS = 24 * 7
MAX_RETRAIN_WINDOW_WEEKS = 52
MAX_SEASONAL_PERIOD_HOURS = 24 * 7
MAX_RETRAIN_SECONDS = 52 * 7 * 24 * 3600
MAX_VALID_FOR_SECONDS = 3600


@dataclass(frozen=True)
class EstimatorConfig:
    rolling_mean_hours: int = DEFAULT_ROLLING_MEAN_HOURS
    retrain_window_weeks: int = DEFAULT_RETRAIN_WINDOW_WEEKS
    seasonal_period_hours: int = DEFAULT_SEASONAL_PERIOD_HOURS
    max_lambda_per_hour: float = DEFAULT_MAX_LAMBDA_PER_HOUR
    valid_for_seconds: int = 3600
    min_history_hours: int = DEFAULT_RETRAIN_WINDOW_WEEKS * 7 * 24
    retrain_seconds: int = 7 * 24 * 3600


def validate_config(config: EstimatorConfig) -> EstimatorConfig:
    if config.rolling_mean_hours <= 0 or config.rolling_mean_hours > MAX_ROLLING_MEAN_HOURS:
        raise ValueError(f"rolling_mean_hours must be in [1,{MAX_ROLLING_MEAN_HOURS}]")
    if config.retrain_window_weeks <= 0 or config.retrain_window_weeks > MAX_RETRAIN_WINDOW_WEEKS:
        raise ValueError(f"retrain_window_weeks must be in [1,{MAX_RETRAIN_WINDOW_WEEKS}]")
    if config.seasonal_period_hours <= 0 or config.seasonal_period_hours > MAX_SEASONAL_PERIOD_HOURS:
        raise ValueError(f"seasonal_period_hours must be in [1,{MAX_SEASONAL_PERIOD_HOURS}]")
    if not math.isfinite(config.max_lambda_per_hour) or config.max_lambda_per_hour <= 0:
        raise ValueError("max_lambda_per_hour must be finite and positive")
    if config.valid_for_seconds <= 0 or config.valid_for_seconds > MAX_VALID_FOR_SECONDS:
        raise ValueError("valid_for_seconds must be positive and no more than one forecast hour")
    max_history = config.retrain_window_weeks * 7 * 24
    if config.min_history_hours <= 0 or config.min_history_hours > max_history:
        raise ValueError("min_history_hours must be positive and no more than the retrain window")
    if config.retrain_seconds <= 0 or config.retrain_seconds > MAX_RETRAIN_SECONDS:
        raise ValueError(f"retrain_seconds must be in [1,{MAX_RETRAIN_SECONDS}]")
    return config


@dataclass(frozen=True)
class AvailabilityTrace:
    counts: list[int]
    gap_seconds: int
    risk_population: int
    start_time: datetime | None = None


def parse_timestamp(value: str) -> datetime:
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        raise ValueError("timestamps must include a timezone")
    return parsed.astimezone(timezone.utc)


def finite_nonnegative_int(value: Any, label: str) -> int:
    if isinstance(value, bool):
        raise ValueError(f"{label} must be a nonnegative integer")
    numeric = float(value)
    if not math.isfinite(numeric) or numeric < 0 or numeric != math.floor(numeric):
        raise ValueError(f"{label} must be a nonnegative integer")
    return int(numeric)


def risk_population_from(raw: dict[str, Any], metadata: dict[str, Any]) -> int:
    value = raw.get("risk_population") or raw.get("riskPopulation") or metadata.get("risk_population") or metadata.get("riskPopulation")
    if value is None:
        raise ValueError("risk_population is required to convert aggregate forecast counts to approximate per-instance hazard")
    population = finite_nonnegative_int(value, "risk_population")
    if population <= 0:
        raise ValueError("risk_population must be positive")
    return population


def load_availability_trace(path: Path) -> AvailabilityTrace:
    raw = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(raw, dict):
        raise ValueError("input must be an object with availability data")
    if "observations" in raw or "preemptions" in raw:
        raise ValueError("event/exposure observations are not the paper availability-count schema")

    metadata = raw.get("metadata", {})
    if metadata is None:
        metadata = {}
    if not isinstance(metadata, dict):
        raise ValueError("metadata must be an object when present")

    population = risk_population_from(raw, metadata)

    if isinstance(raw.get("data"), list):
        counts = [finite_nonnegative_int(v, f"data[{idx}]") for idx, v in enumerate(raw["data"])]
        gap = finite_nonnegative_int(metadata.get("gap_seconds", raw.get("gap_seconds")), "gap_seconds")
        start_value = metadata.get("start_time") or metadata.get("startTime") or raw.get("startTime")
        if not isinstance(start_value, str):
            raise ValueError("metadata.start_time is required for data[] traces")
        start_time = parse_timestamp(start_value)
        return validate_trace(AvailabilityTrace(counts=counts, gap_seconds=gap, risk_population=population, start_time=start_time))

    availability = raw.get("availability")
    if not isinstance(availability, list):
        raise ValueError("input must contain data[] counts or availability[] samples")
    counts: list[int] = []
    timestamps: list[datetime] = []
    for idx, item in enumerate(availability):
        if not isinstance(item, dict):
            raise ValueError(f"availability[{idx}] must be an object")
        value = item.get("available")
        if value is None:
            value = item.get("availableInstances")
        counts.append(finite_nonnegative_int(value, f"availability[{idx}].available"))
        ts_value = item.get("timestamp") or item.get("observedAt")
        if not isinstance(ts_value, str):
            raise ValueError(f"availability[{idx}] missing timestamp")
        timestamps.append(parse_timestamp(ts_value))
    if len(timestamps) < 2:
        raise ValueError("need at least two availability samples")
    gap_seconds = int((timestamps[1] - timestamps[0]).total_seconds())
    if any(int((timestamps[i] - timestamps[i - 1]).total_seconds()) != gap_seconds for i in range(2, len(timestamps))):
        raise ValueError("availability timestamps must have a fixed sampling gap")
    return validate_trace(AvailabilityTrace(counts=counts, gap_seconds=gap_seconds, risk_population=population, start_time=timestamps[0]))


def validate_trace(trace: AvailabilityTrace) -> AvailabilityTrace:
    if trace.gap_seconds <= 0:
        raise ValueError("gap_seconds must be positive")
    if 3600 % trace.gap_seconds != 0:
        raise ValueError("gap_seconds must divide one hour so hourly bins are complete")
    if len(trace.counts) < 2:
        raise ValueError("need at least two availability samples")
    if trace.start_time is None:
        raise ValueError("start_time is required")
    if trace.start_time.minute or trace.start_time.second or trace.start_time.microsecond:
        raise ValueError("start_time must be aligned to an hour boundary")
    total_seconds = (len(trace.counts) - 1) * trace.gap_seconds
    if total_seconds < 3600 or total_seconds % 3600 != 0:
        raise ValueError("availability evidence must cover complete hourly bins")
    return trace


def hourly_preemption_rates(trace: AvailabilityTrace) -> pd.Series:
    rows: list[tuple[pd.Timestamp, int]] = []
    if trace.start_time is None:
        raise ValueError("start_time is required")
    base = trace.start_time
    for idx in range(1, len(trace.counts)):
        raw_count = max(0, trace.counts[idx - 1] - trace.counts[idx])
        interval_start = base + timedelta(seconds=(idx - 1) * trace.gap_seconds)
        hour = pd.Timestamp(interval_start).floor("h")
        rows.append((hour, raw_count))
    if not rows:
        raise ValueError("need at least two availability samples")
    frame = pd.DataFrame(rows, columns=["hour", "preemptions"])
    hourly = frame.groupby("hour", sort=True)["preemptions"].sum().astype(float)
    return require_complete_hourly_index(hourly)


def require_complete_hourly_index(rate: pd.Series) -> pd.Series:
    if rate.empty:
        raise ValueError("need at least one hourly preemption rate")
    expected = pd.date_range(start=rate.index.min(), end=rate.index.max(), freq="h")
    missing = expected.difference(rate.index)
    if len(missing) > 0:
        first = missing[0].isoformat()
        raise ValueError(f"missing hourly availability evidence at {first}; refusing to fill with zero")
    return rate.reindex(expected)


def training_window(rate: pd.Series, config: EstimatorConfig) -> pd.Series:
    max_hours = config.retrain_window_weeks * 7 * 24
    window = rate.tail(max_hours)
    if len(window) < config.min_history_hours:
        raise ValueError(f"need at least {config.min_history_hours} hourly rates, got {len(window)}")
    return window


def causal_rolling_mean(rate: pd.Series, hours: int) -> pd.Series:
    if hours <= 0:
        raise ValueError("rolling_mean_hours must be positive")
    return rate.rolling(window=hours, min_periods=1).mean()


def forecast_lambda_per_hour(rate: pd.Series, config: EstimatorConfig) -> float:
    predicted, _ = forecast_aggregate_preemptions_per_hour(rate, config)
    return predicted


def forecast_aggregate_preemptions_per_hour(rate: pd.Series, config: EstimatorConfig, fitted_model: Any | None = None) -> tuple[float, Any]:
    try:
        from statsmodels.tsa.statespace.sarimax import SARIMAX
    except ImportError as exc:
        raise RuntimeError("statsmodels is required for the paper SARIMA estimator") from exc

    window = training_window(rate, config)
    smoothed = causal_rolling_mean(window, config.rolling_mean_hours)
    if fitted_model is not None:
        fitted = fitted_model.apply(smoothed, refit=False)
    else:
        model = SARIMAX(
            smoothed,
            order=(1, 1, 1),
            seasonal_order=(1, 0, 1, config.seasonal_period_hours),
            enforce_stationarity=False,
            enforce_invertibility=False,
        )
        fitted = model.fit(disp=False)
        if not fitted.mle_retvals.get("converged", False):
            raise ValueError("SARIMA fit did not converge")
    predicted = float(fitted.forecast(steps=1).iloc[0])
    if not math.isfinite(predicted):
        raise ValueError("SARIMA forecast was not finite")
    return max(predicted, 0.0), fitted


def feed_payload(input_path: Path, config: EstimatorConfig, now: datetime | None = None, fitted_model: Any | None = None, return_model: bool = False) -> dict[str, Any] | tuple[dict[str, Any], Any]:
    config = validate_config(config)
    now = (now or datetime.now(timezone.utc)).astimezone(timezone.utc)
    trace = load_availability_trace(input_path)
    rate = hourly_preemption_rates(trace)
    aggregate_forecast_per_hour, fitted = forecast_aggregate_preemptions_per_hour(rate, config, fitted_model)
    lambda_per_hour = min(aggregate_forecast_per_hour / float(trace.risk_population), config.max_lambda_per_hour)
    last_observed_hour_start = rate.index.max().to_pydatetime().astimezone(timezone.utc)
    observed_at = last_observed_hour_start + timedelta(hours=1)
    forecast_target = observed_at
    valid_until = forecast_target + timedelta(seconds=config.valid_for_seconds)
    if observed_at > now:
        raise ValueError("availability evidence is from the future")
    if valid_until <= now:
        raise ValueError("forecast target has expired; provide current availability evidence")
    payload = {
        "lambdaPerHour": lambda_per_hour,
        "observedAt": observed_at.isoformat().replace("+00:00", "Z"),
        "validUntil": valid_until.isoformat().replace("+00:00", "Z"),
        "source": {
            "type": "paper-sarima",
            "provenance": "paper-availability-count-sarima",
            "rollingMeanHours": config.rolling_mean_hours,
            "retrainWindowWeeks": config.retrain_window_weeks,
            "seasonalPeriodHours": config.seasonal_period_hours,
            "samplingGapSeconds": trace.gap_seconds,
            "riskPopulation": trace.risk_population,
            "aggregateForecastPreemptionsPerHour": aggregate_forecast_per_hour,
            "perInstanceLambdaApproximation": "approximate aggregateForecastPreemptionsPerHour/riskPopulation; risk_population must represent the at-risk population for this feed",
            "lastObservedHourStart": last_observed_hour_start.isoformat().replace("+00:00", "Z"),
            "forecastTargetHour": forecast_target.isoformat().replace("+00:00", "Z"),
        },
    }
    if return_model:
        return payload, fitted
    return payload


class FeedHandler(BaseHTTPRequestHandler):
    input_path: Path
    config: EstimatorConfig
    cached_payload: dict[str, Any] | None = None
    cached_input_mtime_ns: int | None = None
    cached_retrain_after: datetime | None = None
    cached_model: Any | None = None
    cached_model_retrain_after: datetime | None = None

    @classmethod
    def cached_feed_payload(cls, now: datetime) -> dict[str, Any]:
        mtime_ns = cls.input_path.stat().st_mtime_ns
        if (
            cls.cached_payload is not None
            and cls.cached_input_mtime_ns == mtime_ns
            and cls.cached_retrain_after is not None
            and now < cls.cached_retrain_after
            and cached_payload_valid(cls.cached_payload, now)
        ):
            return cls.cached_payload
        model = cls.cached_model if cls.cached_model_retrain_after is not None and now < cls.cached_model_retrain_after else None
        payload, fitted = feed_payload(cls.input_path, cls.config, now=now, fitted_model=model, return_model=True)
        cls.cached_payload = payload
        cls.cached_input_mtime_ns = mtime_ns
        cls.cached_retrain_after = now + timedelta(seconds=cls.config.retrain_seconds)
        if model is None:
            cls.cached_model = fitted
            cls.cached_model_retrain_after = now + timedelta(seconds=cls.config.retrain_seconds)
        return payload


    def do_GET(self) -> None:  # noqa: N802
        if self.path not in ("/", "/feed"):
            self.send_error(404)
            return
        try:
            now = datetime.now(timezone.utc)
            body = json.dumps(self.cached_feed_payload(now), separators=(",", ":")).encode("utf-8")
        except Exception as exc:  # pragma: no cover - exercised in deployment smoke tests.
            self.send_error(503, str(exc))
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def cached_payload_valid(payload: dict[str, Any], now: datetime) -> bool:
    value = payload.get("validUntil")
    if not isinstance(value, str):
        return False
    try:
        valid_until = parse_timestamp(value)
    except ValueError:
        return False
    return valid_until > now


def build_config(args: argparse.Namespace) -> EstimatorConfig:
    return validate_config(EstimatorConfig(
        rolling_mean_hours=args.rolling_mean_hours,
        retrain_window_weeks=args.retrain_window_weeks,
        seasonal_period_hours=args.seasonal_period_hours,
        max_lambda_per_hour=args.max_lambda_per_hour,
        valid_for_seconds=args.valid_for_seconds,
        min_history_hours=args.min_history_hours,
        retrain_seconds=args.retrain_seconds,
    ))


def main() -> int:
    parser = argparse.ArgumentParser(description="Serve the paper-method SARIMA preemption risk feed")
    parser.add_argument("--input", required=True, type=Path, help="JSON availability counts sampled at fixed gap_seconds")
    parser.add_argument("--once", action="store_true", help="print one feed response and exit")
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", default=8443, type=int)
    parser.add_argument("--certfile", type=Path)
    parser.add_argument("--keyfile", type=Path)
    parser.add_argument("--rolling-mean-hours", default=DEFAULT_ROLLING_MEAN_HOURS, type=int)
    parser.add_argument("--retrain-window-weeks", default=DEFAULT_RETRAIN_WINDOW_WEEKS, type=int)
    parser.add_argument("--seasonal-period-hours", default=DEFAULT_SEASONAL_PERIOD_HOURS, type=int)
    parser.add_argument("--max-lambda-per-hour", default=DEFAULT_MAX_LAMBDA_PER_HOUR, type=float)
    parser.add_argument("--valid-for-seconds", default=3600, type=int)
    parser.add_argument("--min-history-hours", default=DEFAULT_RETRAIN_WINDOW_WEEKS * 7 * 24, type=int)
    parser.add_argument("--retrain-seconds", default=7 * 24 * 3600, type=int, help="minimum interval between SARIMA refits when the input file is unchanged")
    args = parser.parse_args()
    config = build_config(args)

    if args.once:
        print(json.dumps(feed_payload(args.input, config), indent=2, sort_keys=True))
        return 0

    if args.certfile is None or args.keyfile is None:
        raise SystemExit("HTTPS service mode requires --certfile and --keyfile")
    FeedHandler.input_path = args.input
    FeedHandler.config = config
    server = HTTPServer((args.host, args.port), FeedHandler)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(str(args.certfile), str(args.keyfile))
    server.socket = context.wrap_socket(server.socket, server_side=True)
    server.serve_forever()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())