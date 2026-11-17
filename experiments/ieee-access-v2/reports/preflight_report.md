# 2026-11-17 사전 점검 — 개정 계획 기준

정식 평가 결과가 아닌 기능 점검/예비 측정 보고서다. 기준 문서는
`Hybrid_DDP_IEEE_Access_Experiment_Plan_v2.md`와 동일 내용의 Codex 실행계획 PDF다.
요청한 두 번째 `.md`는 현재 Evaluation 경로에 없어서 해당 PDF를 읽었다.
원본 문서는 수정하지 않았으며 해시는 `config/plan_sources.json`에 기록했다.

## 판정

- **N0: 기능 조건 통과, 정식 실행은 조건부.** 2 OD에서 새로운 namespace/PVC로
  정상 DDP 예비 측정을 완료했다. 다음 Run 전 현재 계정 GPU quota 수치 확인,
  실제 배포 설정/이미지 잠금, 새 Run 비용·시간 및 수집기 점검이 필요하다.
- **N1: 조건부.** 실제 GPT-2 Checkpoint/NFS/새 VM Restore/학습 재개가 확인됐다.
  공통 Adaptive 설정과 자연 중단 시 처리 규칙을 잠근 후 실행한다.
- **E2: NO-GO.** 195초 Trace 입력, 정책 수식의 문서/코드 차이, 공통 복구 경로,
  전환 도중 자연 선점 후 복구가 해결·검증되지 않았다. 내일 N0와 E2 준비를 구분한다.

## 오늘 실제 확인한 내용

| 항목 | 관측 / 근거 |
|---|---|
| 환경 | MGMT, Karmada API, AWS `aws1`, ONPRE1 접근. Karmada API와 MGMT 실행 Pod 구분 |
| AWS | 계정 `935317843233`, profile `new-account-v2`, Seoul `ap-northeast-2c`, g5.xlarge |
| GPU quota | 실제 세 대 동시 준비는 성공. Service Quotas 조회는 AccessDenied로 수치 미검증 |
| OD 가격 | 공식 공개 가격표 Linux g5.xlarge $1.237/hour. Spot 이력 별도 보존 |
| NFS | AWS NFS `10.50.1.98`, /mnt/nfs. 점검 시 약 456 GB 가용. 데이터/모델 SHA manifest 보존 |
| GPT-2 | Small, 전체 WikiText-103 tokenized train, rank당 batch4, seq512, seed42, AdamW lr5e-5 |
| Checkpoint | 정기 저장·durable NFS archive·동일 VM 재개 반복 성공 |
| 전환 | 통제 위험 입력 → OD 준비 → rank1 부분 C/R → Verified → 이전 Spot 실제 종료 성공 |
| 상태 검증 | model/optimizer/RNG/step audit 모두 true, 실제 CRI-O restored container ID 대조 |
| 역전환 | 새 Spot에서 RestoreRequest Verified와 Step3124 이후 진전 확인. 전체 정리 완료 아님 |
| 실제 자연 선점 | 새 Spot `i-03303de44b18edabb`가 AWS `Server.SpotInstanceTermination`으로 종료. 오늘 FIS 아님 |
| 실패 | 역전환 정리 대기 중 자연 선점 이후 자동 복구가 멈춤. 기존 partial 소유권의 자동 handoff 미구현 |
| FIS | 전용 Spot 1대 대상 template 생성/권한 시뮬레이션 성공. OD 대상 implicitDeny. 오늘 실제 FIS 실행 없음 |

### 연속 정상 Pilot과 학습량 동결

전환 디버깅 로그와 분리한 새 실행 `v6-normal-pilot-gpt2`를 기존 OD 두 대에서 수행했다.
새 namespace `v6-normal-pilot-20261117`, 새 Checkpoint PVC, 정기 저장 없음.
VM을 재사용했으므로 정식 N0의 startup/cost 측정으로 사용하지 않는다.

- 60초 warm-up 제외, UTC **03:06:09.809–03:21:09.809**의 연속 900초 측정.
- 두 rank 각각 **1,035 samples**, 관측 구간 899.271초, 최대 진전 간격 1.690초.
- rank0 중앙값 **0.75850046초**, rank1 **0.75840206초**, 단일 attempt 유지.
- 공통 목표 **4,747 committed global steps**, **19,443,712 useful tokens**로 고정.
  Step 중앙값 기준 60분 크기이며 실제 E2E에는 startup/CP/복원/반환 지연이 추가된다.
