"""Offline full-parameter GPT-2 workload; shares FluidCR step/event semantics."""
from dataclasses import asdict
import hashlib
import json
import os
from pathlib import Path
import sys

try:
    from .prepare_data import RECIPE, file_hash, tokenizer_identity
except ImportError:
    from prepare_data import RECIPE, file_hash, tokenizer_identity


class TokenBlocks:
    def __init__(self, root, sequence_length):
        import numpy as np
        root = Path(root)
        self.manifest = json.loads((root / "manifest.json").read_text(encoding="utf-8"))
        manifest = self.manifest
        if (manifest.get("format_version") != 1 or manifest.get("recipe") != RECIPE
                or manifest.get("dtype") != "uint32-le"
                or manifest.get("sequence_length") != sequence_length):
            raise ValueError("prepared dataset format/sequence length mismatch")
        split = manifest["splits"]["train"]
        if split["file"] != "train.bin" or type(split["blocks"]) is not int or split["blocks"] < 1:
            raise ValueError("invalid prepared train split")
        path = root / "train.bin"
        if path.stat().st_size != split["blocks"] * sequence_length * 4:
            raise ValueError("prepared train byte count mismatch")
        if file_hash(path) != split["sha256"]:
            raise ValueError("prepared train checksum mismatch")
        self.tokens = np.memmap(path, dtype="<u4", mode="r",
                                shape=(split["blocks"], sequence_length))

    def __len__(self):
        return len(self.tokens)

    def __getitem__(self, key):
        import torch
        _, index = key
        tokens = torch.tensor(self.tokens[index].astype("int64"), dtype=torch.long)
        return tokens, tokens.clone()


def model_identity(root):
    root = Path(root)
    names = [p for p in root.iterdir() if p.is_file() and
             (p.name == "config.json" or p.suffix in (".safetensors", ".bin")
              or p.name.endswith(".index.json"))]
    if not any(p.suffix in (".safetensors", ".bin") for p in names):
        raise ValueError("model root contains no local weights")
    return {p.name: file_hash(p) for p in sorted(names)}


def load_workload(config):
    import torch
    os.environ["HF_HUB_OFFLINE"] = "1"
    os.environ["HF_DATASETS_OFFLINE"] = "1"
    from transformers import GPT2LMHeadModel
    root = Path(config.model_root)
    if not root.is_dir():
        raise ValueError("model root must be a local directory")
    dataset = TokenBlocks(config.data_root, config.sequence_length)
    if dataset.manifest["tokenizer"] != tokenizer_identity(root):
        raise ValueError("prepared tokenizer does not match model tokenizer")
    dataset.model_files = model_identity(root)
    # Avoid meta-device loading: FluidCR must observe ordinary module construction.
    model = GPT2LMHeadModel.from_pretrained(str(root), local_files_only=True,
        torch_dtype=torch.float32, attn_implementation="eager", low_cpu_mem_usage=False)
    model.config.use_cache = False
    model.tie_weights()
    model.requires_grad_(True)
    if model.config.n_positions < config.sequence_length:
        raise ValueError("model context is shorter than prepared blocks")
    if model.get_input_embeddings().weight is not model.get_output_embeddings().weight:
        raise RuntimeError("GPT-2 input/output weights must remain tied")
    return model, dataset


def workload_digest(config, dataset):
    contract = asdict(config)
    for key in ("rank", "local_rank", "data_root", "model_root"):
        contract.pop(key)
    contract.update(dataset=dataset.manifest, model_files=dataset.model_files,
                    optimizer="AdamW", attention="eager", dtype="float32")
    return hashlib.sha256(json.dumps(contract, sort_keys=True).encode()).digest()


def main(argv=None):
    sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "resnet18"))
    from train_resnet18 import main as train
    os.environ.setdefault("EVAL_MODEL", "gpt2")
    train(argv)


if __name__ == "__main__":
    main()
