# 이미 AWS가 선택된 복원 작업 재개

## 범위

대상: Webhook 등록 누락 중 AWS가 선택되어 policy-manager가
`source placement changed during operation`으로 멈춘 Existing-capacity 작업.
Webhook 등록과 TLS 확인을 먼저 완료해야 한다.

이 패치는 현재 ResourceBinding UID로 명시적으로 요청한 경우에만 동작한다.
RestoreRequest UID, policy/workload UID, ApplicationFailure eviction task,
SourceFenced 증거, 동일 작업의 dispatch hold와 Completed PVMigration을 검사한다.
배치를 잠시 비우고 AWS 결정을 pending-placement로 보존하되 dispatch 보류는 유지한다.
이후 기존 컨트롤러가 PVC 준비 및 target Prepared 확인을 거쳐 전달을 해제한다.

**제약:** 이번 재개 경로의 PVC 생성자는 StatefulSet이 아니라 System이다.
System이 기존 volumeClaimTemplates와 이관된 PV를 검증해 동일 이름의 PVC를
먼저 생성한다. 현재 구현은 target Prepared가 PVC를 필요로 하고 StatefulSet
전달은 Prepared 뒤에 허용하므로, StatefulSet만으로 PVC를 생성하도록 바꾸려면
별도의 Pod admission 기반 선행 전달 단계가 필요하다. 이번 패치는 그 기능이 아니다.
StatefulSet 자체의 PVC 생성 시연이 필수라면 아래 마지막 재개 명령은 실행하지 않는다.
현재 작업의 체크포인트 기반 학습 복원 확인에만 이 절차를 사용한다.

PV 패치 `cad476e`도 적용해야 한다. PVMigration이 Completed가 아니면 이 패치는
진행하지 않는다. status, UID, hold annotation을 수동으로 위조하지 않는다.

## MGMT: 백업

```bash
set -euo pipefail
: "${KARMADA_KUBECONFIG:?}" "${AWS_KUBECONFIG:?}"
export NS=fluidcr-realign-121040
export RB=r50-fluidcr-demo-statefulset
export RECOVERY_BACKUP="/root/gpu-fluidcr-demo/selected-recovery-$(date -u +%Y%m%d%H%M%S)"
mkdir -p "$RECOVERY_BACKUP"
chmod 700 "$RECOVERY_BACKUP"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" \
  get resourcebindings,trainingpolicies,restorerequests,restoreplans,pvmigrations \
  -o yaml > "$RECOVERY_BACKUP/operations.yaml"
kubectl --context=kubernetes -n hybridspot-system \
  get deployment policy-manager placement-webhook -o json > "$RECOVERY_BACKUP/deployments.json"
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" \
  get pods,statefulsets,pvc -o yaml > "$RECOVERY_BACKUP/aws-workloads.yaml"
```

## Buildah 빌드 및 배포

dirty checkout은 reset하지 않는다. pull 충돌이면 중지한다.

```bash
cd /root/hybridspot-validation/System
git status --short
git pull --ff-only
test -f internal/management/selected_placement_recovery.go
export RECOVERY_TAG="selected-recovery-$(git rev-parse --short HEAD)"
for COMPONENT in placement-webhook policy-manager; do
  IMAGE="docker.io/jeongseungjun/hybrid-spot-vm-system:${COMPONENT}-$RECOVERY_TAG"
  buildah bud --layers --build-arg COMPONENT="$COMPONENT" -f Dockerfile -t "$IMAGE" .
  buildah push "$IMAGE" "docker://$IMAGE"
done

# admission이 새 복구 전환을 먼저 검증할 수 있도록 순서를 지킨다.
for COMPONENT in placement-webhook policy-manager; do
  kubectl --context=kubernetes -n hybridspot-system \
    set image "deployment/$COMPONENT" \
    manager="docker.io/jeongseungjun/hybrid-spot-vm-system:${COMPONENT}-$RECOVERY_TAG"
  kubectl --context=kubernetes -n hybridspot-system \
    rollout status "deployment/$COMPONENT" --timeout=180s
done
```

## 재개 전 확인

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" \
  get mutatingwebhookconfiguration hybridspot-placement -o yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" \
  get pvmigrations,restorerequests,restoreplans -o json |
  jq '.items[] | {kind,name:.metadata.name,uid:.metadata.uid,status}'
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" \
  get statefulset r50-fluidcr-demo --ignore-not-found
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" \
  get pod r50-fluidcr-demo-0 --ignore-not-found
```

AWS에 해당 StatefulSet/Pod가 이미 있으면 여기서 중지하고 새 상태를 확인한다.
기대 조건은 기존 RR/Plan UID 유지, source SourceFenced, PVMigration Completed,
target에는 아직 해당 StatefulSet/Pod가 없는 상태이다. 관측 결과를 조작하지 않는다.
policy-manager의 대상 kubeconfig 접근 및 기존 claim preparer 설정도 유지되어야 한다.

## 현재 작업만 명시적으로 재개

아래 명령은 실제 복원 재개를 허용한다. 이전 단계가 통과하고 위 PVC 생성 방식의
제약을 수용한 경우에만 실행한다. 이전 실험 UID를 복사하지 않고 현재 값을 읽는다.

```bash
RB_UID=$(kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" \
  get resourcebinding "$RB" -o jsonpath='{.metadata.uid}')
test -n "$RB_UID"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" \
  annotate resourcebinding "$RB" \
  "training.dcnlab.com/recover-selected-placement=$RB_UID"
```

동일 annotation이 이미 있으면 반복해서 덮어쓰지 말고 로그를 확인한다.

```bash
kubectl --context=kubernetes -n hybridspot-system \
  logs deployment/policy-manager -c manager --tail=80
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" \
  get resourcebinding "$RB" -o json |
  jq '{annotations:.metadata.annotations,clusters:.spec.clusters,suspension:.spec.suspension}'
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" \
  get restorerequests,restoreplans,pvmigrations -o json |
  jq '.items[] | {kind,name:.metadata.name,status}'
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get pods,pvc
```

중간에는 clusters가 비고 pending-placement에 AWS가 남으며 dispatching=true가
유지될 수 있다. 최종 성공은 target 복원 검증과 ResNet-50 로그의 복원 step 및
후속 step 증가로 확인한다. Pod Running만으로 성공 판정하지 않는다.

## 실패 시

dispatch 수동 해제, PV/PVC 삭제, checkpoint 재생성, failover 재실행을 하지 않는다.
복구 annotation이 소비된 뒤에는 이전 Webhook 이미지로 즉시 되돌리지 않는다.
작업 전환 후 rollback은 진행 중인 RR/Plan 상태를 함께 검토해야 한다.
annotation 실행 전 이미지 배포만 되돌릴 경우 백업 deployments.json에서 기존
manager 이미지를 확인하여 Webhook와 policy-manager를 각각 원래 이미지로 교체한다.

로컬 테스트는 안전 조건 및 admission 전환 검증이며 실클러스터 E2E 검증을 대신하지 않는다.
