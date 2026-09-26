# 설치 가이드

MGMT/Karmada가 있고 AWS에는 이전 NodeProvisioner만 설치한 상태라면 [처음부터 따라 하는 설치·검증 가이드](from-existing-mgmt-validation-guide.md)를 사용하세요. 아래 문서는 개별 System 설치 참고용입니다.

명령은 Linux 관리 단말에서 실행합니다. Kubernetes/Karmada 관리 권한, kubectl, Docker, jq, Python 3 + PyYAML, registry 접근 권한이 필요합니다. 기존 검증 가이드의 venv를 재사용하거나 `python3 -m venv .venv && . .venv/bin/activate && pip install PyYAML==6.0.2`로 준비합니다. AWS Member `aws`와 MGMT 실행 클러스터는 별도 kubeconfig로 구분합니다.

```bash
set -eo pipefail
export MGMT_KUBECONFIG=/secure/mgmt-host.kubeconfig
export KARMADA_KUBECONFIG=/secure/karmada-admin.kubeconfig
export AWS_KUBECONFIG=/secure/aws-member.kubeconfig
export ONPREM_KUBECONFIG=/secure/onprem-member.kubeconfig
export SPOT_RISK_COLLECTOR_IMAGE=ghcr.io/gprojectdev/vm-spot-risk-collector:validation-001
export POLICY_MANAGER_IMAGE=ghcr.io/gprojectdev/policy-manager:validation-001
export CHECKPOINT_COORDINATOR_IMAGE=ghcr.io/gprojectdev/checkpoint-coordinator:validation-001
export SPOT_RECOVERY_IMAGE=ghcr.io/gprojectdev/spot-recovery-controller:validation-001
export RUNTIME_COLLECTOR_IMAGE=ghcr.io/gprojectdev/training-runtime-collector:validation-001
export SPOT_WATCHER_IMAGE=ghcr.io/gprojectdev/spot-watcher:validation-001
```

## 1. 기존 시스템 업데이트

아래 저장소의 이번 확장 버전을 사용합니다. 기존 설치 환경은 해당 저장소의 변경된 CRD부터 적용합니다. 기존 NodeProvision을 재생성하지 마세요. NodeProvision 삭제는 EC2 종료를 유발합니다.

```bash
git clone --branch In-aws-create-WorkerNode --single-branch https://github.com/GProjectdev/PublicCloud-VM-Provisioner_test.git provisioner
git clone --branch main --single-branch https://github.com/GProjectdev/Stateful-Migration-Operator-with-PV.git stateful
git clone --branch main --single-branch https://github.com/GProjectdev/Karmada_with_PVMigration.git pv
git clone --branch main --single-branch https://github.com/GProjectdev/HybridSpotVM_ManagementSystem.git System
```

Provisioner의 AWS VPC guide에 따라 Member에 Controller, NetConfig, credential Secret을 설치합니다. Stateful Migration의 runtime/Member/Checkpoint Controller와 PV Migration도 각 README 순서대로 설치합니다. FluidCR HTTP runtime 보고 기능은 이번 runtime 확장이 포함된 payload를 사용해야 합니다.

NodeProvision CRD는 Member와 Karmada 양쪽에 설치합니다. NetConfig와 AWS credential Secret은 Member의 NodeProvision namespace에 둡니다. 같은 NodeProvision을 Member에 수동 생성한 뒤 MGMT에 같은 이름으로 다시 만들면 중복 VM을 만들 수 있으므로, 기존 리소스의 관리권 이전은 별도로 확인합니다.

## 2. 새 System 이미지 빌드

```bash
cd System
docker build --build-arg COMPONENT=vm-spot-risk-collector -t "$SPOT_RISK_COLLECTOR_IMAGE" .
docker build --build-arg COMPONENT=policy-manager -t "$POLICY_MANAGER_IMAGE" .
docker build --build-arg COMPONENT=checkpoint-coordinator -t "$CHECKPOINT_COORDINATOR_IMAGE" .
docker build --build-arg COMPONENT=spot-recovery-controller -t "$SPOT_RECOVERY_IMAGE" .
docker build --build-arg COMPONENT=training-runtime-collector -t "$RUNTIME_COLLECTOR_IMAGE" .
docker build --build-arg COMPONENT=spot-watcher -t "$SPOT_WATCHER_IMAGE" .
docker push "$SPOT_RISK_COLLECTOR_IMAGE"
docker push "$POLICY_MANAGER_IMAGE"
docker push "$CHECKPOINT_COORDINATOR_IMAGE"
docker push "$SPOT_RECOVERY_IMAGE"
docker push "$RUNTIME_COLLECTOR_IMAGE"
docker push "$SPOT_WATCHER_IMAGE"
```