- 이전 4,836 Step 제안은 저장/재시작 대기가 섞인 잠정값이어서 폐기했다.

첫 정기 Checkpoint의 CR start→completion은 UTC 00:43:54→00:45:23, **89초**다.
NFS export는 별도 비동기 완료 시각을 가진다. 이 89초를 정확한 DDP-only stall로
치환하지 않는다. CP 시작/종료 제어 이벤트 계측을 보강해야 정확한 stall 합집합을
계산할 수 있다. Raw step/log/CR 시각은 보존했으며 누락 계측값을 추정해 채우지 않는다.

## 수정·업데이트

1. Provisioner: 긴 대체 NodeProvision 이름의 pre-pull Job label 길이 제한,
   정확한 owner UID 확인, `/usr/local/bin`과 `/usr/bin` crictl 경로 지원.
   AWS/ONPRE1 배포 완료. 기존 bootstrap/EC2 idempotency 수정도 반영했다.
2. System: 완료된 SpotRecovery가 후속 policy generation 변경으로 Rejected로
   덮어써지지 않도록 수정. 과거 기록 복원에는 별도의 UID-bound 완료/복원 증거 요구.
3. System 추가: 기존 대체 VM의 **생성 operation**과 **다음 retirement operation**을
   혼동하던 조건 수정. Completed/current-generation 생성 기록의 정확한 NP UID,
   policy/workload UID/cluster를 대조한다. 현재 world의 신선한 Running/모든 rank
   상태도 요구해, 새 Spot이 이미 죽은 상태에서 OD를 삭제하지 않도록 한다.
   생성 annotation이나 완료 상태를 수동 변경하지 않는다.
4. Stateful artifact: 검증 완료된 immutable archive를 매번 다시 SHA 계산하던 경로
   중지. stale/new generation은 계속 검증. 메모리 2Gi, GOMEMLIMIT 1536MiB,
   request256Mi/250m 반영. ONPRE1 rollout 완료. AWS GPU worker 정리 후 desired=0인
   경우, 이는 새 worker에서 실제 실행 성공을 대신하는 증거가 아니다.
5. 기존 Fixed/Adaptive 보정 대기 job 중지. 새 N0/N1/S0/S1/P0/P1 비교군과 설정 작성.
   기존 namespace가 고정된 runner에 새 비교군 이름만 넣어 실행하지 않는다.

추가 origin 수정은 `go test ./...` 통과 후 Buildah build/push, registry digest 확인,
MGMT rollout 완료. **이 수정 후 역전환 전체 E2E 재시험은 아직 수행하지 않았다.**
이미지 전체 목록은 `/home/ubuntu/v6-readiness-20261117/installed-images-final.json`.

- VM management: `docker.io/jeongseungjun/hybrid-spot-vm-system@sha256:9ce8c5418e85022c6de9a40f62f9108faaf22adb160f0f67411e2ee23ce4a957`
- Provisioner: `docker.io/jeongseungjun/my-publiccloudvm-provisioner@sha256:893407968c2e34977913100fd9e2c52731ddfb8994d6defdf40aab34ddbcb382`
- Artifact: `docker.io/jeongseungjun/stateful-migration-system@sha256:3ab71999a14ade83a565d34fbecd599d28dbbb0a6814fbd2b36521538e72b05a`

## E2 실행 전에 해결할 차이

- 새 Trace를 원본 그대로 확보했다: SHA256
  `568de3507c66707eaa222fdcf26ab98ee8032959dc72bf3c5c456722be912562`,
  20,158 samples, `metadata.gap_seconds=195`. 원본에는 start_time/risk_population이 없다.
  현재 `paper_sarima_feed.py`는 3600을 나누는 gap과 명시적인 시작 시각/모집단을 요구한다.
  인과적 시간 집계, 상대 시간축의 의미, 모집단 해석을 문서화하고 테스트해야 한다.
  300초로 바꾸거나 가짜 서울 위험률을 만들어 통과시키지 않는다.
