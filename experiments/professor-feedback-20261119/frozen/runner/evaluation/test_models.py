"""CPU-only contracts; real tiny-model checks skip when ML dependencies are absent."""
import ast
import dataclasses
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import struct
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch
import weakref

sys.path.insert(0, str(Path(__file__).parent / "resnet18"))
sys.path.insert(0, str(Path(__file__).parent / "gpt2"))
import train_resnet18 as common
import train_gpt2 as gpt
import prepare_data as prep


def config(**env):
    return common.parse_config([], dict(EVAL_RUN_ID="models-test", RANK="0",
        WORLD_SIZE="1", EVAL_BATCH_SIZE="2", EVAL_GOAL_STEPS="4", **env))


class ModelContracts(unittest.TestCase):
    def test_defaults_and_overrides(self):
        for model, lr, root in (("resnet18", .1, "/datasets/cifar10"),
                                ("resnet50", .1, "/datasets/cifar10"),
                                ("gpt2", 5e-5, "/datasets/wikitext103-tokenized")):
            cfg = config(EVAL_MODEL=model)
            self.assertEqual((cfg.learning_rate, cfg.data_root), (lr, root))
        self.assertEqual(config(EVAL_LEARNING_RATE=".002").learning_rate, .002)
        self.assertEqual(config(EVAL_MODEL="gpt2", EVAL_SEQUENCE_LENGTH="2").sequence_length, 2)
        for env in (dict(EVAL_MODEL="bad"), dict(EVAL_SEQUENCE_LENGTH="1"),
                    dict(EVAL_SEQUENCE_LENGTH="1025"), dict(EVAL_LEARNING_RATE="nan")):
            with self.subTest(env=env), patch("sys.stderr", io.StringIO()), self.assertRaises(SystemExit):
                config(**env)

    def test_original_resnet18_digest_unchanged(self):
        cfg = config()
        old = dataclasses.asdict(cfg)
        for name in ("rank", "local_rank", "data_root", "model", "model_root", "sequence_length"):
            old.pop(name)
        old.update(dataset="cifar10-train", dataset_size=50000,
                   model="resnet18-cifar-stem", data_recipe="step-shuffle-crop-flip-v1")
        expected = hashlib.sha256(json.dumps(old, sort_keys=True).encode()).digest()
        self.assertEqual(common.contract_digest(cfg, 50000), expected)
        self.assertNotEqual(common.contract_digest(dataclasses.replace(cfg, model="resnet50"), 50000), expected)

    def test_gpt_step_loss_and_counters(self):
        cfg = config(EVAL_MODEL="gpt2", EVAL_SEQUENCE_LENGTH="8")
        optimizer, backend, torch, model, events = Mock(_fluidcr_step=2), Mock(), Mock(), Mock(), Mock()
        backend.global_step.side_effect = lambda opt: opt._fluidcr_step
        optimizer.step.side_effect = lambda: setattr(optimizer, "_fluidcr_step", optimizer._fluidcr_step + 1)
        model.return_value.loss.detach.return_value.item.return_value = 1.25
        batch = (Mock(), Mock())
        result = common.train_updates(cfg, backend, torch, model, optimizer, [batch] * 5, "cpu", events)
        self.assertEqual(result, 4)
        self.assertEqual(optimizer.step.call_count, 2)
        fields = events.emit.call_args.kwargs
        self.assertEqual((fields["samples_seen"], fields["tokens_seen"], fields["loss_tokens_seen"]), (8, 64, 56))
        self.assertFalse(model.call_args.kwargs["use_cache"])
        self.assertIs(model.call_args.kwargs["labels"], batch[1].to.return_value)
        torch.nn.functional.cross_entropy.assert_not_called()

    def test_gpt_contract_binds_content_but_not_mounts(self):
        cfg = config(EVAL_MODEL="gpt2")
        dataset = Mock(manifest={"train": "hash1"}, model_files={"weights": "hash2"})
        digest = gpt.workload_digest(cfg, dataset)
        self.assertEqual(digest, gpt.workload_digest(dataclasses.replace(cfg,
            rank=9, local_rank=9, data_root="/other", model_root="/other"), dataset))
        dataset.model_files = {"weights": "different"}
        self.assertNotEqual(digest, gpt.workload_digest(cfg, dataset))

    def test_sampler_resume_is_exact_suffix(self):
        full = list(common.StepBatchSampler(35, 2, 2, 1, 42, 0, 20))
        for start in range(21):
            self.assertEqual(list(common.StepBatchSampler(35, 2, 2, 1, 42, start, 20)), full[start:])


