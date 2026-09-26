# System 컴포넌트 이미지

System은 단일 management 이미지가 아니라 6개 고정 entrypoint 이미지로 배포합니다. 루트 `Dockerfile`은 반드시 `--build-arg COMPONENT=<component>`를 받아 `cmd/<component>`를 빌드합니다. `--mode` 인자는 사용하지 않습니다.

| 컴포넌트 | 이미지 | 배포 대상 |
|---|---|---|
| `vm-spot-risk-collector` | `ghcr.io/gprojectdev/vm-spot-risk-collector:dev` | MGMT Deployment `vm-spot-risk-collector` |
| `policy-manager` | `ghcr.io/gprojectdev/policy-manager:dev` | MGMT Deployment `policy-manager` |
| `checkpoint-coordinator` | `ghcr.io/gprojectdev/checkpoint-coordinator:dev` | MGMT Deployment `checkpoint-coordinator` |
| `spot-recovery-controller` | `ghcr.io/gprojectdev/spot-recovery-controller:dev` | MGMT Deployment `spot-recovery-controller` |
| `training-runtime-collector` | `ghcr.io/gprojectdev/training-runtime-collector:dev` | AWS Deployment `training-runtime-collector` |
| `spot-watcher` | `ghcr.io/gprojectdev/spot-watcher:dev` | AWS DaemonSet `spot-watcher` |

아래 예시는 `/root/hybridspot-validation/System`에 clone된 System 저장소에서 실행합니다. 검증용 고유 태그 `validation-001`을 literal 값으로 사용합니다. 다른 registry/tag를 쓰는 경우에도 6개 컴포넌트 이름은 유지합니다.

```bash
cd /root/hybridspot-validation/System

docker build --build-arg COMPONENT=vm-spot-risk-collector -t ghcr.io/gprojectdev/vm-spot-risk-collector:validation-001 .
docker build --build-arg COMPONENT=policy-manager -t ghcr.io/gprojectdev/policy-manager:validation-001 .
docker build --build-arg COMPONENT=checkpoint-coordinator -t ghcr.io/gprojectdev/checkpoint-coordinator:validation-001 .
docker build --build-arg COMPONENT=spot-recovery-controller -t ghcr.io/gprojectdev/spot-recovery-controller:validation-001 .
docker build --build-arg COMPONENT=training-runtime-collector -t ghcr.io/gprojectdev/training-runtime-collector:validation-001 .
docker build --build-arg COMPONENT=spot-watcher -t ghcr.io/gprojectdev/spot-watcher:validation-001 .

docker push ghcr.io/gprojectdev/vm-spot-risk-collector:validation-001
docker push ghcr.io/gprojectdev/policy-manager:validation-001
docker push ghcr.io/gprojectdev/checkpoint-coordinator:validation-001
docker push ghcr.io/gprojectdev/spot-recovery-controller:validation-001
docker push ghcr.io/gprojectdev/training-runtime-collector:validation-001
docker push ghcr.io/gprojectdev/spot-watcher:validation-001
```

렌더러와 설치 스크립트는 `SYSTEM_IMAGE`가 아니라 다음 6개 값을 사용해야 합니다.

```bash
export SPOT_RISK_COLLECTOR_IMAGE=ghcr.io/gprojectdev/vm-spot-risk-collector:validation-001
export POLICY_MANAGER_IMAGE=ghcr.io/gprojectdev/policy-manager:validation-001
export CHECKPOINT_COORDINATOR_IMAGE=ghcr.io/gprojectdev/checkpoint-coordinator:validation-001
export SPOT_RECOVERY_IMAGE=ghcr.io/gprojectdev/spot-recovery-controller:validation-001
export RUNTIME_COLLECTOR_IMAGE=ghcr.io/gprojectdev/training-runtime-collector:validation-001
export SPOT_WATCHER_IMAGE=ghcr.io/gprojectdev/spot-watcher:validation-001
```

MGMT의 4개 Deployment는 기존 `hybridspot-karmada-kubeconfig` Secret과 기존 Karmada RBAC identity를 공유합니다. 이 분리는 보안 격리가 아니라 장애 범위와 rollout 단위를 나누기 위한 것입니다. 각 Deployment는 별도 leader-election ID를 사용해야 하며, 새 split controller를 시작하기 전에 legacy `hybridspot-management` Deployment를 scale 0하고 Pod 종료를 기다려 old/new lease가 겹치지 않게 합니다. runtime collector upgrade 명령은 [설치 가이드의 AWS Member 설치 절](installation.md#5-aws-member-설치)에 있습니다.
