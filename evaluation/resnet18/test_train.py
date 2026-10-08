import dataclasses
from datetime import datetime, timezone
import io
import json
import random
import sys
import unittest
from unittest.mock import Mock, patch

import train_resnet18 as train


def config(**changes):
    initial = train.parse_config([], {
        "EVAL_RUN_ID": "test-run", "EVAL_GOAL_STEPS": "5",
        "EVAL_BATCH_SIZE": "2", "EVAL_SEED": "17", "RANK": "0", "WORLD_SIZE": "2",
    })
    return dataclasses.replace(initial, **changes)


class ConfigurationTests(unittest.TestCase):
    def test_environment_interface(self):
        cfg = config()
        self.assertEqual((cfg.run_id, cfg.goal_steps, cfg.batch_size, cfg.seed), ("test-run", 5, 2, 17))
        self.assertEqual(cfg.data_root, "/datasets/cifar10")

    def test_reject_invalid_settings(self):
        valid = {"EVAL_RUN_ID": "run", "RANK": "0", "WORLD_SIZE": "2"}
        for key, value in (("EVAL_RUN_ID", ""), ("EVAL_RUN_ID", "bad id"), ("EVAL_GOAL_STEPS", "0"),
                           ("EVAL_BATCH_SIZE", "-1"), ("EVAL_SEED", "-1"),
                           ("WORLD_SIZE", "0"), ("RANK", "2"), ("EVAL_GOAL_STEPS", "NaN")):
            with self.subTest(key=key), patch("sys.stderr", io.StringIO()), self.assertRaises(SystemExit):
                train.parse_config([], dict(valid, **{key: value}))

    def test_contract_binds_resume_semantics_but_not_rank_or_mount(self):
        cfg = config()
        original = train.contract_digest(cfg, 50000)
        for change in ({"seed": 18}, {"run_id": "other"}, {"batch_size": 4},
                       {"world_size": 3}, {"goal_steps": 6}, {"learning_rate": 0.2}):
            self.assertNotEqual(original, train.contract_digest(dataclasses.replace(cfg, **change), 50000))
        self.assertEqual(original, train.contract_digest(
            dataclasses.replace(cfg, rank=1, local_rank=1, data_root="/other"), 50000))


class DataTests(unittest.TestCase):
    def sampler(self, rank=0, start=0, goal=12):
        return train.StepBatchSampler(23, 3, 2, rank, 19, start, goal)

    def test_every_restart_is_exact_suffix_including_epoch_boundary(self):
        for rank in (0, 1):
            uninterrupted = list(self.sampler(rank))
            for step in range(13):
                self.assertEqual(list(self.sampler(rank, step)), uninterrupted[step:])

    def test_rank_batches_are_disjoint_and_drop_only_epoch_tail(self):
        streams = [list(self.sampler(rank, goal=3)) for rank in (0, 1)]
        items = [key for stream in streams for batch in stream for key in batch]
        self.assertEqual(len(items), 18)
        self.assertEqual(len(set(items)), 18)
        self.assertTrue(all(epoch == 0 and 0 <= index < 23 for epoch, index in items))

    def test_shuffle_and_augmentation_do_not_consume_global_rng(self):
        state = random.getstate()
        first = list(self.sampler())
        params = [train.augmentation_parameters(19, e, i) for batch in first for e, i in batch]
        self.assertEqual(random.getstate(), state)
        self.assertEqual(params, [train.augmentation_parameters(19, e, i) for batch in first for e, i in batch])
        self.assertTrue(all(0 <= top <= 8 and 0 <= left <= 8 for top, left, _ in params))
        self.assertNotEqual(first[0], first[3])

    def test_empty_or_invalid_sampler_fails(self):
        for args in ((3, 2, 2, 0, 0, 0, 1), (20, 2, 2, 2, 0, 0, 1), (20, 2, 2, 0, 0, 2, 1)):
            with self.assertRaises(ValueError):
                train.StepBatchSampler(*args)


