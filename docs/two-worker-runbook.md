# 2 Worker StatefulSet 통합 실행 Runbook

이 문서는 `docs/installation.md`로 System 컨트롤러를 설치한 뒤, Karmada에서 2개 Pod StatefulSet 학습 workload를 운영하면서 TrainingRuntime, TrainingPolicy, Risk, RestorePlan, RestoreRequest, SpotRecovery를 연결하는 절차입니다.

공개된 운영 이미지가 있다고 가정하지 않습니다. 실제 학습 이미지와 System controller/member 이미지는 접근 가능한 registry에 직접 build/push한 뒤 `docs/installation.md`와 `docs/component-images.md` 절차로 배포해야 합니다. System은 단일 `SYSTEM_IMAGE`가 아니라 `vm-spot-risk-collector`, `policy-manager`, `checkpoint-coordinator`, `spot-recovery-controller`, `training-runtime-collector`, `spot-watcher` 6개 이미지로 배포합니다. 이 문서는 예시 manifest와 실행 순서를 제공하며, 실제 클러스터 적용 테스트를 수행했다고 주장하지 않습니다.

## 관련 문서

- [System installation guide](../docs/installation.md)
- [Stateful-Migration-Operator-with-PV README](https://github.com/GProjectdev/Stateful-Migration-Operator-with-PV)
- [Stateful suspension gate](https://github.com/GProjectdev/Stateful-Migration-Operator-with-PV/blob/main/docs/suspension.md)
- [StatefulSet 2 Pod integration guide](https://github.com/GProjectdev/Stateful-Migration-Operator-with-PV/blob/main/docs/two-replica-migration-guide.md)
- [PV-Migration-System README](https://github.com/GProjectdev/Karmada_with_PVMigration)

## 범위와 책임

- 사용자가 workload StatefulSet, Service, ConfigMap, FluidCR runtime hook, checkpoint/archive 스크립트, workload PropagationPolicy placement를 직접 관리합니다.
- MGMT controller는 Karmada API만 사용합니다. member kubeconfig로 workload placement를 바꾸거나 PV controller 배치, node 삭제를 수행하지 않습니다.
- `TrainingRuntime` member collector는 StatefulSet Pod IP의 `8298` 포트에서 `GET /runtime`을 호출합니다.
- Karmada가 member에 전파한 StatefulSet UID는 원본 Karmada UID와 다를 수 있습니다. 전파되는 StatefulSet에는 반드시 `training.dcnlab.com/workload-uid=<KARMADA_STS_UID>` label이 있어야 합니다.
- source fencing은 자동이 아닙니다. 실제 source writer 정지/fence와 ResourceBinding dispatch pause가 모두 필요합니다. dispatch pause만으로 이미 실행 중인 writer가 멈추지 않습니다.
- `RestoreRequest.status.phase=Verified`와 checkpoint/restore 증거가 확인되기 전에는 old `NodeProvision` 또는 source node를 삭제하지 않습니다.
- `SpotRecovery`는 검증된 RestoreRequest 증거를 참조해서 AWS Spot old NodeProvision cleanup을 수행하는 operation입니다. `SpotRecovery.status.phase=Completed`는 cleanup 종료 상태일 수 있지만 restore 검증 입력으로 사용하지 않습니다.
- split MGMT controller는 기존 shared `hybridspot-karmada-kubeconfig` Secret과 Karmada RBAC identity를 사용합니다. 컴포넌트 분리는 보안 격리가 아니며 workload placement 권한을 추가하지 않습니다.

## 사전 변수

이 runbook은 하나의 일관된 demo로 `source=aws`, `target=onprem`을 사용합니다. recovery controller의 old Spot node cleanup 계약상 `SpotRecovery.spec.sourceCluster`는 반드시 `aws`여야 합니다.

```bash
export KARMADA_KUBECONFIG=/secure/karmada-admin.kubeconfig
export SOURCE_KUBECONFIG=/secure/aws-member.kubeconfig
export TARGET_KUBECONFIG=/secure/onprem-member.kubeconfig
export NS=training-demo
export APP=ddp-trainer
export POLICY_NAME=${APP}-policy
export RUNTIME_NAME=${POLICY_NAME}-runtime
export SOURCE_CLUSTER=aws
export TARGET_CLUSTER=onprem
export AWS_CLUSTER=aws
```

`TrainingPolicy`의 기본 runtimeRef 이름은 `<policy-name>-runtime`입니다. 샘플은 명시적으로 `runtimeRef.name=$RUNTIME_NAME`을 넣고, `SpotRecovery.spec.trainingRuntimeRef.name`도 같은 값을 참조합니다.

## 1. StatefulSet workload 준비

먼저 Stateful Migration 문서대로 실제 2 replica 학습 StatefulSet과 runtime endpoint를 준비합니다. 각 Pod는 `http://$POD_IP:8298/runtime`에서 다음 형태의 JSON을 30초 이내 timestamp로 반환해야 합니다.

```json
{
  "globalStep": 120,
  "checkpointID": "ckpt-000120",
  "rank": 0,
  "worldSize": 2,
  "observedAt": "2026-09-26T12:00:00Z",
  "state": "Running"
}
```

Karmada에서 StatefulSet 원본 UID를 확인하고 StatefulSet metadata label로 고정합니다. 이 label은 template label이 아니라 StatefulSet 객체 label입니다.

```bash
export WORKLOAD_UID="$(
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get sts "$APP" \
    -o jsonpath='{.metadata.uid}'
)"

kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" label sts "$APP" \
  "training.dcnlab.com/workload-uid=$WORKLOAD_UID" --overwrite
```

초기 workload PropagationPolicy는 source cluster만 선택합니다. 예시는 `config/samples/20-workload-propagationpolicy.yaml`에 있습니다. `TrainingRuntime`도 source member collector가 관측할 수 있도록 source cluster로 전파합니다. 예시는 `config/samples/21-training-runtime-propagationpolicy.yaml`에 있습니다.

```bash
envsubst < config/samples/20-workload-propagationpolicy.yaml | \
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f -
```

## 2. TrainingRuntime, Risk, Policy 적용

샘플은 Karmada API에 적용합니다.

```bash
envsubst < config/samples/00-namespace.yaml | \
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f -

envsubst < config/samples/10-training-runtime.yaml | \
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f -

envsubst < config/samples/21-training-runtime-propagationpolicy.yaml | \
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f -

envsubst < config/samples/11-spot-risk-profile-static.yaml | \
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f -

envsubst < config/samples/12-training-policy.yaml | \
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f -
```

정상 흐름에서 확인할 상태입니다.

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingruntime "$RUNTIME_NAME" -o yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get spotriskprofile "$APP-risk" -o yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingpolicy "$POLICY_NAME" -o yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get nodeprovisions
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get propagationpolicy
```

주의: `SpotRiskProfile.spec.staticLambdaPerHour`는 통제된 실험용입니다. 실제 운영에서는 `config/samples/11-spot-risk-profile-https.yaml`의 `aws-risk-feed`를 복사해 endpoint `https://risk-feed.example.invalid/aws/ap-northeast-2/g4dn.xlarge`를 실제 HTTPS feed로 바꾸고, `TrainingPolicy`의 risk reference도 `aws-risk-feed`를 가리키게 수정하세요. static sample과 HTTPS sample을 함께 apply해도 Policy가 자동 전환되지 않습니다.

GPU DDP demo는 `TrainingPolicy.spec.capacity.aws.hardwareType=gpu`와 `nodeLabel=gpu`를 명시합니다. System은 GPU-ready AMI, NVIDIA driver, CUDA runtime, container image dependency를 제공하지 않으므로 운영자가 AMI와 workload image에 맞춰 준비해야 합니다.

## 3. Checkpoint와 durable export 확인

Policy의 checkpoint status가 `checkpoint_created`가 되면 MGMT가 `FluidCRMigration`과 source 고정 PropagationPolicy를 생성합니다. 이 단계는 workload placement를 바꾸지 않습니다.

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get fluidcrmigrations
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingpolicy "$POLICY_NAME" \
  -o jsonpath='{.status.checkpoint.reason}{" "}{.status.checkpoint.lastMigrationName}{"\n"}'
```

RestorePlan을 만들기 전 durable checkpoint 증거가 필요합니다. `FluidCRMigration`의 `Completed` phase만으로는 충분하지 않습니다. 모든 rank의 archive 파일에 durable marker가 있어야 합니다.

Checkpoint ID는 Pod status field가 아니라 `FluidCRMigration` annotation `training.dcnlab.com/checkpoint-id`에서 읽습니다. checkpoint pod evidence는 Karmada RIC가 모은 flat `status.clusters[].pods[]`에 있습니다. 이 목록은 `.status.clusters[].status.pods[]`가 아니며, Pod 이름 field는 `podName`입니다.

필수 marker:

- `status.clusters[].pods[].checkpointFiles[].sha256`
- `status.clusters[].pods[].checkpointFiles[].durableRef`가 `file-store:`로 시작
- `status.clusters[].pods[].checkpointFiles[].exportedAt`

member artifact exporter는 completed `FluidCRMigration`을 감시하고 RestorePlan 생성 전에 kubelet archive를 shared checkpoint PVC backend로 업로드합니다. durable key는 `file-store:<namespace>/sha256/<digest>` 형식입니다. 경로, digest, durableRef는 Karmada object/status에서 읽습니다. source node에 SSH해서 hash를 다시 계산하지 마세요. export 이후 source node가 사라져도 durable backend에서 download할 수 있어야 합니다.

```bash
export LAST_MIGRATION="$(
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingpolicy "$POLICY_NAME" \
    -o jsonpath='{.status.checkpoint.lastMigrationName}'
)"
export CHECKPOINT_ID="$(
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get fluidcrmigration "$LAST_MIGRATION" \
    -o jsonpath='{.metadata.annotations.training\.dcnlab\.com/checkpoint-id}'
)"

kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get fluidcrmigration "$LAST_MIGRATION" \
  -o jsonpath='{range .status.clusters[?(@.clusterName=="'"$SOURCE_CLUSTER"'")].pods[*]}{.podName}{"\n"}{range .checkpointFiles[*]}{"  "}{.sha256}{" "}{.durableRef}{" "}{.exportedAt}{"\n"}{end}{end}'
```

App FluidCR의 immutable round 파일은 별도의 shared checkpoint PVC에 보관해야 합니다. source와 target은 같은 durable backend/shared PVC를 사용해야 하며, RestorePlan/RestoreRequest는 node-local archive 경로가 아니라 durableRef와 digest 증거를 기준으로 작성합니다.

## 4. Fence, dispatch pause, RestorePlan 준비

Restore Verified는 target placement보다 앞설 수 없습니다. 올바른 흐름은 다음 순서입니다.

1. checkpoint 생성 및 durable export 완료
2. 실제 source writer stop/fence 완료
3. ResourceBinding dispatch pause 완료
4. 필요한 경우 PV-Migration-System `PVMigration.status.phase=Completed` 및 모든 Work `applied=true`, `detached=true` 확인
5. RestorePlan `Prepared` 확인
6. 사용자가 workload placement를 target으로 변경하되, dispatch는 계속 paused 상태로 유지
7. suspension release
8. target restore 진행
9. RestoreRequest `Verified`
10. SpotRecovery cleanup 생성

`dispatch pause`는 새 target start를 막는 Karmada/Stateful gate입니다. 이미 source에서 실행 중인 writer를 멈추는 증거가 아니므로 source writer stopped/fenced 확인과 별개로 기록해야 합니다.

```bash
# 예: 실제 suspension/pause 명령은 Stateful suspension 문서와 현재 ResourceBinding 이름에 맞춰 실행합니다.
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get resourcebinding
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get restoreplan
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get pvmigration
```

## 5. 사용자 주도 target placement와 suspension release

RestorePlan이 `Prepared`이고 source writer fence와 RB dispatch pause가 모두 확인된 뒤, 사용자가 workload PropagationPolicy를 target cluster로 바꿉니다. 이 시점에도 dispatch는 paused 상태여야 합니다.

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" patch propagationpolicy "$APP-source-only" \
  --type=merge \
  -p "{\"spec\":{\"placement\":{\"clusterAffinity\":{\"clusterNames\":[\"$TARGET_CLUSTER\"]}}}}"
```

그 다음 Stateful suspension gate를 release해서 target restore가 진행되게 합니다. release 명령은 Stateful Migration 문서의 현재 suspension 계약을 따릅니다. release 후 target Pod에서 `/runtime`이 2 rank 모두 `Running`, 같은 `checkpointID`, 같은 `worldSize=2`를 보고하는지 확인합니다.

## 6. RestoreRequest Verified 이후 SpotRecovery cleanup

target restore가 실제로 진행되고 검증 controller가 `RestoreRequest.status.phase=Verified`를 기록한 뒤에만 `SpotRecovery`를 생성합니다. `SpotRecovery.spec.requestUID`는 새로 만든 operation UID가 아니라 `RestoreRequest.metadata.uid`와 같아야 합니다. controller는 `requestUID == restoreRequestRef.uid`를 강제합니다.

`SpotRecovery`가 삭제할 old Spot node와 optional replacement node는 UID로 고정해서 참조합니다. NodeProvision RIC 증거는 `status.clusters[]`가 아니라 top-level `status.observedCluster`와 `status.spot`입니다.

```bash
export POLICY_UID="$(
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingpolicy "$POLICY_NAME" \
    -o jsonpath='{.metadata.uid}'
)"
export POLICY_GENERATION="$(
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingpolicy "$POLICY_NAME" \
    -o jsonpath='{.metadata.generation}'
)"
export RUNTIME_UID="$(
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingruntime "$RUNTIME_NAME" \
    -o jsonpath='{.metadata.uid}'
)"
export RESTORE_REQUEST_NAME="__REPLACE_WITH_RESTORE_REQUEST_NAME__"
export RESTORE_REQUEST_UID="$(
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get restorerequest "$RESTORE_REQUEST_NAME" \
    -o jsonpath='{.metadata.uid}'
)"
export RESTORE_REQUEST_GENERATION="$(
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get restorerequest "$RESTORE_REQUEST_NAME" \
    -o jsonpath='{.metadata.generation}'
)"
export EVENT_ID="__REPLACE_WITH_OLD_NODEPROVISION_SPOT_EVENT_ID__"
export OPERATION="__REPLACE_WITH_RECOVERY_OPERATION_NAME__"
export OLD_NODEPROVISION_NAME="__REPLACE_OLD_NODEPROVISION_NAME__"
export OLD_NODEPROVISION_UID="__REPLACE_OLD_NODEPROVISION_UID__"
export REPLACEMENT_NODEPROVISION_NAME="__OPTIONAL_REPLACEMENT_NODEPROVISION_NAME__"
export REPLACEMENT_NODEPROVISION_UID="__OPTIONAL_REPLACEMENT_NODEPROVISION_UID__"
```

placeholder를 모두 바꾼 뒤 적용합니다. replacement가 없는 on-prem target 또는 이미 capacity가 있는 경우에는 sample에서 `replacementNodeProvisionRef` block을 제거하세요.

```bash
envsubst < config/samples/30-spotrecovery-restore-verified-placeholder.yaml | \
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f -
```

controller가 기대하는 restore 증거입니다.

- `SpotRecovery.spec.sourceCluster=aws`
- `SpotRecovery.spec.targetCluster=$TARGET_CLUSTER`, source와 target이 서로 다름
- `SpotRecovery.spec.requestUID == SpotRecovery.spec.restoreRequestRef.uid`
- `RestoreRequest.status.phase=Verified`
- `RestoreRequest.status.observedGeneration`이 `SpotRecovery.spec.restoreRequestRef.generation`과 일치
- `RestoreRequest.spec.sourceFenced=true`
- `RestoreRequest.spec.workloadRef.uid`와 `SpotRecovery.spec.workloadRef.uid` 일치
- `RestoreRequest.spec.checkpointRef.checkpointID`와 `SpotRecovery.spec.checkpointID` 일치
- `RestoreRequest.status.verification.requestUID/checkpointID/sourceCluster/targetCluster/trainingRuntimeRef/verifiedAt` 존재 및 일치
- `RestoreRequest.spec.pods[].sourceNode`에 old NodeProvision node name 존재
- old `NodeProvision.status.observedCluster=aws`
- old `NodeProvision.status.spot.instanceID == status.instanceId`
- old `NodeProvision.status.spot.eventID == SpotRecovery.spec.eventID`
- old `NodeProvision.status.spot.atRisk=true`
- old `NodeProvision.status.spot.signalType`이 `InterruptionNotice` 또는 `RebalanceRecommendation`
- replacement ref가 있으면 replacement `NodeProvision.status.phase=Ready`, `status.instanceId` 존재

controller는 별도 spot source field를 요구하지 않습니다. `SpotRecovery.status`는 controller-owned이며 `Pending`, `CleanupRequested`, `Completed`, `Rejected`로 진행됩니다. `Completed`는 restore 검증이 아니라 삭제/cleanup operation의 결과입니다.

## 7. old NodeProvision 삭제 조건

삭제 전 다음 조건을 모두 만족해야 합니다.

- `RestoreRequest.status.phase=Verified`
- `SpotRecovery.spec.policyRef.uid/generation`이 현재 `TrainingPolicy`와 일치
- `SpotRecovery.spec.trainingRuntimeRef.name/uid`가 현재 `TrainingRuntime`과 일치
- `SpotRecovery.spec.requestUID`가 현재 `RestoreRequest.metadata.uid`와 일치
- `SpotRecovery.spec.operation`이 현재 교체 operation과 일치
- old/replacement `NodeProvision` name/UID가 현재 객체와 일치
- old NodeProvision top-level `status.observedCluster`, `status.instanceId`, `status.spot.eventID`, `status.spot.instanceID`, `status.spot.atRisk`, `status.spot.signalType` RIC 증거가 일치
- source writer stopped/fenced 수동 확인 완료
- RB dispatch pause, target placement while paused, suspension release 순서 기록 완료
- 필요한 경우 PV migration 증거가 현재 migration UID/generation/planHash/source PVC/target PVC와 일치

이 문서는 실행 가능한 예시와 controller 계약을 맞춘 통합 절차입니다. placeholder 값, registry 이미지, 실제 Stateful Migration/PV Migration 준비는 환경별로 채워야 합니다.
## 8. SpotRecovery record retention safety

Keep each `SpotRecovery` CR for the lifetime of its `TrainingPolicy`. The immutable `spec.oldNodeProvisionRef` is the retirement marker that prevents the policy reconciler from recreating the original generated NodeProvision slot; this applies even when the recovery status is `Rejected`. Deleting the `SpotRecovery` while the policy is still active removes that retirement evidence and can allow the original slot to be considered creatable again.

Do not delete a `SpotRecovery` CR just to retry the same cleanup. Create a new recovery operation with corrected evidence when needed, and retain the old record. For decommissioning, remove or disable the owning `TrainingPolicy` first, wait for policy reconciliation to stop, and only then remove historical recovery records according to the cluster retention policy.