자세한 per-component build/push 명령은 [컴포넌트 이미지 가이드](component-images.md)를 따릅니다. 루트 Dockerfile은 `--build-arg COMPONENT=<component>`로 `cmd/<component>`를 빌드합니다. `--mode`나 단일 `SYSTEM_IMAGE`를 사용하지 않습니다. Private registry라면 각 실행 클러스터에 imagePullSecrets도 설정합니다.

## 3. CRD와 Karmada 권한

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -k config/crd
kubectl --kubeconfig="$AWS_KUBECONFIG" apply -k config/crd
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -k config/karmada
```

Provisioner의 NodeProvision RIC도 Karmada에 적용합니다. TrainingRuntime RIC는 이 저장소에 포함되어 있습니다. Checkpoint 및 Restore RIC는 Stateful Migration 설치에서 제공합니다. on-prem target이 준비되어 TrainingRuntime 수집이 필요할 때만 같은 CRD와 runtime Deployment를 on-prem에 설치합니다. GPU가 없는 on-prem 검증에서는 GPU label과 RuntimeClass 입력도 필수가 아닙니다. SpotWatcher는 AWS에만 배포합니다.

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f config/runbook/nodeprovision-status.yaml
```

이 파일은 status RIC만 설치하며 PropagationPolicy를 추가하지 않습니다. System이 만드는 개별 NodeProvision policy가 `aws`를 선택합니다. 기존의 광범위한 PropagationPolicy와 충돌하지 않는지 적용 전에 확인하세요.

## 4. MGMT Controller의 Karmada 연결

```bash
export OUTPUT_KUBECONFIG=/secure/hybridspot-controller.kubeconfig
bash scripts/create-karmada-kubeconfig.sh
kubectl --kubeconfig="$MGMT_KUBECONFIG" apply -f config/management/namespace.yaml
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system create secret generic hybridspot-karmada-kubeconfig \
  --from-file=kubeconfig="$OUTPUT_KUBECONFIG" --dry-run=client -o yaml | \
  kubectl --kubeconfig="$MGMT_KUBECONFIG" apply -f -

# legacy 단일 controller가 남아 있으면 split controller 시작 전에 먼저 멈춥니다.
LEGACY_DEPLOYMENT="$(kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system get deployment/hybridspot-management --ignore-not-found -o json)"
if test -n "$LEGACY_DEPLOYMENT"; then
  LEGACY_SELECTOR="$(printf '%s' "$LEGACY_DEPLOYMENT" | jq -r '.spec.selector.matchLabels | to_entries | map("\(.key)=\(.value)") | join(",")')"
  kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system scale deployment/hybridspot-management --replicas=0
  kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system wait \
    --for=delete pod -l "$LEGACY_SELECTOR" --timeout=180s
fi

mkdir -p rendered
kubectl kustomize config/management |
  python3 scripts/render-validation-manifests.py > rendered/rendered-management.json
# rendered/rendered-management.json의 image와 Secret mount를 확인한 뒤 적용합니다.
kubectl --kubeconfig="$MGMT_KUBECONFIG" apply -f rendered/rendered-management.json
```

MGMT의 4개 Deployment는 기존 `hybridspot-karmada-kubeconfig` Secret과 기존 Karmada RBAC identity를 공유합니다. 이것은 보안 격리가 아닙니다. 독립 leader-election ID로 lease 충돌을 피하되, legacy `hybridspot-management`와 새 split controller를 동시에 실행하지 않습니다. 생성 스크립트의 token은 24시간을 요청하며 실제 만료시간은 API server 설정에 따라 짧아질 수 있습니다. 만료 전에 kubeconfig Secret을 갱신하고 Deployment를 rollout restart합니다. 운영 환경에서는 조직의 token 자동 갱신 체계를 사용하세요. Karmada API 주소는 MGMT Pod에서 도달 가능해야 합니다.

## 5. AWS Member 설치

