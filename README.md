# Hybrid Spot VM Management System

고정된 DDP Worker 수를 유지하면서 Spot/On-Demand 구성을 선택하고, 시간 기반 Checkpoint를 요청하는 Kubernetes Controller 모음입니다. MGMT의 Controller는 Karmada API를 사용합니다.

| 실행 위치 | 컴포넌트 | 역할 |
|---|---|---|
| MGMT 실행 클러스터 | `vm-spot-risk-collector` | 외부 HTTPS hazard/static 실험 입력을 `SpotRiskProfile.status`에 기록 |
| MGMT 실행 클러스터 | `policy-manager` | 고정 Worker 수의 Spot/On-Demand 목표와 NodeProvision을 조정 |
| MGMT 실행 클러스터 | `checkpoint-coordinator` | 시간 기반 Checkpoint 요청과 중복 방지 |
| MGMT 실행 클러스터 | `spot-recovery-controller` | 검증된 `SpotRecovery`에 연결된 old Spot NodeProvision cleanup |
| AWS Member | `training-runtime-collector` | TrainingRuntime 수집 |
| AWS Member의 AWS 노드 | `spot-watcher` | IMDSv2 Spot 신호 감지와 NodeProvision.status.spot 기록 |

Karmada가 RIC에 따라 Member status를 MGMT로 반영합니다. workload placement 변경은 사용자 책임입니다. SpotWatcher가 직접 Spot status를 기록하며 Provisioner는 별도 provisioning 필드를 관리합니다.

## 설치와 실행

[기존 MGMT/Karmada + AWS Member, worker 0개에서 시작하는 단계별 설치·검증 가이드](docs/from-existing-mgmt-validation-guide.md)를 먼저 사용하세요. NodeProvisioner 갱신, 최초 VM 2개 생성, GPU/NFS, checkpoint, 사용자 placement, restore와 Spot cleanup을 통과 기준과 함께 설명합니다.

[설치 가이드](docs/installation.md), [컴포넌트 이미지 빌드 가이드](docs/component-images.md), [두 Worker 운영 가이드](docs/two-worker-runbook.md)를 순서대로 사용하세요. [위험률 입력 계약](docs/risk-feed.md)을 공급자에 맞춰 설정하고, [컴포넌트 책임과 검증 경계](docs/architecture.md)를 확인하세요.

이 저장소의 System 이미지는 6개 고정 entrypoint로 나눕니다. 루트 Dockerfile은 `--build-arg COMPONENT=<component>`를 필요로 하며 `cmd/<component>`를 빌드합니다. `--mode` 방식이나 단일 `SYSTEM_IMAGE`를 사용하지 않습니다. 문서 예시 태그는 `ghcr.io/gprojectdev/<component>:dev`이지만, 이미지가 이미 게시되어 있다고 가정하지 않습니다. 사용자가 접근 가능한 registry에 빌드 및 push한 뒤 Deployment와 DaemonSet 이미지를 변경합니다.

## 관련 저장소

- [NodeProvisioner](https://github.com/GProjectdev/PublicCloud-VM-Provisioner_test/tree/In-aws-create-WorkerNode): NodeProvision API와 실제 VM 생성/삭제. 해당 브랜치를 사용합니다.
- [Stateful Migration](https://github.com/GProjectdev/Stateful-Migration-Operator-with-PV): FluidCR 실행 확장, archive 전송, Restore 검증과 suspension.
- [PV Migration](https://github.com/GProjectdev/Karmada_with_PVMigration): PV metadata 수집과 PV Work 생성/분리.

FluidCR 원본 다운로드 폴더는 독립 Git 저장소가 아니므로 이번 확장은 Stateful Migration 저장소의 runtime 배포 경로에 포함합니다.

## 상태와 책임

Spot 위험률은 외부 공급자의 시간당 hazard를 받습니다. AWS Spot 가격이나 placement score를 hazard로 간주하지 않습니다. 실험용 static 입력은 실험용임을 status에 표시합니다.

VM Spot Risk Collector는 `internal/collector`에 구현된 위험 입력 수집기입니다. 외부 HTTPS feed 또는 static 실험 입력을 소비하며, AWS hazard estimation을 구현하지 않습니다. HTTPS feed 예시는 `config/samples/11-spot-risk-profile-https.yaml`의 `aws-risk-feed`이며, placeholder endpoint `https://risk-feed.example.invalid/aws/ap-northeast-2/g4dn.xlarge`는 반드시 실제 feed로 바꾸고 `TrainingPolicy`가 같은 `SpotRiskProfile`을 참조하게 해야 합니다.

Checkpoint 주기는 초 단위입니다. 요청 시각과 실제 안전한 optimizer-step에서 저장되는 시각은 다를 수 있습니다. DDP rank의 Checkpoint 동기화와 통신 재구성은 FluidCR이 담당합니다.

초기 정책은 전달받은 위험률과 안정성 기준으로 Spot/On-Demand 목표 수를 선택합니다. 아직 측정하지 않은 손실 비용을 0으로 가정하지 않으며, 금액 기반 fallback은 `costEvaluated=false`로 구분합니다. 이미 생성된 VM의 시장 유형은 즉시 변경하지 않고 명시적인 교체 절차가 필요합니다. Checkpoint 주기는 초기 위험 구간 정책을 사용하고, 측정된 저장·복사 시간을 설정하면 시간 기반 비용 최소화로 전환합니다.

원본 실행 차단, PV 준비, archive 준비, 실제 복원 검증은 서로 다른 증거입니다. PVMigration Completed와 Pod Ready만으로 원본 노드 삭제를 허용하지 않습니다. Source fencing과 사용자 placement 단계를 완료해야 복원이 진행됩니다.

## 개발 검증

```bash
go test -mod=readonly ./...
go vet ./...
go build -mod=readonly ./cmd/...
```

단위 테스트와 manifest 검증은 실제 EC2 생성/삭제, IMDS, Karmada 전파 지연, GPU/NCCL 복원의 실험을 대체하지 않습니다. 실제 환경 확인 절차는 운영 가이드에 있습니다.
