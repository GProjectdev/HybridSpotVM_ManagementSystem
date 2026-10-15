# ResNet-50 / GPT-2 small 실행 가이드
작성일: 2026-10-15

## 먼저 확인
이 문서는 환경 이미지가 아닌 **실제 학습 이미지**를 빌드하는 순서다.
이전 `resnet50_env_v1` / `gpt2_small_env_v1` 명령은 사용하지 않는다.
ResNet-18을 포함한 기존 실행은 기본값으로 유지한다. 컨트롤러 이미지는 이번 변경으로 교체하지 않는다.
새 모델의 실제 A10G DDP/FluidCR 복원은 아래 짧은 검증에서 확인해야 한다. 로컬 테스트 통과를 클라우드 검증 완료로 해석하지 않는다.

| 위치 | 할 작업 |
|---|---|
| MGMT: /root/hybridspot-validation/System | 코드 갱신, 이미지 Buildah 빌드/Push, 설정, 평가 실행 |
| NFS 서버: 10.50.1.31 | 이미 받은 GPT-2/WikiText 확인, GPT-2 토큰화 |
| AWS Master | 필요할 때 Pod/Node 상태 조회 |
| GPU Worker | 직접 설치할 것 없음. Pod가 이미지와 NFS 마운트 사용 |

한 번에 하나의 평가만 실행한다. 이전 run cleanup 후 실제 EC2 종료를 확인한다.
기존 NFS 원본 데이터나 이전 evidence를 삭제하지 않는다.

## 1. MGMT: 코드 갱신 및 테스트
```bash
set -euo pipefail
export ROOT=/root/hybridspot-validation
cd "$ROOT/System"
git status --short
git pull --ff-only origin restore-automation-20260928
python3 -B -m unittest discover -s evaluation -p 'test_*.py'
python3 -B -m unittest discover -s evaluation/resnet18 -p 'test_*.py'
```
로컬 변경 때문에 pull이 거부되면 중단하고 변경 내용을 확인한다. reset/clean으로 설정을 지우지 않는다.
`evaluation/policy-source.json`은 이전에 검증한 AWS 설정 파일을 그대로 사용한다.
기존 TrainingPolicy/StatefulSet을 미리 만들지 않는다.

## 2. MGMT: 최종 학습 이미지 빌드/Push
아래 두 빌드의 context는 `evaluation`이다. 이전 resnet18 전용 context와 다르다.
```bash
set -euo pipefail
cd "$ROOT/System"
buildah login docker.io
export R50_IMAGE=docker.io/jeongseungjun/hybrid-spot-vm-system:resnet50_eval_v1
export GPT2_IMAGE=docker.io/jeongseungjun/hybrid-spot-vm-system:gpt2_small_eval_v1

buildah bud --arch amd64 -f evaluation/resnet50/Dockerfile -t "$R50_IMAGE" evaluation
buildah push "$R50_IMAGE" "docker://$R50_IMAGE"

buildah bud --arch amd64 -f evaluation/gpt2/Dockerfile -t "$GPT2_IMAGE" evaluation
buildah push "$GPT2_IMAGE" "docker://$GPT2_IMAGE"
```
각 명령이 성공해야 다음 단계로 간다. 실패한 빌드를 무시하고 push하지 않는다.
다시 빌드해 내용이 달라지면 새 태그를 사용하고, 같은 모델 A/B는 동일 이미지로 실행한다.
빌드 머신에는 GPU가 없어도 되지만 인터넷과 레지스트리 권한이 필요하다.

## 3. MGMT에서 전처리 코드를 NFS 서버로 전달
NFS SSH 계정을 환경변수로 직접 지정한다. 기존에 root로 접속했다면 root를 사용한다.
```bash
export NFS_SSH_USER=root
scp evaluation/gpt2/prepare_data.py "${NFS_SSH_USER}@10.50.1.31:/tmp/prepare_gpt2_data.py"
```

