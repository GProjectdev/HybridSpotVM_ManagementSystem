# E1·E2 데이터 준비 — 2026-11-17 추가 요구 반영

실험 종료 2026-11-27. 데이터 준비 목표 11-18. 오늘 작업 마감은 22:00 KST.
원본 평가계획 PDF/MD는 보존한다. GPT-2 Small과 ResNet-50 각각에서
E1-A/E1-B/E2-A/E2-B를 수행하고, 같은 모델·설정·데이터·정책·이미지의
검토된 무선점 결과만 E2 baseline으로 재사용한다.
GPU VM 생성/삭제, FIS 실행, 활성 정책 변경은 이 문서의 준비 작업에 포함하지 않는다.

## 학습 데이터와 실행 계약

| 항목 | GPT-2 Small | ResNet-50 |
|---|---|---|
| 데이터 | 기존 전체 WikiText-103 | CIFAR-10 Python 공식 배포본 |
| NFS 경로 | `/mnt/nfs/validation-assets-v6-20261116/datasets/wikitext103-tokenized-gpt2-512-v1` | `/mnt/nfs/validation-assets-v6-20261117/datasets/cifar10-v1` |
| 모델 원본 | `/mnt/nfs/validation-assets-v6-20261116/models/gpt2-small` | torchvision ResNet-50, `weights=None`, CIFAR stem/10 classes |
| 버전 | dataset revision `b08601e04326c79dfdd32d625aee71d232d685c3`, model revision `607a30d783dfa663caf39e06633721c8d4cfcd7e` | Python archive MD5 `c58f30108f718f92721af3b95e74349a`; 개별 파일 SHA-256 manifest |
| 데이터 양 | train 232588 / validation 487 / test 558 token blocks | train 50000 / test 10000 images; 정상 학습은 train 사용 |
| 전처리 | row EOS, shard 경계 이어붙임, 512-token block, split tail drop, uint32 little endian | 4-pixel pad, 32x32 crop, horizontal flip; CIFAR mean/std 정규화 |
| per-rank batch / seed | 4 / 42, 기존 g5 파일럿과 동일할 때만 재사용 | 32 / 42는 준비 후보. GPU 메모리/시간 파일럿 후 고정 |
| optimizer | AdamW, lr 5e-5, weight decay 5e-4 | SGD, lr .1, momentum .9, weight decay 5e-4 |
| 진행량 단위 | tokens / tokens per second | samples / samples per second |
| 목표 step | 4747은 기존 GPT-2/g5 파일럿 기반 잠정값 | 미정. GPT-2 목표값·중간 FIS step을 복사하지 않음 |

두 모델은 기존 `StepBatchSampler`를 공유한다. Seed와 epoch로 순열을 재생성하고,
rank별 서로 겹치지 않는 full batch를 사용하며 global tail을 버린다.
복원된 optimizer global step에서 sampler를 바로 시작한다. Crop/flip은
(seed, epoch, sample index)에 묶고 loader RNG는 model RNG와 분리한다.
FluidCR는 rank별 모델·optimizer·RNG·step을 저장/복원한다.
데이터 경로/manifest SHA-256/이미지 digest/코드 SHA-256/모델/학습 설정을
run별 고정하며 실행 중 download=False와 read-only 데이터 mount를 사용한다.

ResNet-50의 일반 DDP BatchNorm 버퍼는 rank별로 달라질 수 있다.
최종 rank 간 검사는 parameters와 optimizer 일치를 확인하고, buffers 해시는
각 rank별 별도 보존한다. 복원 직후에는 각 rank의 전체 saved state와 비교한다.
CPU 통과를 CUDA/NCCL/CRI-O/CRIU/컨트롤러/NFS 복구 통과로 표시하지 않는다.

## 위험률 이력 및 공개 입력

현재 Collector CR은 static 입력이며 status는 매 poll 덮어쓴다.
`observedAt`은 처리 시각이지 AWS 관측 시각이 아니다. 오늘 회수한 현황은
`/home/ubuntu/v6-readiness-20261117/data-preparation/risk-history-inventory.json`에 기록한다.
검색 범위에서 연속적인 실제 AWS 선점/at-risk exposure 이력은 확보되지 않았다.

기존 SkyPilot 자료를 재사용할 수 있다:

- 출처: https://github.com/skypilot-org/spot-traces
- 파일: `availability/1-node/aws-02-15-2023/us-east-2a_v100_1.json`
- 로컬: `/home/ubuntu/v6-readiness-20261117/trace/us-east-2a_v100_1.json`
- SHA-256: `568de3507c66707eaa222fdcf26ab98ee8032959dc72bf3c5c456722be912562`
- 20158 samples, 195초 간격. 상대시계 가정은 원본의 실제 시각 정보가 아니다.
- 기존 Desai 기반 경로: availability 감소량을 완성된 시간 구간으로 집계 →
  causal 3시간 평균 → 기존 SARIMA(1,1,1)(1,0,1)[24], 과거 4주 →
  예측 집계량 / 명시한 모집단으로 근사 lambda. 기존 672시간 fit/168시간
  held-out 결과와 replay manifest를 우선 재사용하고 모델은 재개발하지 않는다.
