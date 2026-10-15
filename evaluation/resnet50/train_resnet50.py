"""ResNet50 CIFAR-10 entrypoint using the common FluidCR workload."""
import os
from pathlib import Path
import sys


def main(argv=None):
    sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "resnet18"))
    from train_resnet18 import main as train
    os.environ.setdefault("EVAL_MODEL", "resnet50")
    train(argv)


if __name__ == "__main__":
    main()
