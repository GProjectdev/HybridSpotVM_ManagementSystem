# 컴포넌트와 운영 경계

## 제어 경로

```text
External risk feed -> VM Spot Risk Collector -> SpotRiskProfile.status
                                                  |
                              Policy Manager / CheckpointCoordinator
                                                  |
                                   Karmada API + PropagationPolicy
                                                  |
                  aws Member: NodeProvision / FluidCRMigration / TrainingRuntime
                         |                |                 |
                    Provisioner       FluidCR         Runtime Collector
                         |                |                 |
                    SpotWatcher       checkpoint        rank telemetry
                         +------------ status --------------+
                                          |
                         ResourceInterpreterCustomization -> MGMT
```

MGMT 컨트롤러는 Karmada kubeconfig만 사용합니다. Member의 Pod, Node, IMDS, 파일시스템 접근은 Member에 설치한 컴포넌트가 담당합니다. RIC는 이미 전파된 CR의 상태를 반환하는 규칙이며, Member에만 존재하는 CR을 자동으로 MGMT에 등록하지 않습니다.

System은 6개 고정 entrypoint 이미지로 나뉩니다. MGMT에는 `vm-spot-risk-collector`, `policy-manager`, `checkpoint-coordinator`, `spot-recovery-controller` Deployment를 배포하고, AWS Member에는 `training-runtime-collector` Deployment와 `spot-watcher` DaemonSet을 배포합니다. MGMT Deployment들은 기존 `hybridspot-karmada-kubeconfig` Secret과 shared Karmada RBAC identity를 계속 사용합니다. 이 분리는 보안 격리가 아니며, 각 controller의 rollout과 leader-election ID를 분리하기 위한 운영 경계입니다. legacy `hybridspot-management` Deployment가 남아 있으면 먼저 scale 0하고 Pod 종료를 기다린 뒤 split controller를 시작하여 old/new lease가 겹치지 않게 합니다. runtime collector도 leader-election ID가 `hybridspot-runtime`에서 `hybridspot-training-runtime-collector`로 바뀌므로 upgrade 때 AWS `training-runtime-collector` Deployment를 scale 0하고 Pod 종료를 기다린 뒤 새 manifest/image로 교체합니다. `spot-watcher` DaemonSet 이름은 그대로라서 일반 rolling update 경로를 사용합니다.

| 책임 | 담당 |
| --- | --- |
| 위험률 입력 수집, 유효기간 확인 | VM Spot Risk Collector |
| 고정 Worker 수의 Spot/On-Demand 구성 결정 | Policy Manager |
| 시간 단위 주기, 중복 방지, Checkpoint CR 생성 | CheckpointCoordinator |
| 학습 step/rank/world size 수집 | Member TrainingRuntime Collector |
| IMDS 이벤트 감지, NodeProvision.status.spot 기록 | Member SpotWatcher |
| NodeProvision 상태 집계 | NodeProvision RIC |
| DDP 안전 지점에서 체크포인트와 통신 재구성 | FluidCR runtime |
| 아카이브 보관/복사 및 체크섬 검증 | Stateful Migration artifact 컴포넌트 |
| PV 메타데이터와 대상 PV 준비 | PV Migration System |
| workload placement 변경 | 사용자 |
| 복원 후 학습 재개 검증 | Stateful Migration Restore 컴포넌트 |
| 검증된 작업에 연결된 기존 NodeProvision 삭제 | Policy Manager |

## 세 가지 서로 다른 상태

- **Checkpoint 완료:** 당시 상태를 저장했다는 증거입니다. 대상 노드에서 접근 가능한 파일인지, DDP 복원이 성공했는지는 별도입니다.
- **PV Migration Completed:** 해당 작업의 PV 생성 및 Work 분리가 끝났다는 역사적 증거입니다. 실시간 스토리지 건강 상태나 학습 재개 증거가 아닙니다.
- **Restore Verified:** 지정된 체크포인트, 대상 Pod UID, 학습 진행을 확인한 복원 증거입니다. 단순 Pod Running/Ready를 대체 증거로 쓰지 않습니다.

Source fencing은 이전 소스가 동일한 학습 작업이나 공유 볼륨에 계속 쓰지 못하도록 정지·격리하는 것입니다. RB dispatch suspension만으로 실행 중인 프로세스가 정지하지 않으며, sourceFenced 선언은 실제 격리를 수행하는 명령이 아닙니다. 운영자가 확인한 뒤 해당 작업에 기록해야 합니다.

## 정책 계산