class RuntimeTests(unittest.TestCase):
    def test_main_restores_before_ddp_without_startup_collectives(self):
        optimizer, fluidcr, torch = self.fixture(2)
        dist, model = Mock(), Mock()
        torch.distributed = dist
        model.to.return_value = model
        loaded = []

        def restore(*args, **kwargs):
            loaded.append(True)
            return optimizer

        def ddp(inner, **kwargs):
            self.assertEqual(loaded, [True], "DDP must broadcast restored parameters")
            self.assertIs(inner, model)
            return model

        torch.optim.SGD.side_effect = restore
        torch.nn.parallel.DistributedDataParallel.side_effect = ddp
        datasets, models = Mock(), Mock()
        datasets.CIFAR10.return_value = range(50000)
        models.resnet18.return_value = model
        modules = {"fluidcr": fluidcr, "numpy": Mock(), "torch": torch,
                   "torch.distributed": dist, "torch.nn": torch.nn,
                   "torch.nn.parallel": torch.nn.parallel, "torch.utils": torch.utils,
                   "torch.utils.data": torch.utils.data, "torchvision": Mock(),
                   "torchvision.datasets": datasets, "torchvision.models": models}
        with patch.dict(sys.modules, modules), patch.dict(train.os.environ, {
            "RANK": "0", "WORLD_SIZE": "2", "EVAL_RUN_ID": "test-run", "EVAL_GOAL_STEPS": "5",
        }), patch("sys.stdout", io.StringIO()), \
                patch.object(train, "train_updates", return_value=5), \
                patch.object(train, "hold_completed") as hold:
            train.main(["--device", "cpu"])
        hold.assert_called_once()
        self.assertEqual(hold.call_args.args[2], 5)
        self.assertEqual([call[0] for call in dist.method_calls], ["init_process_group", "barrier"])

    def fixture(self, start=2):
        optimizer = Mock(_fluidcr_step=start)
        optimizer.step.side_effect = lambda: setattr(optimizer, "_fluidcr_step", optimizer._fluidcr_step + 1)
        fluidcr = Mock()
        fluidcr.global_step.side_effect = lambda opt: opt._fluidcr_step
        torch = Mock()
        torch.nn.functional.cross_entropy.return_value.detach.return_value.item.return_value = 0.25
        return optimizer, fluidcr, torch

    def test_resumed_training_stops_at_goal_without_extra_update(self):
        optimizer, fluidcr, torch = self.fixture()
        events = Mock()
        step = train.train_updates(config(), fluidcr, torch, Mock(), optimizer,
                                   [(Mock(), Mock())] * 20, "cpu", events)
        self.assertEqual(step, 5)
        self.assertEqual(optimizer.step.call_count, 3)
        self.assertEqual([call.args[1] for call in events.emit.call_args_list], [3, 4, 5])
        self.assertEqual(events.emit.call_args.kwargs["samples_seen"], 20)

    def test_already_completed_does_not_touch_loader_or_optimizer(self):
        optimizer, fluidcr, torch = self.fixture(5)
        self.assertEqual(train.train_updates(config(), fluidcr, torch, Mock(), optimizer,
                                            None, "cpu", Mock()), 5)
        optimizer.step.assert_not_called()

    def test_missing_hook_and_invalid_restored_step_fail(self):
        with self.assertRaises(RuntimeError):
            train.resumed_step(Mock(), object(), 5)
        for step in (-1, 6):
            optimizer, fluidcr, _ = self.fixture(step)
            with self.assertRaises(RuntimeError):
                train.resumed_step(fluidcr, optimizer, 5)

    def test_missing_step_increment_fails(self):
        optimizer, fluidcr, torch = self.fixture()
        optimizer.step.side_effect = None
        with self.assertRaisesRegex(RuntimeError, "exactly one"):
            train.train_updates(config(), fluidcr, torch, Mock(), optimizer, [(Mock(), Mock())], "cpu", Mock())

    def test_nonfinite_loss_never_updates(self):
        optimizer, fluidcr, torch = self.fixture()
        torch.nn.functional.cross_entropy.return_value.detach.return_value.item.return_value = float("nan")
        with self.assertRaisesRegex(RuntimeError, "non-finite"):
            train.train_updates(config(), fluidcr, torch, Mock(), optimizer, [(Mock(), Mock())], "cpu", Mock())
        optimizer.step.assert_not_called()

    def test_short_data_stream_is_failure_not_completion(self):
        optimizer, fluidcr, torch = self.fixture()
        with self.assertRaisesRegex(RuntimeError, "before the goal"):
            train.train_updates(config(), fluidcr, torch, Mock(), optimizer, [], "cpu", Mock())

    def test_completion_parks_and_disarms_checkpoint_watchdog(self):
        fluidcr, events = Mock(), Mock()
        with patch.object(train.time, "sleep", side_effect=InterruptedError), patch.object(train.signal, "signal"):
            with self.assertRaises(InterruptedError):
                train.hold_completed(fluidcr, events, 5)
        fluidcr.cancel_checkpoint_watchdog.assert_called_once_with()
        events.emit.assert_called_once_with("completed", 5)

    def test_json_lines_are_utc_and_have_collector_keys(self):
        stream = io.StringIO()
        events = train.Events(config(), stream)
        for name in ("worker_started", "restored", "step", "completed"):
            events.emit(name, 3)
        for line in stream.getvalue().splitlines():
            self.assertTrue(line.startswith("EVAL_EVENT "))
            record = json.loads(line[len("EVAL_EVENT "):])
            self.assertEqual((record["run_id"], record["rank"], record["global_step"]), ("test-run", 0, 3))
            self.assertEqual(datetime.fromisoformat(record["timestamp"].replace("Z", "+00:00")).tzinfo, timezone.utc)
            self.assertEqual(record["attempt_id"], events.attempt_id)


if __name__ == "__main__":
    unittest.main()
