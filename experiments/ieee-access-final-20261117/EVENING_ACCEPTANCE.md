# 2026-11-17 야간 추가 검증 결과

오늘 22:00 KST까지 유료 검증을 허용한 요청에 따라 수행했다. 정식 논문 E1/E2 결과와 사전 검증 결과를 구분한다. 원본 평가계획 PDF와 핵심 Controller/Desai 정책은 변경하지 않았다.

## 실제 GPU 확인

- g5.xlarge On-Demand 2대, ResNet-50/CIFAR-10, rank별 batch 32, seed 42로 두 rank 모두 5,000 step 완료. GPU/NCCL 학습과 최종 parameter/optimizer 상태 일치 검사 통과. Rank별 BN buffer 차이는 허용되는 학습 특성으로 별도 처리한다.
- 기존 Coordinator의 120초 full-group 주기 체크포인트를 활성화한 후 저장→동일 Pod 내 프로세스 복원→학습 재개 5회 완료. 10개 rank 복원 감사에서 모델·optimizer·RNG·step 검사 통과.
- 이는 VM 교체나 다른 Pod로의 이주 검증이 아니다. 이번 실행에서 새 VM의 full-group RestoreRequest Verified 및 FIS 복구는 검증하지 못했다.
- 학습 경과 986.932초, 처리 samples 320,000. 관측된 application pause/resume 합집합 108.617273초이며 archive I/O 시간으로 해석하면 안 된다.
- 120초 설정에 대한 pause 시작 간격은 약 139~166초, 직전 resume→다음 pause는 약 120~146초. CR 완료·export 및 조정 지연이 포함될 수 있어 설정 주기와 실제 pause 간격을 동일시하지 않는다. 상세값은 `reports/evening-evidence/checkpoint-cadence.json` 참조.
- `T_startup=91.627초`는 workload scale 요청부터 첫 step까지이며 VM 생성 대기 시간을 포함하지 않는다. 정식 E2E 시작시간으로 재사용하지 않는다.
- 초기 no-checkpoint 파일럿 이후 120초 체크포인트로 설정을 변경했으므로 이 실행은 정식 E1-A/B 또는 E2-A/B 비교 자료로 사용하지 않는다.

## Spot/FIS 제한과 다음 실행

기존 subnet의 ap-northeast-2c에서 g5.xlarge Spot이 두 차례 `InsufficientInstanceCapacity`로 실패했다. 이는 이번 응답에서 GPU quota 또는 IAM 권한 부족으로 판정된 것이 아니다. 새로운 AZ subnet/기반 인프라는 만들지 않았다. 두 번째 시도는 Spot만 먼저 요청하고 On-Demand 생성은 보류해 불필요한 대기 비용을 피했다. 실제 FIS는 실행하지 않았다.

다음 유료 실행 전에 Spot 공급 가능 여부를 확인하고, 공급/하드웨어 선택이 바뀌면 해당 모델의 파일럿과 비교 설정을 다시 고정해야 한다. 공급 확보 후 ResNet-50 새 VM 복원 및 정확한 현재 NodeRequest/NodeProvision UID에 묶인 FIS 1회 검증이 남아 있다. 과거 GPT-2 복원 성공이 이 경로의 검증을 대신하지 않는다.

## 데이터·위험률 연동