- 모집단 1은 한 개 가용 슬롯이라는 proxy 가정이다. 실제 실행 중 VM의
  at-risk population이 아니며 Seoul/g5의 실측 위험률/예측 정확도가 아니다.
- SkyPilot에는 별도 preemption trace도 있다. 현재 feed에 연결된 파일은
  availability이며 둘을 혼용하지 않는다. 짧은 preemption 자료를 4주 이력으로
  채우거나 결측을 0으로 보충하지 않는다.
- Collector의 `trace` 단순 감소량/exposure 방식과 `paperEstimator`의 외부
  SARIMA feed 경로는 다르다. Desai 기반 입력 검토는 후자를 기준으로 한다.
- 현재 final E1/E2 필수 조건은 외부 trace가 아니다. 기본 static .05와
  외부 replay 중 실제 사용할 입력은 실행 전 고정하고 동일 Hybrid pair에서
  같게 적용한다. 준비 과정에서는 활성 CR을 변경하지 않는다.

기존 `trace/source_manifest.json`의 초기 blocker 문구는 당시 상태다.
후속 잠금 정보는 `experiments/ieee-access-v2/config/risk_replay_manifest.json`이
우선이며 실제 endpoint/TLS/Run 시계 연결이 완료되었다는 의미는 아니다.

## 가격: 실행 직전에 다시 수집

각 Run의 실제 instance type을 먼저 고정한다. 아래는 g5 예시이며 기본 계정을
사용하지 않는다. 공개 OD 원본은 Linux/Shared/NA/Used/RunInstances 및 정확한
APN2-BoxUsage 차원의 시간당 USD 행만 허용한다. 0원/미사용 예약 요금은 거부한다.

```bash
python3 evaluation/price_receipt.py --instance-type g5.xlarge --out RUN/ondemand.UTC.json
python3 evaluation/ieee_v2_prices.py --profile new-account-v2 --account 935317843233 --instance-type g5.xlarge --ondemand-receipt RUN/ondemand.UTC.json --out RUN/prices.UTC.json
```

두 명령은 읽기 전용이다. OD receipt는 24시간 이내 검토본이어야 한다.
Spot quote의 effectiveAt과 조회 UTC는 별도 저장한다. 실제 비용 계산에는
정확한 instance ID/market/시간 구간을 연결하고 NFS·network·EBS는 GPU VM
Compute Cost에서 제외한다고 명시한다. 장시간 실험의 Spot 가격 변동은
추가 timestamped 조회로 구간을 분리한다. 오늘의 quote를 미래 Run 가격으로 쓰지 않는다.

## E2 FIS 및 복원 증거 준비

기존 `ieee_v2_observe.py`를 fresh Run 디렉터리에서 사용하여 rank 로그,
Checkpoint/Restore CR, NodeProvision, watcher 관련 event, 위험률/정책 결정을
immutable snapshot으로 남긴다. Secret/ConfigMap 내용은 수집하지 않는다.

FIS 실행 후 정확한 experiment ID에 대해서만 별도 receipt를 수집한다:

```bash
python3 evaluation/fis_receipt.py --run RUN/run.json --ownership RUN/instance-ownership.json --profile new-account-v2 --account 935317843233 --experiment-id EXACT_ID --instance-id EXACT_OWNED_SPOT_ID --out RUN/fis.UTC.json
```

이 도구는 STS 확인과 FIS GetExperiment만 호출하며 fault를 시작하지 않는다.
Run policy UID, NodeProvision UID, Spot market, exact ARN, 실험 tag를 확인한다.
FIS action 시작/종료, IMDS notice, 실제 EC2 종료 관측, Checkpoint 시작/완료,
Restore Verified, 두 rank의 first valid resumed step을 구분한다.
누락된 시각은 N/A로 남기며 FIS 완료를 복원 성공으로 대체하지 않는다.

기존 `evaluation/fis.py`는 legacy scenario/TrainingPolicy 기준의 실행 도구다.
최종 E2-B의 committed-step trigger와 NodeRequest UID 연결을 별도로 검증하기
전에는 이 도구로 새 Run을 실행하지 않는다. 오늘은 수집 준비만 완료한다.

## 11-18까지 남은 작업 및 완료 기준

1. CIFAR 공식 archive MD5, 개별 SHA-256, torchvision train=50000/test=10000 로딩 확인.
2. 실제 CIFAR/ResNet-50 CPU 2-rank save → 새 프로세스 restore → 다음 update의
   모델/optimizer/RNG 비교. 결과 로그를 보존하고 GPU 검증과 구분.