## 4. NFS 서버: GPT-2 데이터 전처리
이하 블록은 **10.50.1.31에 접속한 터미널**에서 실행한다. GPU/PyTorch는 필요 없다.
ResNet-50은 기존 CIFAR-10을 그대로 사용하므로 이 단계가 필요 없다.
```bash
python3 -m venv "$HOME/gpt2-preprocess-env"
source "$HOME/gpt2-preprocess-env/bin/activate"
python -m pip install "numpy==1.26.4" "pyarrow==17.0.0" "transformers==4.44.2" "datasets==2.21.0" "huggingface-hub==0.24.7"
export HF_HUB_OFFLINE=1 HF_DATASETS_OFFLINE=1 TRANSFORMERS_OFFLINE=1
export TOKENIZERS_PARALLELISM=false
export MODEL_ROOT=/mnt/nfs/models/gpt2-small/607a30d783dfa663caf39e06633721c8d4cfcd7e
export RAW_ROOT=/mnt/nfs/datasets/wikitext103/b08601e04326c79dfdd32d625aee71d232d685c3/wikitext-103-raw-v1
export TOKEN_ROOT=/mnt/nfs/datasets/wikitext103-tokenized/gpt2-512-v1

python /tmp/prepare_gpt2_data.py --model-root "$MODEL_ROOT" --data-root "$RAW_ROOT" \
  --output "$TOKEN_ROOT" --sequence-length 512
```
Ubuntu Python 3.10/3.11 환경을 권장한다. 다운로드 때의 가상환경과 별도로 만든다.
처리에는 시간이 걸리며 디스크 여유를 확인한다. 결과 디렉터리를 학습 중 수정하지 않는다.
기존 output이 있으면 덮어쓰지 않도록 거부한다. 실패한 부분 결과는 확인 후 새 output 이름으로 재실행한다.
원본 train/validation/test를 섞지 않는다. 토큰화는 측정 전에 수행한다.
전처리는 빈 행을 제외하고 각 행 뒤에 EOS를 붙인 후 split 안에서 이어 붙여 고정 길이로 나눈다.
split 마지막의 부족한 토큰은 버린다. 이 데이터 처리 방식은 manifest에 기록되며 A/B에 동일하게 적용된다.
완료되면 output에 manifest.json과 train.bin/validation.bin/test.bin이 생긴다.
위 모델/데이터와 다른 공개 벤치마크의 perplexity 수치를 직접 비교하는 용도가 아니다.
실제 학습은 train만 읽으며, validation/test 생성 자체가 품질 평가 수행을 뜻하지는 않는다.

## 5. MGMT: 모델별 로컬 설정 생성
다시 **MGMT 터미널**로 돌아온다.
```bash
cd "$ROOT/System"
test -f evaluation/policy-source.json
cp -n evaluation/config.resnet50.example.json evaluation/config.resnet50.local.json
cp -n evaluation/config.gpt2.example.json evaluation/config.gpt2.local.json
```
이미 로컬 파일이 있으면 복사하지 말고 기존 파일을 확인한다.
각 설정은 NFS IP/다운로드 revision을 이번 실험 경로로 채워 두었다.
전처리 output을 바꿨으면 GPT-2의 `nfs_dataset_path`도 동일하게 바꾼다.
GPT-2 `nfs_model_path`는 원본 모델 디렉터리, `nfs_dataset_path`는 **원본 parquet이 아니라 전처리 output**이다.

기본 구성:
- ResNet-50: CIFAR-10, 무작위 초기화, CIFAR stem, GPU당 batch 32, SGD lr 0.1.
- GPT-2 small: 전체 파라미터 미세조정, GPU당 batch 4, sequence 512, FP32, AdamW lr 5e-5.
- optimizer weight decay는 두 모델 모두 5e-4이며, SGD momentum은 0.9다.
- 둘 다 GPU 2개 DDP, accumulation 1, scheduler 없음.
- 예제 10,000 step은 시작값일 뿐, 소요 시간이나 복원 완료를 보장하는 값이 아니다.
- 예제 위험률 변경은 학습 관측 시작 후 180초이며 **정식 1시간 실험 설정이 아니다.**

## 6. MGMT: 먼저 짧은 A 실행으로 속도 확인
아래는 ResNet-50 예시다. GPT-2는 CONFIG와 RUN만 아래 설명대로 바꾼다.
```bash
export CONFIG=evaluation/config.resnet50.local.json
export RUN=r50pilot01
export PILOT_CONFIG="${CONFIG%.local.json}.pilot.local.json"
jq '.goal_steps = 500 | .scenario = "constant"' "$CONFIG" > "$PILOT_CONFIG"
python3 evaluation/cost.py render --config "$PILOT_CONFIG" --run-id "$RUN" --arm A --out "evidence/cost/$RUN"
jq '{run_id,model,dataset,goal_steps,batch_size,sequence_length,training_image}' "evidence/cost/$RUN/run.json"
python3 evaluation/cost.py prepare --evidence "evidence/cost/$RUN"
python3 evaluation/cost.py start --evidence "evidence/cost/$RUN"
```
GPT-2에서는 `CONFIG=evaluation/config.gpt2.local.json`, `RUN=gptpilot01`로 사용한다.
run-id와 output은 매번 새로 만든다. 디렉터리 복사나 run.json 수동 phase 변경으로 재시작하지 않는다.
위 명령은 원본 CONFIG를 유지하고 별도 pilot 파일만 500 step으로 만든다.
500 step은 실행 확인용이며, 정식 목표 step 산정에는 더 긴 안정 구간 측정이 필요하다.
이미 render한 파일에는 원본 config 수정이 반영되지 않는다.

