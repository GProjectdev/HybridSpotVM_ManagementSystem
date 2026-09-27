import tempfile
import unittest
from datetime import datetime, timedelta, timezone
from pathlib import Path
from unittest import mock

import paper_sarima_feed as sarima


class PaperSARIMAPreprocessingTest(unittest.TestCase):
    def write_json(self, payload):
        handle = tempfile.NamedTemporaryFile("w", suffix=".json", delete=False, encoding="utf-8")
        try:
            import json

            json.dump(payload, handle)
            return Path(handle.name)
        finally:
            handle.close()

    def test_availability_counts_become_hourly_downward_preemption_rates(self):
        path = self.write_json(
            {
                "metadata": {"gap_seconds": 1800, "start_time": "2026-09-26T00:00:00Z", "risk_population": 10},
                "data": [10, 8, 9, 5, 5],
            }
        )
        trace = sarima.load_availability_trace(path)
        rates = sarima.hourly_preemption_rates(trace)
        self.assertEqual(list(rates), [2.0, 4.0])

    def test_missing_hour_is_rejected_not_filled_with_zero(self):
        index = [
            sarima.pd.Timestamp("2026-09-26T00:00:00Z"),
            sarima.pd.Timestamp("2026-09-26T02:00:00Z"),
        ]
        series = sarima.pd.Series([1.0, 2.0], index=index)
        with self.assertRaisesRegex(ValueError, "refusing to fill with zero"):
            sarima.require_complete_hourly_index(series)

    def test_min_history_defaults_to_paper_four_week_window(self):
        config = sarima.EstimatorConfig()
        self.assertEqual(config.min_history_hours, 4 * 7 * 24)
        short = sarima.pd.Series([0.0] * 24, index=sarima.pd.date_range("2026-09-01", periods=24, freq="h", tz="UTC"))
        with self.assertRaisesRegex(ValueError, "672 hourly rates"):
            sarima.training_window(short, config)

    def test_feed_payload_uses_last_observed_hour_and_rejects_expired_trace(self):
        start = datetime(2026, 9, 26, 0, 0, tzinfo=timezone.utc)
        path = self.write_json(
            {
                "metadata": {"gap_seconds": 3600, "start_time": start.isoformat().replace("+00:00", "Z"), "risk_population": 5},
                "data": [5, 4, 3, 2],
            }
        )
        config = sarima.EstimatorConfig(min_history_hours=1, valid_for_seconds=3600)
        with mock.patch.object(sarima, "forecast_aggregate_preemptions_per_hour", return_value=(0.5, object())):
            payload = sarima.feed_payload(path, config, now=start + timedelta(hours=3, minutes=30))
        self.assertEqual(payload["lambdaPerHour"], 0.1)
        self.assertEqual(payload["source"]["aggregateForecastPreemptionsPerHour"], 0.5)
        self.assertEqual(payload["source"]["riskPopulation"], 5)
        self.assertEqual(payload["observedAt"], "2026-09-26T03:00:00Z")
        self.assertEqual(payload["validUntil"], "2026-09-26T04:00:00Z")
        self.assertEqual(payload["source"]["lastObservedHourStart"], "2026-09-26T02:00:00Z")
        self.assertEqual(payload["source"]["forecastTargetHour"], "2026-09-26T03:00:00Z")
        with mock.patch.object(sarima, "forecast_aggregate_preemptions_per_hour", return_value=(0.5, object())):
            with self.assertRaisesRegex(ValueError, "expired"):
                sarima.feed_payload(path, config, now=start + timedelta(hours=4, minutes=1))


    def test_http_cache_does_not_refit_unchanged_input_before_weekly_retrain(self):
        start = datetime(2026, 9, 26, 0, 0, tzinfo=timezone.utc)
        path = self.write_json(
            {
                "metadata": {"gap_seconds": 3600, "start_time": start.isoformat().replace("+00:00", "Z"), "risk_population": 10},
                "data": [10, 9, 8, 7],
            }
        )
        sarima.FeedHandler.input_path = path
        sarima.FeedHandler.config = sarima.EstimatorConfig(min_history_hours=1, retrain_seconds=7 * 24 * 3600)
        sarima.FeedHandler.cached_payload = None
        sarima.FeedHandler.cached_input_mtime_ns = None
        sarima.FeedHandler.cached_retrain_after = None
        sarima.FeedHandler.cached_model = None
        sarima.FeedHandler.cached_model_retrain_after = None
        with mock.patch.object(sarima, "forecast_aggregate_preemptions_per_hour", return_value=(1.0, object())) as forecast:
            first = sarima.FeedHandler.cached_feed_payload(start + timedelta(hours=3))
            second = sarima.FeedHandler.cached_feed_payload(start + timedelta(hours=3, minutes=30))
        self.assertEqual(first, second)
        self.assertEqual(forecast.call_count, 1)
        with mock.patch.object(sarima, "forecast_aggregate_preemptions_per_hour", return_value=(1.0, object())):
            with self.assertRaisesRegex(ValueError, "expired"):
                sarima.FeedHandler.cached_feed_payload(start + timedelta(hours=4, minutes=1))

    def test_new_data_before_weekly_retrain_reuses_cached_model_parameters(self):
        start = datetime(2026, 9, 26, 0, 0, tzinfo=timezone.utc)
        path = self.write_json(
            {
                "metadata": {"gap_seconds": 3600, "start_time": start.isoformat().replace("+00:00", "Z"), "risk_population": 10},
                "data": [10, 9, 8, 7],
            }
        )
        sarima.FeedHandler.input_path = path
        sarima.FeedHandler.config = sarima.EstimatorConfig(min_history_hours=1, retrain_seconds=7 * 24 * 3600)
        sarima.FeedHandler.cached_payload = None
        sarima.FeedHandler.cached_input_mtime_ns = None
        sarima.FeedHandler.cached_retrain_after = None
        sarima.FeedHandler.cached_model = None
        sarima.FeedHandler.cached_model_retrain_after = None
        first_model = object()
        with mock.patch.object(sarima, "forecast_aggregate_preemptions_per_hour", return_value=(1.0, first_model)) as forecast:
            sarima.FeedHandler.cached_feed_payload(start + timedelta(hours=3))
        import json, os
        path.write_text(json.dumps({"metadata": {"gap_seconds": 3600, "start_time": start.isoformat().replace("+00:00", "Z"), "risk_population": 10}, "data": [10, 9, 8, 7, 6]}), encoding="utf-8")
        os.utime(path, None)
        with mock.patch.object(sarima, "forecast_aggregate_preemptions_per_hour", return_value=(1.2, first_model)) as forecast:
            payload = sarima.FeedHandler.cached_feed_payload(start + timedelta(hours=4))
        self.assertEqual(payload["lambdaPerHour"], 0.12)
        self.assertIs(forecast.call_args.args[2], first_model)

    def test_per_instance_cap_applies_after_risk_population_division(self):
        start = datetime(2026, 9, 26, 0, 0, tzinfo=timezone.utc)
        path = self.write_json({"metadata": {"gap_seconds": 3600, "start_time": start.isoformat().replace("+00:00", "Z"), "risk_population": 10}, "data": [10, 0]})
        config = sarima.EstimatorConfig(min_history_hours=1, max_lambda_per_hour=0.5)
        with mock.patch.object(sarima, "forecast_aggregate_preemptions_per_hour", return_value=(100.0, object())):
            payload = sarima.feed_payload(path, config, now=start + timedelta(hours=1))
        self.assertEqual(payload["lambdaPerHour"], 0.5)
        self.assertEqual(payload["source"]["aggregateForecastPreemptionsPerHour"], 100.0)

    def test_future_evidence_is_rejected(self):
        start = datetime(2026, 9, 26, 0, 0, tzinfo=timezone.utc)
        path = self.write_json({"metadata": {"gap_seconds": 3600, "start_time": start.isoformat().replace("+00:00", "Z"), "risk_population": 10}, "data": [10, 9]})
        with mock.patch.object(sarima, "forecast_aggregate_preemptions_per_hour", return_value=(1.0, object())):
            with self.assertRaisesRegex(ValueError, "future"):
                sarima.feed_payload(path, sarima.EstimatorConfig(min_history_hours=1), now=start + timedelta(minutes=30))

    def test_max_request_size_alias_is_not_accepted_as_risk_population(self):
        path = self.write_json({"metadata": {"gap_seconds": 3600, "start_time": "2026-09-26T00:00:00Z", "max_request_size": 10}, "data": [10, 9]})
        with self.assertRaisesRegex(ValueError, "risk_population"):
            sarima.load_availability_trace(path)


    def test_config_validation_rejects_invalid_bounds(self):
        bad_configs = [
            (sarima.EstimatorConfig(rolling_mean_hours=0), "rolling_mean_hours"),
            (sarima.EstimatorConfig(retrain_window_weeks=0, min_history_hours=1), "retrain_window_weeks"),
            (sarima.EstimatorConfig(seasonal_period_hours=0), "seasonal_period_hours"),
            (sarima.EstimatorConfig(max_lambda_per_hour=0), "max_lambda_per_hour"),
            (sarima.EstimatorConfig(valid_for_seconds=3601), "valid_for_seconds"),
            (sarima.EstimatorConfig(min_history_hours=9999), "min_history_hours"),
            (sarima.EstimatorConfig(retrain_seconds=0), "retrain_seconds"),
        ]
        for config, want in bad_configs:
            with self.subTest(want=want):
                with self.assertRaisesRegex(ValueError, want):
                    sarima.validate_config(config)

    def test_real_statsmodels_fit_regression_when_available(self):
        import importlib.util
        if importlib.util.find_spec("statsmodels") is None:
            self.skipTest("statsmodels is not installed")
        index = sarima.pd.date_range("2026-09-01", periods=96, freq="h", tz="UTC")
        values = [max(0.0, 0.6 + 0.2 * ((i % 24) / 24.0)) for i in range(96)]
        rate = sarima.pd.Series(values, index=index)
        value = sarima.forecast_lambda_per_hour(rate, sarima.EstimatorConfig(min_history_hours=72, retrain_window_weeks=1))
        self.assertGreaterEqual(value, 0.0)
        self.assertTrue(sarima.math.isfinite(value))

    def test_event_exposure_schema_is_not_paper_input(self):
        path = self.write_json({"observations": [{"timestamp": "2026-09-26T00:00:00Z", "preemptions": 1}]})
        with self.assertRaisesRegex(ValueError, "not the paper availability-count schema"):
            sarima.load_availability_trace(path)


if __name__ == "__main__":
    unittest.main()