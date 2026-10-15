"""Prepare offline WikiText parquet splits as deterministic packed token blocks."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import struct

RECIPE = "wikitext-row-eos-cross-shard-pack-drop-split-tail-v1"


def file_hash(path):
    digest = hashlib.sha256()
    with open(path, "rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def tokenizer_identity(root):
    root = Path(root)
    names = ("tokenizer.json", "tokenizer_config.json", "special_tokens_map.json",
             "vocab.json", "merges.txt", "added_tokens.json")
    return {name: file_hash(root / name) for name in names if (root / name).is_file()}


def discover_splits(root):
    root = Path(root)
    splits = {}
    for path in sorted(root.rglob("*.parquet")):
        relative = path.relative_to(root)
        matches = {split for split in ("train", "validation", "test")
                   if any(re.match(r"^" + split + r"(?:[-_.]|$)", part)
                          for part in relative.parts)}
        if len(matches) != 1:
            raise ValueError("cannot determine unique split for " + str(relative))
        splits.setdefault(matches.pop(), []).append(path)
    if "train" not in splits:
        raise ValueError("raw parquet directory must contain a train split")
    return splits


def packed_blocks(rows, tokenizer, sequence_length):
    """Carry tokens across every row/shard; drop only the final split remainder."""
    pending = []
    for text in rows:
        if not isinstance(text, str):
            raise ValueError("parquet text values must be strings")
        if not text.strip():
            continue
        pending.extend(tokenizer.encode(text, add_special_tokens=False))
        pending.append(tokenizer.eos_token_id)
        offset = 0
        while len(pending) - offset >= sequence_length:
            yield pending[offset:offset + sequence_length]
            offset += sequence_length
        pending = pending[offset:]


def parquet_rows(paths):
    import pyarrow.parquet as pq
    for path in paths:
        for batch in pq.ParquetFile(path).iter_batches(batch_size=1024, columns=["text"]):
            yield from batch.column(0).to_pylist()


def prepare(model_root, data_root, output, sequence_length=512):
    if not 2 <= sequence_length <= 1024:
        raise ValueError("sequence length must be in [2, 1024]")
    model_root, output = Path(model_root), Path(output)
    if not model_root.is_dir():
        raise ValueError("model root must be a local directory")
    if output.exists():
        raise FileExistsError("output already exists: " + str(output))
    splits = discover_splits(data_root)
    os.environ["HF_HUB_OFFLINE"] = "1"
    os.environ["HF_DATASETS_OFFLINE"] = "1"
    from transformers import AutoTokenizer
    tokenizer = AutoTokenizer.from_pretrained(str(model_root), local_files_only=True, use_fast=True)
    if tokenizer.eos_token_id is None:
        raise ValueError("tokenizer requires an EOS token")
    output.mkdir(parents=True, exist_ok=False)
    manifest = dict(format_version=1, recipe=RECIPE, dtype="uint32-le",
                    sequence_length=sequence_length, tokenizer=tokenizer_identity(model_root),
                    eos_token_id=tokenizer.eos_token_id, splits={})
    for split, paths in sorted(splits.items()):
        print("Preparing " + split + ": " + str(len(paths)) + " parquet shards", flush=True)
        target = output / (split + ".bin")
        blocks = 0
        with target.open("xb") as stream:
            for block in packed_blocks(parquet_rows(paths), tokenizer, sequence_length):
                stream.write(struct.pack("<" + "I" * sequence_length, *block))
                blocks += 1
        if split == "train" and blocks == 0:
            raise ValueError("train split contains no complete token blocks")
        manifest["splits"][split] = dict(file=target.name, blocks=blocks,
            sha256=file_hash(target), sources=[
                dict(file=str(p.relative_to(data_root)).replace("\\", "/"), sha256=file_hash(p))
                for p in paths])
        print("Completed " + split + ": " + str(blocks) + " blocks", flush=True)
    # Publish the completion marker last; incomplete outputs are never accepted.
    with (output / "manifest.json").open("x", encoding="utf-8") as stream:
        json.dump(manifest, stream, sort_keys=True, indent=2)
        stream.write("\n")
    print("Manifest: " + str(output / "manifest.json"), flush=True)
    return manifest


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--model-root", required=True)
    parser.add_argument("--data-root", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--sequence-length", type=int, default=512)
    args = parser.parse_args(argv)
    prepare(args.model_root, args.data_root, args.output, args.sequence_length)


if __name__ == "__main__":
    main()
