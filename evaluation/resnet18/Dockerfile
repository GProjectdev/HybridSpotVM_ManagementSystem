FROM pytorch/pytorch:2.4.1-cuda12.1-cudnn9-runtime

RUN python -m pip install --no-cache-dir --no-deps torchvision==0.19.1 \
    && python -c "import torch, torchvision; assert torch.__version__.split('+')[0] == '2.4.1'; assert torchvision.__version__.split('+')[0] == '0.19.1'"

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    CUBLAS_WORKSPACE_CONFIG=:4096:8 \
    FLUIDCR_DISTRIBUTED=1 \
    FLUIDCR_DISABLE_ENUMERATE_PATCH=1 \
    FLUIDCR_DISABLE_ITER_PATCH=1 \
    FLUIDCR_DISABLE_EPOCH_RANGE_PATCH=1 \
    EVAL_DATA_ROOT=/datasets/cifar10

WORKDIR /workspace
COPY train_resnet18.py /workspace/train_resnet18.py

# FluidCR's webhook wraps the explicit Pod command with its injected launcher.
CMD ["python", "-u", "/workspace/train_resnet18.py"]