```bash
# runtime leader-election ID가 hybridspot-runtime에서 hybridspot-training-runtime-collector로 바뀐 upgrade에서는 먼저 기존 Pod를 종료합니다.
RUNTIME_DEPLOYMENT="$(kubectl --kubeconfig="$AWS_KUBECONFIG" -n hybridspot-system get deployment/training-runtime-collector --ignore-not-found -o json)"
if test -n "$RUNTIME_DEPLOYMENT"; then
  RUNTIME_SELECTOR="$(printf '%s' "$RUNTIME_DEPLOYMENT" | jq -r '.spec.selector.matchLabels | to_entries | map("\(.key)=\(.value)") | join(",")')"
  kubectl --kubeconfig="$AWS_KUBECONFIG" -n hybridspot-system scale deployment/training-runtime-collector --replicas=0
  kubectl --kubeconfig="$AWS_KUBECONFIG" -n hybridspot-system wait \
    --for=delete pod -l "$RUNTIME_SELECTOR" --timeout=180s
fi

mkdir -p rendered
kubectl kustomize config/member |
  python3 scripts/render-validation-manifests.py > rendered/rendered-aws-member.json
# rendered/rendered-aws-member.json의 training-runtime-collector/spot-watcher image를 확인한 뒤 적용합니다.
kubectl --kubeconfig="$AWS_KUBECONFIG" apply -f rendered/rendered-aws-member.json
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system rollout status deployment/vm-spot-risk-collector
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system rollout status deployment/policy-manager
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system rollout status deployment/checkpoint-coordinator
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system rollout status deployment/spot-recovery-controller
kubectl --kubeconfig="$AWS_KUBECONFIG" -n hybridspot-system rollout status deployment/training-runtime-collector
kubectl --kubeconfig="$AWS_KUBECONFIG" -n hybridspot-system get pods -o wide
```

SpotWatcher는 Provisioner가 부착한 `ml.dcn.ssu.ac.kr/provider=AWS` 노드에서 실행됩니다. IMDSv2가 활성화되어 있어야 하며 hostNetwork로 metadata endpoint에 접근합니다. NodeProvision 이름/namespace/Member UID label과 status.instanceId를 확인하므로 일반 수동 생성 노드에는 status를 임의 기록하지 않습니다. DaemonSet 이름은 `spot-watcher` 그대로이며 일반 rolling update로 교체해도 됩니다.

TrainingRuntime collector는 Member 내부 Pod IP의 8298 포트로 접근합니다. 해당 포트는 외부에 노출하지 않고 NetworkPolicy로 Collector/Checkpoint Controller의 접근만 허용하세요. runtime 수집은 현재 StatefulSet에 대해 지원합니다.

## 6. 설치 확인과 운영

on-prem target에서도 runtime 수집기를 사용할 때만 설치합니다. 아래 명령은 AWS 전용 SpotWatcher를 설치하지 않습니다. GPU가 없는 on-prem 준비 단계에서는 GPU label과 RuntimeClass를 요구하지 않습니다. Stateful Migration의 Restore 검증기는 이 저장소의 TrainingRuntime CRD를 참조하므로, target restore 검증을 수행할 때 CRD 설치 후 Stateful Migration MGMT controller를 시작합니다.

```bash
kubectl --kubeconfig="$ONPREM_KUBECONFIG" apply -k config/crd
RUNTIME_DEPLOYMENT="$(kubectl --kubeconfig="$ONPREM_KUBECONFIG" -n hybridspot-system get deployment/training-runtime-collector --ignore-not-found -o json)"
if test -n "$RUNTIME_DEPLOYMENT"; then
  RUNTIME_SELECTOR="$(printf '%s' "$RUNTIME_DEPLOYMENT" | jq -r '.spec.selector.matchLabels | to_entries | map("\(.key)=\(.value)") | join(",")')"
  kubectl --kubeconfig="$ONPREM_KUBECONFIG" -n hybridspot-system scale deployment/training-runtime-collector --replicas=0
  kubectl --kubeconfig="$ONPREM_KUBECONFIG" -n hybridspot-system wait \
    --for=delete pod -l "$RUNTIME_SELECTOR" --timeout=180s
fi
kubectl --kubeconfig="$ONPREM_KUBECONFIG" apply -f config/member/namespace.yaml -f config/member/rbac.yaml
mkdir -p rendered
python3 scripts/render-validation-manifests.py \
  < config/member/runtime.yaml > rendered/rendered-onprem-member.json
kubectl --kubeconfig="$ONPREM_KUBECONFIG" apply -f rendered/rendered-onprem-member.json
kubectl --kubeconfig="$ONPREM_KUBECONFIG" -n hybridspot-system rollout status deployment/training-runtime-collector
```

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" get crd | grep training.dcnlab.com
kubectl --kubeconfig="$KARMADA_KUBECONFIG" get resourceinterpretercustomizations
kubectl --kubeconfig="$AWS_KUBECONFIG" get nodeprovisions -A -o yaml
```

이후 [두 Worker 운영 가이드](two-worker-runbook.md)를 따릅니다. Management Controller RBAC에는 workload StatefulSet과 ResourceBinding 수정 권한이 없으므로 workload placement를 자동 변경하지 않습니다.