class PreprocessingTests(unittest.TestCase):
    def tokenizer(self):
        tok = Mock(eos_token_id=99)
        tok.encode.side_effect = lambda text, **kw: [int(n) for n in text.split()]
        return tok

    def test_cross_row_packing_and_eos_are_preserved(self):
        tok = self.tokenizer()
        self.assertEqual(list(prep.packed_blocks(["1 2", "", "3 4", "5"], tok, 4)),
                         [[1, 2, 99, 3], [4, 99, 5, 99]])
        self.assertTrue(all(call.kwargs == {"add_special_tokens": False} for call in tok.encode.call_args_list))

    def test_sorted_shards_and_split_preservation(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            for name in ("train-002.parquet", "test-000.parquet", "train-001.parquet", "validation.parquet"):
                (root / name).touch()
            splits = prep.discover_splits(root)
            self.assertEqual([p.name for p in splits["train"]], ["train-001.parquet", "train-002.parquet"])
            self.assertEqual(set(splits), {"train", "test", "validation"})

    def test_prepare_manifest_no_overwrite_and_train_only_reader(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            model, raw, output = root / "model", root / "raw", root / "out"
            model.mkdir()
            raw.mkdir()
            (model / "tokenizer.json").write_text("{}")
            for split in ("train", "validation", "test"):
                (raw / (split + ".parquet")).touch()
            transformers = Mock()
            transformers.AutoTokenizer.from_pretrained.return_value = self.tokenizer()
            with patch.dict(sys.modules, transformers=transformers), patch.object(prep, "parquet_rows",
                    side_effect=lambda paths: iter(["1 2", "3 4", "5"])), patch("sys.stdout", io.StringIO()):
                manifest = prep.prepare(model, raw, output, 4)
                with self.assertRaises(FileExistsError):
                    prep.prepare(model, raw, output, 4)
            transformers.AutoTokenizer.from_pretrained.assert_called_once_with(str(model),
                local_files_only=True, use_fast=True)
            self.assertEqual(set(manifest["splits"]), {"train", "validation", "test"})
            self.assertEqual(manifest["splits"]["train"]["blocks"], 2)
            self.assertEqual((output / "train.bin").read_bytes(), struct.pack("<8I", 1, 2, 99, 3, 4, 99, 5, 99))
            if importlib.util.find_spec("numpy") is None:
                self.skipTest("numpy unavailable for memory-map reader")
            data = gpt.TokenBlocks(output, 4)
            self.assertEqual(len(data), 2)
            self.assertEqual(data.tokens[0].tolist(), [1, 2, 99, 3])
            del data
            with self.assertRaises(ValueError):
                gpt.TokenBlocks(output, 8)
            (output / "train.bin").write_bytes(bytes(32))
            with self.assertRaisesRegex(ValueError, "checksum"):
                gpt.TokenBlocks(output, 4)

    def test_local_loading_options_and_tied_parameters(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / "config.json").write_text("{}")
            (root / "model.safetensors").write_bytes(b"weights")
            dataset = Mock(manifest={"tokenizer": {}})
            model = Mock()
            model.config.n_positions = 1024
            shared = object()
            model.get_input_embeddings.return_value.weight = shared
            model.get_output_embeddings.return_value.weight = shared
            hf, torch = Mock(), Mock()
            hf.GPT2LMHeadModel.from_pretrained.return_value = model
            with patch.dict(sys.modules, transformers=hf, torch=torch), patch.object(gpt, "TokenBlocks", return_value=dataset):
                actual, _ = gpt.load_workload(dataclasses.replace(config(EVAL_MODEL="gpt2"), model_root=str(root)))
            self.assertIs(actual, model)
            hf.GPT2LMHeadModel.from_pretrained.assert_called_once_with(str(root),
                local_files_only=True, torch_dtype=torch.float32,
                attn_implementation="eager", low_cpu_mem_usage=False)
            model.tie_weights.assert_called_once()
            model.requires_grad_.assert_called_once_with(True)


class FluidCRRootTests(unittest.TestCase):
    def test_existing_backend_deduplicates_shared_root_and_filters_children(self):
        backend = Path(__file__).resolve().parents[2] / "My_FluidCR-work/fluidcr/backends/pytorch.py"
        if not backend.exists():
            backend = Path(__file__).resolve().parents[2] / "fluidcr/fluidcr/backends/pytorch.py"
        if not backend.exists():
            self.skipTest("sibling FluidCR backend unavailable")
        names = {"_live_refs", "_unwrap_model", "_has_parameters", "_root_models"}
        tree = ast.parse(backend.read_text(encoding="utf-8"))
        tree.body = [node for node in tree.body if isinstance(node, ast.FunctionDef) and node.name in names]
        scope = {"Any": object, "List": list, "Set": set, "weakref": weakref}
        exec(compile(tree, str(backend), "exec"), scope)
        class Module:
            def __init__(self, *children):
                self.children = children
            def modules(self):
                yield self
                for child in self.children:
                    yield from child.modules()
            def parameters(self):
                yield object()
        embedding, head = Module(), Module()
        model = Module(embedding, head)
        scope["_tracked_models"] = [weakref.ref(m) for m in (embedding, head, model, model)]
        self.assertEqual(scope["_root_models"](), [model])


@unittest.skipUnless(importlib.util.find_spec("torch") and importlib.util.find_spec("transformers"),
                     "torch/transformers unavailable; real tiny GPT-2 CPU check skipped")
class RealModelTests(unittest.TestCase):
    def test_tiny_gpt2_full_parameter_update_and_tie(self):
        import torch
        from transformers import GPT2Config, GPT2LMHeadModel
        model = GPT2LMHeadModel(GPT2Config(vocab_size=32, n_positions=8, n_embd=8, n_layer=1, n_head=1,
                                          attn_pdrop=0, resid_pdrop=0, embd_pdrop=0))
        self.assertIs(model.transformer.wte.weight, model.lm_head.weight)
        self.assertTrue(all(p.requires_grad for p in model.parameters()))
        optimizer = torch.optim.AdamW(model.parameters(), lr=5e-5)
        inputs = torch.tensor([[1, 2, 3, 4]])
        before = model.transformer.wte.weight.detach().clone()
        model(input_ids=inputs, labels=inputs, use_cache=False).loss.backward()
        optimizer.step()
        self.assertFalse(torch.equal(before, model.transformer.wte.weight))
        self.assertIs(model.transformer.wte.weight, model.lm_head.weight)


if __name__ == "__main__":
    unittest.main()