전체 Worker 수 `N`은 고정합니다. 시간당 위험률 `lambda`와 예측 시간 `h`(시간)를 받아 후보 Spot 수 `Ns`의 독립 사건 근사 생존 확률 `exp(-lambda*h*Ns)`가 `alpha` 이상인 범위에서 Spot 수를 최대화합니다. 실측 손실 비용이 없는 초기 단계에서는 금액 기준 최적화가 수행됐다고 표시하지 않습니다. 기존 VM 비율과 새 목표가 다르면 `replacement_required`로 알리고, AWS 내부 Spot-to-OnDemand 전환은 `SpotReplacement` 작업, partial-rank checkpoint 요청, Verified RestoreRequest 증거가 모두 맞을 때만 이전 NodeProvision 삭제로 진행합니다. 검증되지 않은 VM 자동 삭제는 하지 않습니다.

Checkpoint 주기 `tau`는 초 단위입니다. 초기에는 설정 가능한 위험 구간과 후보 주기를 사용합니다. 최신 측정값 `C=checkpointSeconds`, `S=copySeconds`가 설정되면 후보별 `C/tau + (lambda*Ns/3600)*tau/2 + max(0,S/tau-1)`을 비교합니다. 이 값은 학습 시간 대비 상대 overhead이며 금액이 아닙니다. 측정 입력의 기본 유효기간은 10분이고, 모르는 측정값을 0으로 간주하지 않습니다.

주기 변경은 TrainingPolicy의 checkpoint 설정 갱신으로 반영합니다. 진행 중인 Checkpoint는 중복 생성하지 않으며, 재시작 시 누락된 과거 주기를 몰아서 실행하지 않습니다. 실제 Spot 이벤트는 주기와 별도로 긴급 요청을 만들지만 runtime이 준비되지 않은 경우에는 실행을 강제하지 않습니다.

## Spot 알림 운영

VM Spot Risk Collector는 `internal/collector`에 구현된 수집기로 외부 HTTPS hazard feed 또는 static 실험 입력을 소비합니다. AWS Spot 가격, placement score, IMDS 이벤트를 hazard로 추정하지 않으며 AWS hazard estimation을 구현하지 않습니다. HTTPS feed sample은 `config/samples/11-spot-risk-profile-https.yaml`의 `aws-risk-feed`입니다. 이 sample의 `https://risk-feed.example.invalid/aws/ap-northeast-2/g4dn.xlarge` endpoint는 placeholder이며, 운영자는 실제 feed로 바꾸고 `TrainingPolicy`의 risk reference가 이 객체를 가리키는지 확인해야 합니다.

SpotWatcher는 IMDSv2를 5초마다 조회합니다. interruption notice와 rebalance recommendation은 확률 입력과 별개인 실제 이벤트이며, 사전 경고 시간과 도착이 항상 보장되는 복구 수단으로 취급하지 않습니다. 마지막 위험 이벤트는 metadata의 404 응답만으로 지우지 않습니다. NodeProvision UID와 EC2 instance ID가 일치해야 상태를 기록합니다.

AWS의 [interruption notice 문서](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/spot-instance-termination-notices.html)와 [rebalance 문서](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/rebalance-recommendations.html)를 참고하세요. Karmada 상태 반환 구조는 [Resource Interpreter 문서](https://karmada.io/docs/userguide/globalview/customizing-resource-interpreter/)를 따릅니다.

## 검증 수준

단위 테스트의 fake Kubernetes client는 실제 CRD admission, RBAC, Karmada 전파, AWS 생성, GPU/CRIU 복원을 대신하지 않습니다. 실제 운영 전에 별도 테스트 환경에서 다음 순서로 확인합니다.

1. 두 Pod의 서로 다른 rank와 동일한 worldSize, 최신 TrainingRuntime 상태를 확인합니다.
2. 두 번 이상 주기 체크포인트를 수행하고 서로 다른 ID 및 불변 아카이브를 확인합니다.
3. 컨트롤러 재시작과 중복 이벤트에서 체크포인트가 겹치지 않는지 확인합니다.
4. 테스트 인스턴스의 실제 Spot 이벤트가 Member status와 MGMT status에 반영되는지 확인합니다.
5. 사용자 placement 변경, source fencing, PV 준비, archive 준비를 완료합니다.
6. 두 rank의 복원 후 step 증가를 확인하고, 해당 작업에 연결된 기존 NodeProvision만 삭제되는지 확인합니다.
7. 오래된 UID, 다른 checkpoint ID, 누락 rank, 오래된 status에서는 삭제가 거절되는지 확인합니다.

운영 클러스터에서의 AWS/GPU 검증 완료를 이 저장소의 로컬 테스트 결과만으로 주장하지 않습니다.
