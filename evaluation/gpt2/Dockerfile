# Build from the evaluation directory.
FROM pytorch/pytorch:2.4.1-cuda12.1-cudnn9-runtime
RUN python -m pip install --no-cache-dir numpy==1.26.4 pyarrow==17.0.0 transformers==4.44.2 datasets==2.21.0 huggingface-hub==0.24.7 \
    && python -c "import torch, transformers, datasets, huggingface_hub; assert torch.__version__.split('+')[0] == '2.4.1'; assert transformers.__version__ == '4.44.2'; assert datasets.__version__ == '2.21.0'; assert huggingface_hub.__version__ == '0.24.7'"
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 CUBLAS_WORKSPACE_CONFIG=:4096:8 \
    FLUIDCR_DISTRIBUTED=1 FLUIDCR_DISABLE_ENUMERATE_PATCH=1 FLUIDCR_DISABLE_ITER_PATCH=1 \
    FLUIDCR_DISABLE_EPOCH_RANGE_PATCH=1 HF_HUB_OFFLINE=1 HF_DATASETS_OFFLINE=1 \
    TRANSFORMERS_OFFLINE=1 EVAL_MODEL=gpt2 EVAL_MODEL_ROOT=/models/gpt2 \
    EVAL_DATA_ROOT=/datasets/wikitext103-tokenized EVAL_SEQUENCE_LENGTH=512
WORKDIR /workspace
RUN python -c "import torch; from transformers import GPT2Config, GPT2LMHeadModel; torch.set_num_threads(1); c=GPT2Config(vocab_size=32,n_positions=8,n_embd=8,n_layer=1,n_head=1); c._attn_implementation='eager'; m=GPT2LMHeadModel(c); assert m.transformer.wte.weight is m.lm_head.weight; assert all(p.requires_grad for p in m.parameters()); o=torch.optim.AdamW(m.parameters(),lr=5e-5); x=torch.tensor([[1,2,3,4]]); before=m.transformer.wte.weight.detach().clone(); loss=m(input_ids=x,labels=x,use_cache=False).loss; assert torch.isfinite(loss); loss.backward(); o.step(); assert not torch.equal(before,m.transformer.wte.weight); assert m.transformer.wte.weight is m.lm_head.weight"
COPY resnet18/train_resnet18.py gpt2/train_gpt2.py gpt2/prepare_data.py /workspace/
CMD ["python", "-u", "/workspace/train_resnet18.py"]