## 7. MGMT: 결과 확인/정리
다른 MGMT 터미널에서 ROOT/RUN을 동일한 값으로 설정하고 조회한다.
```bash
tail -n 20 -F "$ROOT/System/evidence/cost/$RUN/runner-events.jsonl"
```
`all_ranks_completed`와 run.json의 `phase=completed`를 확인한다.
```bash
jq '{run_id,model,phase,failure_reason}' "evidence/cost/$RUN/run.json"
python3 evaluation/cost.py cleanup --evidence "evidence/cost/$RUN" --confirm "$RUN"
```
cleanup은 VM 삭제 요청이며 실제 EC2 종료 확인까지 필요하다. PVC/PV는 보존된다.
실패한 실행은 로그를 먼저 보존한 뒤 `abort --evidence ... --confirm ...` 후 cleanup한다.
finalizer 제거/강제 성공 처리로 통과시키지 않는다.

## 8. MGMT: 3분 위험률 변경으로 B 복원 검증
A 파일럿 속도로 **두 번의 순차 복원 이후에도 학습이 계속될 충분한 step**을 설정한다.
VM 준비는 병렬이지만 Checkpoint/Restore는 순차다. 30분으로 반드시 끝난다고 가정하지 않는다.
위 A smoke용 PILOT_CONFIG가 아니라 원본 CONFIG를 사용한다.
예제 goal_steps=10000도 모델별 실측 시간에 따라 조정한다.
CONFIG에서 scenario=risk-schedule, risk_schedule의 after_seconds=180을 확인한다.
```bash
export RUN=r50restore01
python3 evaluation/cost.py render --config "$CONFIG" --run-id "$RUN" --arm B --out "evidence/cost/$RUN"
python3 evaluation/cost.py prepare --evidence "evidence/cost/$RUN"
python3 evaluation/cost.py start --evidence "evidence/cost/$RUN"
```
GPT-2에서는 해당 CONFIG와 `RUN=gptrestore01`를 사용한다.
필수 확인: 두 RestoreRequest Verified, 두 SpotReplacement/SpotRecovery Completed,
두 rank의 복원 후 step 증가, 목표 step 완료, run phase completed.
Pod Running만으로 합격으로 판단하지 않는다.

## 9. 정식 실험으로 변경
짧은 검증 성공 후 모델별 goal_steps를 A 파일럿의 안정 구간 처리율로 결정한다.
ResNet-18의 43,000 step을 그대로 복사하지 않는다.
동일 모델의 A/B에는 같은 goal_steps/batch/seed/precision/학습 코드/이미지를 사용한다.
정식 B 설정:
```json
"scenario": "risk-schedule",
"risk_schedule": [{"after_seconds": 3600, "lambda_per_hour": 0.8}]
```
risk_change_after_seconds도 혼동 방지를 위해 3600으로 맞춘다.
A는 렌더러가 constant + 정기 Checkpoint disabled로 변환한다.
B는 계산된 주기(60~3600초)를 사용한다. 위험률 변경은 실제 VM 강제 선점과 다른 시나리오다.
새 run-id로 A -> cleanup/EC2 종료 -> B 순서로 수행한다.
B 추가 중단 때문에 오래 걸리는 것을 숨기려고 A/B step을 다르게 바꾸지 않는다.
각 모델의 효과는 해당 모델 A/B끼리 비교한다.

## 10. 보존할 자료와 한계
evidence 전체, config, 이미지 digest, 모델/data manifest와 revision을 함께 보존한다.
원본 NFS 경로 내용은 변경하지 않는다. 같은 태그에 다른 이미지를 push한 경우 새 버전으로 기록한다.
고정 step 평가는 학습 시스템의 시간/비용 평가이며 동일 정확도/수렴 달성 주장을 대신하지 않는다.
이번 코드는 자동 EC2 즉시 종료를 추가하지 않았다. 평가 후 cleanup은 여전히 필요하다.
GPU 메모리/OOM, 실제 Cloud DDP 복원은 로컬 단위 테스트로 보장되지 않는다.
OOM이면 batch를 줄여 새 파일럿부터 수행하고 같은 모델 A/B 설정을 모두 맞춘다.

## 구현 근거
- https://huggingface.co/docs/transformers/v4.44.2/en/main_classes/model
- https://huggingface.co/docs/transformers/v4.44.2/en/model_doc/gpt2
- https://huggingface.co/docs/datasets/v2.21.0/loading