- 기존 WikiText-103 및 CIFAR-10 데이터를 유지했다. ResNet-50 GPU 파일럿은 warmup 60초 이후 정상 학습 180초, rank별 1,179개 iteration을 측정했다. 더 느린 rank의 median은 0.147123764초다.
- 동일 규칙 `ceil(3600/median_iteration)`의 ResNet 목표 후보는 24,470 step, fault 후보는 12,235 step이다. GPT-2 후보는 4,747/2,374이며 모델 간 공유하지 않는다. ResNet template에 측정 후보를 기록했으나 정식 hardware/comparison freeze는 보류했다.
- 공개 SkyPilot trace→기존 SARIMA forecast→HTTPS feed→실제 Collector→격리 SpotRiskProfile의 live 연동 통과. fresh ready=true, 오래된 가격 입력 ready=false, 가격 갱신 후 ready=true 복구를 확인했다. λ=0.3702637197/h. 서울 실측 위험률이 아닌 공개 이력 proxy다.
- 실제 기존 학습 정책의 static 위험률 binding은 바꾸지 않았다. 격리 테스트 뒤 Collector의 원래 TLS 신뢰 설정을 복구하고 테스트 feed와 인증자료를 제거했다.
- 이전 replay manifest에서 필수 `paperEstimator.method: sarima` 누락을 수정했다. 모델/trace/forecast 계산식은 변경하지 않았다.

## 실행 도구 보완 및 검사

- 현재 Pod UID, 최근 rank별 step, rollback, 완료 상태, deadline, 단일 fault 여부를 검사하는 오프라인 `evaluation/final_fault_gate.py` 추가. 이 도구 자체는 AWS FIS 실행기가 아니다. 실제 scoped FIS 호출과의 연결/검증은 남아 있다.
- 기존 고정 namespace용 legacy FIS 실행기는 final protocol 입력을 거부하도록 했다. 잘못된 대상/기준으로 실행하지 않도록 한 제한이다.
- `evaluation/price_watch.py`는 명시적 profile/account/deadline을 받아 240초 기본 주기로 가격 영수증을 보관한다. UID와 Run label이 일치하는 ConfigMap의 `prices.json`만 UID/resourceVersion 검사를 거쳐 갱신한다. forecast/manifest/clock은 변경하지 않는다. 실제 AWS 조회와 ConfigMap patch를 격리 smoke test로 확인했다. 외부 feed를 선택한 Run에서 TLS/clock을 설정한 뒤 사용한다.
- EC2 ledger에 provider UID 대조, KeyName 및 EBS VolumeId/DeleteOnTermination 증거 수집을 추가했다. 과거 instance ID가 NotFound여도 다른 현재 인스턴스 관측을 막지 않되, NotFound를 종료 완료로 판정하지 않는다.
- 평가 도구 전체 unittest 242개: 240 통과, 호스트 Torch 미설치로 2 skip. 실제 GPU 학습 검증은 별도 위 결과로 수행했다.

## 정리와 비용

검증용 EC2 `i-03fc72d94bb49288b`, `i-0ecde5eae050849e0` 모두 terminated 확인, 소유가 확인된 임시 keypair 제거 완료. 두 번째 Spot-only 시도는 생성된 EC2가 없고 FIS도 시작하지 않았다. NodeProvision 정상 삭제 절차를 사용했으며 finalizer 강제 제거는 하지 않았다. Checkpoint PVC/PV/NFS 데이터와 운영 기반 인프라를 보존했다.

두 On-Demand VM의 LaunchTime부터 terminated 관측까지 $1.237/h로 산정한 보수적 GPU compute 추정 상한은 약 **$4.02**다. 실제 청구액이 아니며 EBS·네트워크·기존 NFS 비용은 제외한다. 기존 EBS ID 영수증을 확보하지 못해 개별 볼륨 ID 종료 증명은 없다. root DeleteOnTermination 설정과 별도 AZ/100GiB/생성시간 범위 조회에서 잔여 후보 0개를 확인했다. 이 제한을 숨기지 않는다.

앞으로는 목표 step·최종 감사·증거 저장이 끝나면 즉시 해당 Run 자원을 정상 정리하고 분석은 GPU 없이 수행한다. 정리 watchdog은 최종 방어선으로만 둔다.

원시 증거/백업: `/home/ubuntu/v6-readiness-20261117/evening-acceptance/`. Git에는 선별된 작은 결과만 포함하며 TLS private key, Secret 백업, 인증정보는 포함하지 않는다. 정식 E1/E2는 아직 NOT_RUN이다.