3. 선택 GPU에서 ResNet-50 batch/목표 step/반환 deadline을 파일럿으로 고정.
   새 유료 실행은 이 준비 작업의 범위가 아니다.
4. 새 Run마다 데이터 hash/이미지/설정 잠금, 실제 mount/주입/시계 동기화와
   GPU 전체 그룹 복구 경로 확인. 이전 GPT-2 부분복원 성공으로 대신하지 않음.
5. 위험 입력 선택과 provenance 잠금; 공개 replay 선택 시 기존 TLS/Run 시계
   binding만 연결. 새 위험 예측 모델이나 장기간 AWS 이력 수집은 하지 않음.
6. E2 실행 전 최종 committed-step FIS trigger/정확한 소유 대상과 deadline 검증.

데이터 준비 증거와 진행 상태는 `reports/data_preparation_status.json`과
`/home/ubuntu/v6-readiness-20261117/data-preparation/`을 함께 확인한다.

## 11-17 실측 완료 결과

- WikiText-103 전처리 세 split의 크기/해시 일치, 모델·원시 데이터 11개 파일
  해시 일치를 재확인했다. 기존 데이터 다운로드/전처리를 반복하지 않았다.
- GPT-2의 `asset_manifest_sha256=cac21...`는 **모델+원시 데이터+전처리 정보를
  합친 로컬 `full-evaluation-assets.json` 파일**의 해시다. NFS 전처리
  `manifest.json` 자체 해시 `42ab5e...`와 다른 것은 정상이며 두 값을 구분해 기록했다.
- CIFAR archive와 개별 파일 manifest를 준비했다. manifest SHA-256은
  `7a68293160c2176460b85eb03d78b3b41727ddfcb218ff4bb595db031354eaf1`이다.
  첫 다운로드 대기 제한은 초과했지만 다운로드는 이후 완료했고 무결성을 확인했다.
- 실제 CIFAR-10/ResNet-50 CPU Gloo 2-rank, per-rank batch 2로 step 3 저장 →
  새 프로세스 복원 → step 4 결과가 uninterrupted 참조와 각 rank에서
  모델 전체·optimizer·RNG 해시까지 일치했다. 기존 참조 이미지와 현재
  training digest `0e521f...`에서 확인했고 전체 로그는 private 증거 디렉터리에 있다.
  두 rank의 parameters/optimizer 해시는 같고 BatchNorm buffers는 달랐다.
- 기존 AWS CPU 노드의 임시 Job에서 **현재 training image**로 NFS read-only mount,
  CIFAR train=50000/test=10000, WikiText train blocks=232588을 확인했다.
  GPU나 VM을 생성하지 않았으며 Job은 UID 확인 후 정상 삭제했다.
- 최종 렌더러의 잘못된 `capacity.instanceType` 필드를 수정한 뒤
  Karmada/AWS server dry-run **11/11 통과**. 실제 CR/PV/PVC는 생성하지 않았다.
- 평가 도구 229개(227 통과/호스트 torch 부재 2 skip), ResNet 단위 검사
  19개(17 통과/동일 사유 2 skip), 실제 CPU collective 4개 통과.
  호스트 skip을 GPU 검증 통과로 세지 않는다.
- 가격 조회 경로를 실제 읽기 전용 호출로 확인했다. 실행 가격은 Run 시작 시 다시 조회한다.
- 외부 SkyPilot 원본 bytes 및 기존 잠금 forecast hash 일치 확인.
  연속적인 실측 AWS 선점 이력은 없는 상태이며 현재 회수한 6개 점은 모두 static이다.

**학습 데이터 준비는 오늘 완료했다.** 11-18에는 선택 GPU에 맞는 ResNet batch/목표량,
위험 입력의 최종 선택과 필요한 runtime binding을 고정한다. 실제 GPU 실행·FIS는
별도 실행 단계이며 오늘의 데이터 검사만으로 전체 E1/E2 실행 완료를 선언하지 않는다.

FIS receipt 필드는 [AWS GetExperiment 공식 문서](https://docs.aws.amazon.com/cli/latest/reference/fis/get-experiment.html)를 따른다.
CIFAR 배포본은 [공식 데이터 페이지](https://www.cs.toronto.edu/~kriz/cifar.html)를 기준으로 했다.


## 야간 추가 결과

ResNet-50 2-GPU 파일럿·5,000 step·동일 Pod 복원 5회 및 격리 live Collector 연동을 확인했다. 정식 목표 후보는 24,470 step이나 Spot 공급 및 최종 freeze는 남아 있다. 자세한 한계·계측·정리 결과는 [야간 검증 보고](EVENING_ACCEPTANCE.md)를 따른다.