- 현재 VM 결정은 `maxStableSpotWorkers`의 독립 생존확률 제한과 선택적인 expected-loss
  economics다. Desai §4.3의 유효 처리율+switching-cost 식과 동일하다는 증거가 없다.
  원논문 PDF와 직접 대조하는 검증도 이번 경로에서는 완료하지 못했다.
  현 구현의 위험 관리 효과를 평가할지, 문서의 엄격한 재현 요구를 변경할지 정리한다.
- Static의 fixedOnDemand와 Proposed가 다른 복구 구현을 쓰면 ON/OFF-only 비교가 아니다.
  네 arm 모두 `planned-partial=disabled`로 공통 full-group 경로를 선택하는 프로토콜을
  준비했다. 이 경로의 실제 FIS/전환 시험이 추가로 필요하다.
- 기존 partial 전환 중 다른 Spot이 사라질 때 자동 ownership handoff가 없다.
  source fencing/UID 검증을 없애 해결하지 않는다. 이는 여전히 알려진 제한이다.
- `paperProfile`은 실제 asynchronous 비용이 필요하다. synchronous CRIU/export 시간을
  GPU→DRAM 시간으로 넣지 않는다. 실제 사용한 Adaptive 구현/분기와 interval을 기록한다.

## 다음 순서

11/18: quota 조회 권한과 preflight/수집기 점검 후 새 N0 Run. N0 완료 로그/모델 상태와
VM 실제 반환까지 확인하고 N1 착수 여부 판단. N0는 오늘 sizing Pilot과 분리한다.
11/19까지: N1 및 causal SARIMA Preview, 고정된 28일/7일 구간과 공통 복구 검증,
S/P 정책 divergence 확인 후 E2 설정 잠금. 결과를 보고 구간/임계값을 재선정하지 않는다.

## 증거 경로

- 전체: `/home/ubuntu/v6-readiness-20261117`
- 전환 성공: `live/forward-proof.json`
- 자연 선점: `live/natural-spot-termination.json`
- 연속 Pilot: `live/normal-pilot/normal-pilot-confirmed.json`
- Checkpoint 상세: `live/checkpoint-timing-evidence.json`
- Raw rank CSV: `experiments/FUNCTIONAL`, `experiments/NORMAL-PILOT`
- Trace: `trace/source_manifest.json`
- 정리: `live/cleanup-provider-verification.json`, `live/finish-completed.json`

인증정보, kubeconfig, Secret, SSH key, 원본 모델/데이터는 Git에 포함하지 않는다.

## 조기 종료 및 정리 결과 (12:38 KST 확인)

테스트 EC2 **4대 모두 terminated**, 연결 EBS **4개 모두 삭제**, 이번 실행에서 만든
EC2 key pair **4개 삭제**를 확인했다. 초기 Spot의 종료 기록은 당시 직접 AWS 응답
`sample-010929/ec2.json`으로 증명한다. 나중 목록에서 빠진 것만으로 종료를 추정하지 않았다.
전용 FIS template/CloudWatch alarm 삭제, 두 임시 namespace의 webhook selector 추가만
제거, 자동 정리 timer 해제. FIS IAM Role은 종료된 원래 Spot ARN/실험 태그로 제한된
상태로 남아 있으며 새 Run의 대상으로 재사용하면 안 된다.

Karmada/AWS의 해당 NodeRequest·NodeProvision·학습 Pod는 남지 않았다. CP/RestoreRequest,
Verified receipt가 있는 RestorePlan, 성공/거절 SpotReplacement는 증거로 보존했다.
RestorePlan의 과거 Running 표시는 현재 학습 실행을 의미하지 않는다. 완료 receipt는
member의 재실행을 막는다. 보존한 Completed CP의 finalizer는 삭제 대기가 아니며
강제 제거하지 않았다. 역전환의 미완료 기록은 원본 정책 삭제 후 Rejected로 남았다.
Namespace/PVC/PV/NFS/학습 결과/운영 master/WireGuard/인증정보는 보존했다.
운영 controller Deployment는 최종 inventory에서 모두 Ready였다. GPU worker가 없어서
artifact/SpotWatcher DaemonSet desired=0인 상태는 새 GPU에서의 검증 성공으로 세지 않는다.

백업: `/home/ubuntu/v6-readiness-20261117`; 이 문서와 설정만 Git에 반영하며 원본 비밀
자료는 반영하지 않는다. 예정된 18:00보다 일찍 유료 점검을 마쳤다. 다음 유료 Run을
자동 예약하거나 시작하지 않았다.
