# 고정 namespace 체크포인트·스케줄 릴리스 가이드 (2026-10-01)

> 이 문서는 [Partial 복원 수정본 빌드·재배포 가이드](partial-resume-release-20260930.md)를 **고정 namespace `fluidcr-realign-121040` 절차로 대체**한다. 이전 문서는 새 namespace와 이전 체크포인트 운용을 전제로 하므로 이번 실행에는 사용하지 않는다.

## 범위와 중단 조건

이번 문서는 2026-09-30에 아직 수행하지 않은 배포 변경과 2026-10-01 기준 실제 구현된 정기 체크포인트 스케줄 계약을 함께 적용한다. 실행 대상 namespace는 기존 `fluidcr-realign-121040` 하나뿐이다. 새 namespace, 새 PVC를 만들지 않는다. 기존 PVC와 checkpoint 데이터는 보존한다.


`no new namespace`는 `no new operation`이 아니다. 컨트롤러가 현재 source UID에 묶어 자동 생성하는 새 `SpotReplacement`, `RestoreRequest`, `FluidCRMigration` operation은 허용한다. 단, 과거 operation ID를 재사용하거나 사람이 예전 CR status/finalizer를 고쳐 재사용하지 않는다.

모든 명령은 MGMT Bash에서 한 줄씩 실행한다. shell function, 반복문, `kubectl edit`, `--watch`를 사용하지 않는다. 명령이 실패하면 다음 단계로 넘어가지 않는다.

금지 사항:

- PVC, Node, finalizer, lock, status를 눈가림으로 삭제·패치하지 않는다.
- 실패한 CR을 성공 상태로 바꾸지 않는다.
- Secret 값을 파일로 저장하지 않는다.
- capability 라벨을 수동으로 붙여 인증을 우회하지 않는다.
- `Failed` 상태의 과거 replacement를 안전하다고 간주해 자동 group 작업을 진행하지 않는다.

사용자가 수동 정리를 이미 했을 수 있다. 백업 단계에서 필수 CR, PVC, StatefulSet이 없으면 새 CR을 만들어 맞추지 말고 중단한다. 이 경우 같은 namespace에서 필요한 리소스 manifest를 먼저 복구해야 하며, 새 실험 namespace를 만들지 않는다.

## 함께 적용되는 9월 30일 수정

- 체크포인트 요청 전 모든 rank의 GPU worker 등록 준비를 확인한다.
- survivor 재개 승인과 실제 DDP 재결합 완료 증거를 분리한다.
- 실제 로드한 checkpointID는 보존하고, 이번 복원 참여는 별도의 `survivorResume` 증거로 확인한다.
- 대상 rank와 survivor의 복원 증거 및 두 번의 관측 사이 step 증가를 확인한다.
- StatefulSet template의 workload UID 라벨을 보완하고 `OnDelete`로 survivor 자동 롤링을 막는다.
- Provisioner의 NVIDIA 라이브러리 경로 설정과 신규 노드 복원 기능 인증을 적용한다.

이 내용 때문에 System/Stateful 이미지뿐 아니라 FluidCR payload와 Provisioner도 아래 절차대로 함께 재배포한다. 기존 Pod에는 새 payload가 자동 주입되지 않으므로 유지보수 후 새로 생성된 Pod로 검증한다.

## 1. 실행 전 사전 조건

MGMT shell에서 kubeconfig와 registry 상태를 먼저 확인한다. kubeconfig 경로와 환경변수가 비어 있거나 파일이 없으면 중단한다. Docker Hub login이 없으면 `buildah login docker.io`를 대화형으로 실행하되 token/password 값을 파일이나 shell history에 남기지 않는다.

```bash
printf '%s\n' "KARMADA_KUBECONFIG=$KARMADA_KUBECONFIG"
printf '%s\n' "AWS_KUBECONFIG=$AWS_KUBECONFIG"
printf '%s\n' "MGMT_KUBECONFIG=$MGMT_KUBECONFIG"
test -n "${KARMADA_KUBECONFIG:?}"
test -n "${AWS_KUBECONFIG:?}"
test -n "${MGMT_KUBECONFIG:?}"
test -f "${KARMADA_KUBECONFIG:?}"
test -f "${AWS_KUBECONFIG:?}"
test -f "${MGMT_KUBECONFIG:?}"
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" version --client=true
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" version --client=true
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" version --client=true
buildah login --get-login docker.io
buildah login docker.io
```

`buildah login --get-login docker.io`가 성공하면 마지막 `buildah login docker.io`는 실행하지 않는다. 실패하거나 login 사용자가 없을 때만 대화형으로 로그인한다. 이 문서의 namespace 제한은 실험 namespace를 새로 만들지 말라는 뜻이다. `System/config/karmada/access.yaml` 안의 기존 `hybridspot-system` Namespace manifest를 idempotent하게 apply하는 것은 허용된다.
## 2. 저장소와 브랜치 확인

MGMT의 저장소 경로와 브랜치는 고정한다.

- System: `/root/hybridspot-validation/System`
- Stateful: `/root/hybridspot-validation/stateful`
- FluidCR: `/root/hybridspot-validation/fluidcr`
- Provisioner: `/root/hybridspot-validation/provisioner`
- Branch: `restore-automation-20260928`

```bash
git -C /root/hybridspot-validation/System status --short --branch
git -C /root/hybridspot-validation/stateful status --short --branch
git -C /root/hybridspot-validation/fluidcr status --short --branch
git -C /root/hybridspot-validation/provisioner status --short --branch
git -C /root/hybridspot-validation/System fetch origin restore-automation-20260928
git -C /root/hybridspot-validation/System merge --ff-only origin/restore-automation-20260928
git -C /root/hybridspot-validation/stateful fetch origin restore-automation-20260928
git -C /root/hybridspot-validation/stateful merge --ff-only origin/restore-automation-20260928
git -C /root/hybridspot-validation/fluidcr fetch origin restore-automation-20260928
git -C /root/hybridspot-validation/fluidcr merge --ff-only origin/restore-automation-20260928
git -C /root/hybridspot-validation/provisioner fetch origin restore-automation-20260928
git -C /root/hybridspot-validation/provisioner merge --ff-only origin/restore-automation-20260928
git -C /root/hybridspot-validation/System rev-parse --abbrev-ref HEAD
git -C /root/hybridspot-validation/stateful rev-parse --abbrev-ref HEAD
git -C /root/hybridspot-validation/fluidcr rev-parse --abbrev-ref HEAD
git -C /root/hybridspot-validation/provisioner rev-parse --abbrev-ref HEAD
```

네 저장소가 모두 `restore-automation-20260928`이어야 한다. 로컬 변경이 있으면 덮어쓰지 않는다.

## 3. 백업과 필수 객체 존재 확인

백업 파일에는 배포 인자, 이미지, CR status가 들어간다. Secret object나 Secret data는 저장하지 않는다. 아래 `get` 명령 중 하나라도 `NotFound`이면 사용자가 직접 CR/PVC/StatefulSet을 삭제했을 가능성이 있으므로 즉시 중단한다. 누락된 리소스는 같은 namespace의 실제 manifest로 복구해야 하며, 새 namespace나 과거 이름을 임의로 만들지 않는다.

```bash
export NS=fluidcr-realign-121040
export POLICY=trainer-realign
export RISK=trainer-risk
export APP=trainer
export RELEASE=checkpoint-schedule-$(date -u +%Y%m%dT%H%M%SZ)
export EVIDENCE=/root/hybridspot-validation/evidence/$RELEASE
export IMAGES=$EVIDENCE/images
mkdir -p "$IMAGES"
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get trainingpolicy "$POLICY" -o yaml > "$EVIDENCE/trainingpolicy-before.yaml"
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get spotriskprofile "$RISK" -o yaml > "$EVIDENCE/spotriskprofile-before.yaml"
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get statefulset "$APP" -o yaml > "$EVIDENCE/karmada-statefulset-before.yaml"
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get trainingruntime -o yaml > "$EVIDENCE/trainingruntime-before.yaml"
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get spotreplacements,restorerequests,spotrecoveries -o yaml > "$EVIDENCE/recovery-before.yaml"
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get fluidcrmigrations -o yaml > "$EVIDENCE/karmada-fluidcrmigrations-before.yaml"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get statefulset "$APP" -o yaml > "$EVIDENCE/aws-statefulset-before.yaml"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get pods,pvc,nodeprovisions,fluidcrmigrations,nodeprovisionnetconfigs -o yaml > "$EVIDENCE/aws-workload-before.yaml"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system get deployments -o yaml > "$EVIDENCE/mgmt-hybrid-before.yaml"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n stateful-migration-system get deployment stateful-management -o yaml > "$EVIDENCE/mgmt-stateful-before.yaml"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n stateful-migration-system get deployments,daemonsets -o yaml > "$EVIDENCE/aws-stateful-before.yaml"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n hybridspot-system get deployment training-runtime-collector -o yaml > "$EVIDENCE/aws-runtime-before.yaml"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n remote-cluster-provisioner-system get deployment remote-cluster-provisioner -o yaml > "$EVIDENCE/aws-provisioner-before.yaml"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n fluidcr-system get deployment fluidcr-webhook -o json > "$EVIDENCE/webhook-before.json"
```

Secret은 이름만 확인한다.

```bash
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get trainingpolicy "$POLICY" -o json | jq '{credentialsRef:.spec.capacity.aws.credentialsRef, workloadRef:.spec.workloadRef, runtimeRef:.spec.runtimeRef, checkpoint:.spec.checkpoint, replacement:.spec.replacement}'
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get secrets -o custom-columns='NAME:.metadata.name,TYPE:.type'
```

## 4. 2VM mixed 전제와 유지보수 게이트

이번 재개 전제는 2-worker mixed policy다. `spec.targetWorkers=2`, `spec.expectedWorldSize=2`, `spec.policy.minOnDemand=1`, workload UID 라벨 `training.dcnlab.com/workload-uid`가 실제 StatefulSet origin UID와 맞아야 한다. 코드 기준 workload UID label key는 `training.dcnlab.com/workload-uid`다. `targetWorkers`는 CRD상 immutable이므로 여기서는 검증만 하고 patch하지 않는다.

```bash
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get trainingpolicy "$POLICY" -o json | jq -e '.spec.policy.minOnDemand == 1 and .spec.targetWorkers == 2 and .spec.expectedWorldSize == 2'
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get statefulset "$APP" -o json | jq -e '.spec.template.metadata.labels["training.dcnlab.com/workload-uid"] == .metadata.uid'
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get trainingpolicy "$POLICY" -o json | jq '{targetWorkers:.spec.targetWorkers,expectedWorldSize:.spec.expectedWorldSize,policy:.spec.policy,workloadRef:.spec.workloadRef,runtimeRef:.spec.runtimeRef,checkpoint:.spec.checkpoint}'
```

공유 컨트롤러 교체는 학습 Pod가 없고 inflight restore/trainer가 없는 시간에만 진행한다. `suspend`는 새 intent를 막는 장치이지 진행 중인 작업 삭제 기능이 아니다. 초기 static risk는 2VM mixed 재개 전제상 0이 아니라 낮은 위험값 `0.05`로 둔다.

```bash
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" annotate trainingpolicy "$POLICY" training.dcnlab.com/suspend=true --overwrite
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" patch trainingpolicy "$POLICY" --type=merge -p '{"spec":{"policy":{"minOnDemand":1},"replacement":{"enabled":false}}}'
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" patch spotriskprofile "$RISK" --type=merge -p '{"spec":{"staticLambdaPerHour":0.05}}'
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get trainingpolicy "$POLICY" -o json | jq '{annotations:.metadata.annotations,targetWorkers:.spec.targetWorkers,expectedWorldSize:.spec.expectedWorldSize,policy:.spec.policy,replacement:.spec.replacement,riskProfile:.spec.riskProfileRef,status:.status}'
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get spotriskprofile "$RISK" -o json | jq '.spec'
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get spotreplacements,restorerequests,spotrecoveries -o custom-columns='KIND:.kind,NAME:.metadata.name,PHASE:.status.phase,MESSAGE:.status.message'
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get pod -o wide
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get fluidcrmigrations -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,GEN:.metadata.generation'
```

`spotreplacements`에 `Failed`, `AwaitingPartialCheckpoint`, `Running`, `RestoreReady` 등이 있으면 이 문서만으로 group fallback을 시작하지 않는다. 보수적으로 중단하고 사용자가 정리한다.

## 5. 이미지 빌드와 새 digest

base payload를 먼저 push하고, 그 digest로 overlay를 새로 빌드한다. 과거 digest를 재사용하지 않는다. 계획된 full fallback 검증까지 할 경우 `group-control`도 이번 release digest로 빌드한다. 단, `Failed` 과거 operation이나 API error만으로 group fallback을 시작하지 않는다.

```bash
git -C /root/hybridspot-validation/System rev-parse HEAD > "$IMAGES/system.commit"
git -C /root/hybridspot-validation/stateful rev-parse HEAD > "$IMAGES/stateful.commit"
git -C /root/hybridspot-validation/fluidcr rev-parse HEAD > "$IMAGES/fluidcr.commit"
git -C /root/hybridspot-validation/provisioner rev-parse HEAD > "$IMAGES/provisioner.commit"
buildah bud --arch amd64 --build-arg COMPONENT=policy-manager -t "docker.io/jeongseungjun/hybrid-spot-vm-system:policy-manager-$RELEASE" /root/hybridspot-validation/System
buildah push --digestfile "$IMAGES/policy-manager.digest" "docker.io/jeongseungjun/hybrid-spot-vm-system:policy-manager-$RELEASE" "docker://docker.io/jeongseungjun/hybrid-spot-vm-system:policy-manager-$RELEASE"
buildah bud --arch amd64 --build-arg COMPONENT=checkpoint-coordinator -t "docker.io/jeongseungjun/hybrid-spot-vm-system:checkpoint-coordinator-$RELEASE" /root/hybridspot-validation/System
buildah push --digestfile "$IMAGES/checkpoint-coordinator.digest" "docker.io/jeongseungjun/hybrid-spot-vm-system:checkpoint-coordinator-$RELEASE" "docker://docker.io/jeongseungjun/hybrid-spot-vm-system:checkpoint-coordinator-$RELEASE"
buildah bud --arch amd64 --build-arg COMPONENT=spot-recovery-controller -t "docker.io/jeongseungjun/hybrid-spot-vm-system:spot-recovery-controller-$RELEASE" /root/hybridspot-validation/System
buildah push --digestfile "$IMAGES/spot-recovery-controller.digest" "docker.io/jeongseungjun/hybrid-spot-vm-system:spot-recovery-controller-$RELEASE" "docker://docker.io/jeongseungjun/hybrid-spot-vm-system:spot-recovery-controller-$RELEASE"
buildah bud --arch amd64 --build-arg COMPONENT=placement-webhook -t "docker.io/jeongseungjun/hybrid-spot-vm-system:placement-webhook-$RELEASE" /root/hybridspot-validation/System
buildah push --digestfile "$IMAGES/placement-webhook.digest" "docker.io/jeongseungjun/hybrid-spot-vm-system:placement-webhook-$RELEASE" "docker://docker.io/jeongseungjun/hybrid-spot-vm-system:placement-webhook-$RELEASE"
buildah bud --arch amd64 --build-arg COMPONENT=training-runtime-collector -t "docker.io/jeongseungjun/hybrid-spot-vm-system:training-runtime-collector-$RELEASE" /root/hybridspot-validation/System
buildah push --digestfile "$IMAGES/training-runtime-collector.digest" "docker.io/jeongseungjun/hybrid-spot-vm-system:training-runtime-collector-$RELEASE" "docker://docker.io/jeongseungjun/hybrid-spot-vm-system:training-runtime-collector-$RELEASE"
buildah bud --arch amd64 -t "docker.io/jeongseungjun/stateful-migration-operator:$RELEASE" /root/hybridspot-validation/stateful
buildah push --digestfile "$IMAGES/stateful.digest" "docker.io/jeongseungjun/stateful-migration-operator:$RELEASE" "docker://docker.io/jeongseungjun/stateful-migration-operator:$RELEASE"
buildah bud --arch amd64 -t "docker.io/jeongseungjun/my-publiccloudvm-provisioner:$RELEASE" /root/hybridspot-validation/provisioner
buildah push --digestfile "$IMAGES/provisioner.digest" "docker.io/jeongseungjun/my-publiccloudvm-provisioner:$RELEASE" "docker://docker.io/jeongseungjun/my-publiccloudvm-provisioner:$RELEASE"
buildah bud --arch amd64 -f /root/hybridspot-validation/fluidcr/Dockerfile.payload -t "docker.io/jeongseungjun/myfluidcr-operator:payload-base-$RELEASE" /root/hybridspot-validation/fluidcr
buildah push --digestfile "$IMAGES/payload-base.digest" "docker.io/jeongseungjun/myfluidcr-operator:payload-base-$RELEASE" "docker://docker.io/jeongseungjun/myfluidcr-operator:payload-base-$RELEASE"
cat "$IMAGES/payload-base.digest"
buildah bud --arch amd64 -f /root/hybridspot-validation/stateful/Dockerfile.payload-overlay --build-arg "FLUIDCR_PAYLOAD_IMAGE=docker.io/jeongseungjun/myfluidcr-operator@$(cat "$IMAGES/payload-base.digest")" -t "docker.io/jeongseungjun/myfluidcr-operator:payload-stateful-$RELEASE" /root/hybridspot-validation/stateful
buildah push --digestfile "$IMAGES/payload-stateful.digest" "docker.io/jeongseungjun/myfluidcr-operator:payload-stateful-$RELEASE" "docker://docker.io/jeongseungjun/myfluidcr-operator:payload-stateful-$RELEASE"
buildah bud --arch amd64 -f /root/hybridspot-validation/fluidcr/Dockerfile.group-control -t "docker.io/jeongseungjun/myfluidcr-operator:group-control-$RELEASE" /root/hybridspot-validation/fluidcr
buildah push --digestfile "$IMAGES/group-control.digest" "docker.io/jeongseungjun/myfluidcr-operator:group-control-$RELEASE" "docker://docker.io/jeongseungjun/myfluidcr-operator:group-control-$RELEASE"
cat "$IMAGES/policy-manager.digest" "$IMAGES/checkpoint-coordinator.digest" "$IMAGES/spot-recovery-controller.digest" "$IMAGES/placement-webhook.digest" "$IMAGES/training-runtime-collector.digest" "$IMAGES/stateful.digest" "$IMAGES/provisioner.digest" "$IMAGES/payload-stateful.digest" "$IMAGES/group-control.digest"
```

출력된 모든 digest가 `sha256:`으로 시작해야 한다.

## 6. CRD, RIC, management RBAC 적용 순서

CRD는 컨트롤러보다 먼저, 그리고 Karmada API와 AWS member API 양쪽에 적용한다. Stateful 실제 스케줄 계약은 `fluidcrmigrations.yaml`에 들어 있으므로 System CRD와 함께 먼저 적용한다. RIC는 Karmada에만 적용한다. `System/config/karmada/access.yaml`은 schedule parent/status mirror를 위해 `fluidcrmigrations/status` patch/update 권한을 포함하므로 반드시 적용한다.

```bash
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" apply -f /root/hybridspot-validation/System/config/crd/trainingruntimes.yaml
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" apply -f /root/hybridspot-validation/System/config/crd/trainingruntimes.yaml
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" apply -f /root/hybridspot-validation/System/config/crd/trainingpolicies.yaml
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" apply -f /root/hybridspot-validation/System/config/crd/trainingpolicies.yaml
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" apply -f /root/hybridspot-validation/System/config/crd/spotriskprofiles.yaml
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" apply -f /root/hybridspot-validation/System/config/crd/spotriskprofiles.yaml
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" apply -f /root/hybridspot-validation/System/config/crd/spotreplacements.yaml
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" apply -f /root/hybridspot-validation/System/config/crd/spotreplacements.yaml
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" apply -f /root/hybridspot-validation/System/config/crd/spotrecoveries.yaml
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" apply -f /root/hybridspot-validation/System/config/crd/spotrecoveries.yaml
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" apply -f /root/hybridspot-validation/stateful/config/crd/fluidcrmigrations.yaml
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" apply -f /root/hybridspot-validation/stateful/config/crd/fluidcrmigrations.yaml
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" apply -f /root/hybridspot-validation/stateful/config/crd/restorerequests.yaml
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" apply -f /root/hybridspot-validation/stateful/config/crd/restorerequests.yaml
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" apply -f /root/hybridspot-validation/stateful/config/crd/restoreplans.yaml
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" apply -f /root/hybridspot-validation/stateful/config/crd/restoreplans.yaml
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" apply -f /root/hybridspot-validation/System/config/karmada/runtime-interpreter.yaml
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" apply -k /root/hybridspot-validation/stateful/config/karmada/ric
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" apply -f /root/hybridspot-validation/System/config/karmada/access.yaml
```

적용 후 API와 RIC를 확인한다.

```bash
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" get crd trainingpolicies.training.dcnlab.com trainingruntimes.training.dcnlab.com spotreplacements.training.dcnlab.com fluidcrmigrations.fluidcr.dcnlab.com restorerequests.migration.dcnlab.com restoreplans.migration.dcnlab.com
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" get crd trainingpolicies.training.dcnlab.com trainingruntimes.training.dcnlab.com spotreplacements.training.dcnlab.com fluidcrmigrations.fluidcr.dcnlab.com restorerequests.migration.dcnlab.com restoreplans.migration.dcnlab.com
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" get resourceinterpretercustomizations -o custom-columns='NAME:.metadata.name,API:.spec.target.apiVersion,KIND:.spec.target.kind'
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" auth can-i patch fluidcrmigrations.fluidcr.dcnlab.com --subresource=status --as=system:serviceaccount:hybridspot-system:hybridspot-management
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" auth can-i update fluidcrmigrations.fluidcr.dcnlab.com --subresource=status --as=system:serviceaccount:hybridspot-system:hybridspot-management
```

## 7. 컨트롤러 교체

각 rollout이 성공한 뒤 다음 명령으로 진행한다. Deployment/컨테이너 이름이 다르면 먼저 실제 이름을 확인하고 문서를 수정한 뒤 실행한다.

```bash
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system set image deployment/policy-manager "manager=docker.io/jeongseungjun/hybrid-spot-vm-system@$(cat "$IMAGES/policy-manager.digest")"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system rollout status deployment/policy-manager --timeout=300s
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system set image deployment/checkpoint-coordinator "manager=docker.io/jeongseungjun/hybrid-spot-vm-system@$(cat "$IMAGES/checkpoint-coordinator.digest")"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system rollout status deployment/checkpoint-coordinator --timeout=300s
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system set image deployment/spot-recovery-controller "manager=docker.io/jeongseungjun/hybrid-spot-vm-system@$(cat "$IMAGES/spot-recovery-controller.digest")"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system rollout status deployment/spot-recovery-controller --timeout=300s
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system set image deployment/placement-webhook "manager=docker.io/jeongseungjun/hybrid-spot-vm-system@$(cat "$IMAGES/placement-webhook.digest")"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system rollout status deployment/placement-webhook --timeout=300s
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n hybridspot-system set image deployment/training-runtime-collector "manager=docker.io/jeongseungjun/hybrid-spot-vm-system@$(cat "$IMAGES/training-runtime-collector.digest")"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n hybridspot-system rollout status deployment/training-runtime-collector --timeout=300s
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n stateful-migration-system set image deployment/stateful-management "manager=docker.io/jeongseungjun/stateful-migration-operator@$(cat "$IMAGES/stateful.digest")"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n stateful-migration-system rollout status deployment/stateful-management --timeout=600s
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n stateful-migration-system set image deployment/stateful-checkpoint "manager=docker.io/jeongseungjun/stateful-migration-operator@$(cat "$IMAGES/stateful.digest")"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n stateful-migration-system rollout status deployment/stateful-checkpoint --timeout=600s
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n stateful-migration-system set image deployment/stateful-member "manager=docker.io/jeongseungjun/stateful-migration-operator@$(cat "$IMAGES/stateful.digest")"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n stateful-migration-system rollout status deployment/stateful-member --timeout=600s
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n stateful-migration-system set image daemonset/stateful-artifact "verifier=docker.io/jeongseungjun/stateful-migration-operator@$(cat "$IMAGES/stateful.digest")"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n stateful-migration-system rollout status daemonset/stateful-artifact --timeout=600s
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n remote-cluster-provisioner-system set image deployment/remote-cluster-provisioner "manager=docker.io/jeongseungjun/my-publiccloudvm-provisioner@$(cat "$IMAGES/provisioner.digest")"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n remote-cluster-provisioner-system rollout status deployment/remote-cluster-provisioner --timeout=600s
```

계획된 full fallback 검증을 실제로 수행할 때만 `stateful-member`의 `--group-control-image` 인자도 새 digest로 바꾼다. 아래 patch는 기존 인자가 정확히 하나일 때만 적용된다.

```bash
export GROUP_CONTROL_IMAGE="docker.io/jeongseungjun/myfluidcr-operator@$(cat "$IMAGES/group-control.digest")"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n stateful-migration-system get deployment stateful-member -o json > "$EVIDENCE/stateful-member-current.json"
jq -e --arg image "$GROUP_CONTROL_IMAGE" '
  . as $d |
  [.spec.template.spec.containers | to_entries[] | select(.value.name=="manager")] as $cs |
  (if ($cs|length)!=1 then error("manager container must be unique") else $cs[0] end) as $c |
  ($c.value.args // []) as $a |
  [$a | to_entries[] | select(.value=="--group-control-image" or (.value|startswith("--group-control-image=")))] as $flags |
  (if ($flags|length)!=1 then error("group-control flag must be unique") else $flags[0] end) as $f |
  (if $f.value=="--group-control-image" then
     if ($a|length)<=$f.key+1 or ($a[$f.key+1]|startswith("--")) then error("group-control value missing")
     else {index:($f.key+1),value:$image} end
   else {index:$f.key,value:("--group-control-image="+$image)} end) as $v |
  [
    {op:"test",path:"/metadata/resourceVersion",value:$d.metadata.resourceVersion},
    {op:"replace",path:("/spec/template/spec/containers/"+($c.key|tostring)+"/args/"+($v.index|tostring)),value:$v.value}
  ]
' "$EVIDENCE/stateful-member-current.json" > "$EVIDENCE/stateful-member-group-control.patch.json"
cat "$EVIDENCE/stateful-member-group-control.patch.json"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n stateful-migration-system patch deployment stateful-member --type=json --patch-file "$EVIDENCE/stateful-member-group-control.patch.json"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n stateful-migration-system rollout status deployment/stateful-member --timeout=600s
```

## 8. webhook payload와 namespace injection 확인

webhook 자체 이미지는 이 절차에서 바꾸지 않는다. 기존 `--payload-image`만 새 overlay digest로 교체한다.

```bash
export PAYLOAD_IMAGE="docker.io/jeongseungjun/myfluidcr-operator@$(cat "$IMAGES/payload-stateful.digest")"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n fluidcr-system get deployment fluidcr-webhook -o json > "$EVIDENCE/webhook-current.json"
jq -e --arg image "$PAYLOAD_IMAGE" '
  . as $d |
  [.spec.template.spec.containers | to_entries[] | select(.value.name=="webhook")] as $cs |
  (if ($cs|length)!=1 then error("webhook container must be unique") else $cs[0] end) as $c |
  ($c.value.args // []) as $a |
  [$a | to_entries[] | select(.value=="--payload-image" or (.value|startswith("--payload-image=")))] as $flags |
  (if ($flags|length)!=1 then error("payload flag must be unique") else $flags[0] end) as $f |
  (if $f.value=="--payload-image" then
     if ($a|length)<=$f.key+1 or ($a[$f.key+1]|startswith("--")) then error("payload value missing")
     else {index:($f.key+1),value:$image} end
   else {index:$f.key,value:("--payload-image="+$image)} end) as $v |
  [
    {op:"test",path:"/metadata/resourceVersion",value:$d.metadata.resourceVersion},
    {op:"replace",path:("/spec/template/spec/containers/"+($c.key|tostring)+"/args/"+($v.index|tostring)),value:$v.value}
  ]
' "$EVIDENCE/webhook-current.json" > "$EVIDENCE/webhook-payload.patch.json"
cat "$EVIDENCE/webhook-payload.patch.json"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n fluidcr-system patch deployment fluidcr-webhook --type=json --patch-file "$EVIDENCE/webhook-payload.patch.json"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n fluidcr-system rollout status deployment/fluidcr-webhook --timeout=300s
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n fluidcr-system get deployment fluidcr-webhook -o json | jq '.spec.template.spec.containers[] | {name,args}'
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" get namespace "$NS" -o yaml
```

기존 namespace injection 설정을 그대로 사용한다. namespace를 새로 만들지 않는다. 과거 operation ID를 재사용하지 않는다.

## 9. NodeProvisionNetConfig runtime 인증 설정

Provisioner의 `certifyRestore=true` 경로는 새 노드 준비 중 driver library 로딩과 runtime qualification을 통과해야 한다. 이미 Ready인 과거 노드는 NodeProvisionNetConfig를 바꿔도 자동 재인증되지 않는다. 기존 real deb/runtime URL과 hash 값은 이미 있는 NetConfig에서 보존한다. 대체용 placeholder 값을 export하거나 새 runtime URL/hash를 덮어쓰지 않는다.

대상 이름은 명시적으로 `aws-vpc-netconfig`다. `.items[0]`로 고르지 않는다. 먼저 AWS source cluster(원본 클러스터)에서 실제 객체와 runtime 값을 백업·검증한다.

```bash
export NETCONFIG_NAME=aws-vpc-netconfig
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get nodeprovisionnetconfig "$NETCONFIG_NAME" -o json > "$EVIDENCE/nodeprovisionnetconfig-aws-before-runtime.json"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get nodeprovisionnetconfig "$NETCONFIG_NAME" -o yaml > "$EVIDENCE/nodeprovisionnetconfig-aws-before-runtime.yaml"
jq -e '.metadata.name == "aws-vpc-netconfig"' "$EVIDENCE/nodeprovisionnetconfig-aws-before-runtime.json"
jq -e '.spec.softwareConfig.nodeSoftware.migrationRuntime as $r | ($r.packageURL|type=="string" and length>0 and (contains("example.invalid")|not) and (contains("replace-with-actual")|not)) and ($r.packageSHA256|type=="string" and length>0 and (contains("replace-with-actual")|not)) and ($r.crioCommit|type=="string" and length>0 and (contains("replace-with-actual")|not)) and ($r.criuCommit|type=="string" and length>0 and (contains("replace-with-actual")|not)) and ($r.adapterSHA256|type=="string" and length>0 and (contains("replace-with-actual")|not))' "$EVIDENCE/nodeprovisionnetconfig-aws-before-runtime.json"
jq '{name:.metadata.name,ownerReferences:.metadata.ownerReferences,labels:.metadata.labels,annotations:.metadata.annotations,nodeSoftware:.spec.softwareConfig.nodeSoftware}' "$EVIDENCE/nodeprovisionnetconfig-aws-before-runtime.json"
jq -n '{spec:{softwareConfig:{nodeSoftware:{migrationRuntime:{certifyRestore:true}}}}}' > "$EVIDENCE/nodeprovisionnetconfig-certifyrestore.patch.json"
cat "$EVIDENCE/nodeprovisionnetconfig-certifyrestore.patch.json"
```

소유권을 확인한다. Karmada가 source object를 관리하고 있으면 member를 직접 patch하지 말고 Karmada source를 patch한다. Karmada에 같은 이름의 source object가 없고 AWS member object가 직접 관리 대상이면 AWS member object만 patch한다.

Karmada source가 있는 경우:

```bash
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get nodeprovisionnetconfig "$NETCONFIG_NAME" -o json > "$EVIDENCE/nodeprovisionnetconfig-karmada-before-runtime.json"
jq '{name:.metadata.name,ownerReferences:.metadata.ownerReferences,labels:.metadata.labels,annotations:.metadata.annotations,nodeSoftware:.spec.softwareConfig.nodeSoftware}' "$EVIDENCE/nodeprovisionnetconfig-karmada-before-runtime.json"
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" patch nodeprovisionnetconfig "$NETCONFIG_NAME" --type=merge --patch-file "$EVIDENCE/nodeprovisionnetconfig-certifyrestore.patch.json"
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get nodeprovisionnetconfig "$NETCONFIG_NAME" -o json | jq '{name:.metadata.name,runtime:.spec.softwareConfig.nodeSoftware.migrationRuntime}'
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get nodeprovisionnetconfig "$NETCONFIG_NAME" -o json | jq '{name:.metadata.name,runtime:.spec.softwareConfig.nodeSoftware.migrationRuntime}'
```

AWS member object가 직접 관리 대상인 경우:

```bash
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" patch nodeprovisionnetconfig "$NETCONFIG_NAME" --type=merge --patch-file "$EVIDENCE/nodeprovisionnetconfig-certifyrestore.patch.json"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get nodeprovisionnetconfig "$NETCONFIG_NAME" -o json | jq '{name:.metadata.name,runtime:.spec.softwareConfig.nodeSoftware.migrationRuntime}'
```

검증은 실제 Provisioner 인증 로그와 NodeProvision/Node status가 일치해야 한다. 라벨만 수동으로 붙인 경우는 불합격이다. Node label key는 코드 기준 `migration.dcnlab.com/restore-from-file`이다.

```bash
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n remote-cluster-provisioner-system logs deployment/remote-cluster-provisioner --since=10m --tail=200
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" get nodes -L migration.dcnlab.com/restore-from-file -o wide
```

## 10. 실제 정기 checkpoint 스케줄 계약

스케줄은 더 이상 pending 계약이 아니다. System `checkpoint-coordinator`가 Karmada에 schedule parent `FluidCRMigration`을 만들고, Stateful `stateful-checkpoint`의 `FluidCRMigrationReconciler`가 member cluster에서 immutable child round를 생성한다.

구현 기준:

- parent 이름은 `<policy>-periodic`이다. 이번 policy에서는 `trainer-realign-periodic`이다. Coordinator parent schedule name도 `trainer-realign-periodic`으로 고정 확인한다.
- parent는 `spec.schedule.enabled=true`, `spec.schedule.intervalSeconds=<seconds>`를 가진다.
- parent는 직접 checkpoint를 실행하지 않는다.
- child는 parent UID label `training.dcnlab.com/scheduled-parent`에 묶이고 `spec.schedule`이 제거된 immutable execution spec이다.
- child checkpoint ID와 child name은 `stateful-checkpoint`의 `FluidCRMigrationReconciler`가 reservation 시점에 동적으로 만들고 `status.currentRun.checkpointID`, child annotation `training.dcnlab.com/checkpoint-id`에 persisted ref로 보존한다. 문서에서 형식을 고정하지 않는다.
- 활성 child가 있으면 다음 child를 만들지 않는다.
- periodic child가 `Failed`이면 schedule은 hold한다. 실패 child만 보고 다음 child를 자동 생성하지 않고, 안전 조건이 충족될 때까지 parent는 `Pending` 대기 상태로 남는다. `Paused`는 `spec.schedule.enabled=false` ack에만 사용한다.
- child가 durable archive evidence까지 완료되어야 parent `status.lastSuccessfulFullCheckpoint`가 갱신된다.
- `lastSuccessfulFullCheckpoint.result.spec/status`는 Stateful member가 보존하고, System Coordinator가 control-plane evidence CR role `checkpoint-evidence`로 mirror한다. 이 mirrored control CP record는 handoff/evidence용이며 workload로 다시 전파하지 않는다.

합격 조건:

- `trainer-realign-periodic` parent가 존재한다.
- child의 `spec.schedule`은 `null` 또는 absent다.
- parent `status.currentRun`이 활성 child 하나를 가리킨다.
- parent `status.lastSuccessfulFullCheckpoint.result.spec/status`가 durable full checkpoint 완료 child 결과를 가진다.
- Coordinator mirror CR은 control-plane record로서 `training.dcnlab.com/role=checkpoint-evidence`이고 source checkpoint UID와 schedule UID annotation을 가진다. 이 record를 member workload로 다시 전파하지 않는다.
- 이전 child의 completion/export evidence가 끝나기 전에는 다음 child가 생성되지 않는다.
- retained scheduled round와 mirrored evidence CR은 현재 자동 GC되지 않는다. RestoreRequest/partial handoff reference가 더 이상 쓰지 않는 것을 확인한 뒤 사용자가 수동 retention/삭제를 결정한다.

주기 입력은 기존 TrainingPolicy checkpoint 필드에서 계산되지만, 실행 확인은 재개 이후 12절에서 수행한다. 10절은 계약 설명만 담고 parent 조회나 interval patch를 실행하지 않는다.

## 11. 기존 namespace 재개 순서

기존 PVC를 보존하므로 학습 재개 전 상태를 확인한다. 과거 Completed checkpoint를 이번 성공 증거로 쓰지 않는다. 새 operation은 컨트롤러가 현재 source UID에 묶어 자동 생성하게 둔다.

```bash
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get statefulset "$APP" -o jsonpath='{.metadata.uid}{"\n"}'
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get statefulset "$APP" -o json | jq '{uid:.metadata.uid,templateUID:.spec.template.metadata.labels["training.dcnlab.com/workload-uid"],strategy:.spec.updateStrategy,replicas:.spec.replicas}'
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get trainingpolicy "$POLICY" -o json | jq '{annotations:.metadata.annotations,targetWorkers:.spec.targetWorkers,expectedWorldSize:.spec.expectedWorldSize,policy:.spec.policy,workloadRef:.spec.workloadRef,runtimeRef:.spec.runtimeRef,checkpoint:.spec.checkpoint,replacement:.spec.replacement}'
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get pods,pvc,nodeprovisions,nodeprovisionnetconfigs,fluidcrmigrations
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" scale statefulset "$APP" --replicas=2
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" annotate trainingpolicy "$POLICY" training.dcnlab.com/suspend-
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get pod trainer-0 trainer-1 -o json | jq '[.items[] | {name:.metadata.name,uid:.metadata.uid,node:.spec.nodeName,initContainers:.spec.initContainers,containerStatuses:.status.containerStatuses}]'
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get statefulset "$APP" -o json | jq '{labels:.spec.template.metadata.labels,strategy:.spec.updateStrategy}'
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get pod trainer-0 trainer-1 -o wide
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" logs trainer-0 -c trainer --timestamps --since=2m --tail=20
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" logs trainer-1 -c trainer --timestamps --since=2m --tail=20
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" exec trainer-0 -c trainer -- python3 -c 'import urllib.request; print(urllib.request.urlopen("http://127.0.0.1:8298/runtime",timeout=5).read().decode())'
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" exec trainer-1 -c trainer -- python3 -c 'import urllib.request; print(urllib.request.urlopen("http://127.0.0.1:8298/runtime",timeout=5).read().decode())'
```

새 Pod의 initContainer image가 이번 `payload-stateful.digest`여야 한다. StatefulSet `OnDelete`와 `training.dcnlab.com/workload-uid` 라벨을 확인한다.

## 12. 정기 checkpoint 검증

두 rank가 Running이고 step이 증가한 뒤에만 정기 checkpoint를 성공 증거로 본다.

재개 후 Coordinator가 `trainer-realign-periodic` parent를 만들거나 갱신한 뒤에만 아래 조회를 실행한다. parent가 아직 없으면 즉시 실패로 판단하지 말고 controller reconcile, TrainingPolicy suspend 해제, Pod Running 상태를 먼저 확인한다.

```bash
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" patch trainingpolicy "$POLICY" --type=merge -p '{"spec":{"checkpoint":{"minIntervalSeconds":120,"maxIntervalSeconds":120,"candidateIntervalSeconds":[120],"riskBands":[{"maxLambdaPerHour":1,"intervalSeconds":120}]}}}'
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get trainingpolicy "$POLICY" -o json | jq '.spec.checkpoint'
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get fluidcrmigration "$POLICY-periodic" -o json | jq '{name:.metadata.name,uid:.metadata.uid,labels:.metadata.labels,ownerReferences:.metadata.ownerReferences,schedule:.spec.schedule,workloadRef:.spec.workloadRef,status:.status}'
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get fluidcrmigration "$POLICY-periodic" -o json | jq '{name:.metadata.name,uid:.metadata.uid,labels:.metadata.labels,schedule:.spec.schedule,status:.status.currentRun,lastSuccessfulFullCheckpoint:.status.lastSuccessfulFullCheckpoint}'
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get fluidcrmigrations -l training.dcnlab.com/scheduled-parent -o json | jq '[.items[] | {name:.metadata.name,uid:.metadata.uid,labels:.metadata.labels,annotations:.metadata.annotations,schedule:.spec.schedule,phase:.status.phase,archive:.status.archive,completionTime:.status.completionTime}]'
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get fluidcrmigrations -l training.dcnlab.com/role=checkpoint-evidence -o json | jq '[.items[] | {name:.metadata.name,labels:.metadata.labels,annotations:.metadata.annotations,spec:.spec,status:.status}]'
```

```bash
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get trainingruntime -o yaml
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get fluidcrmigration "$POLICY-periodic" -o yaml > "$EVIDENCE/member-periodic-parent.yaml"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get fluidcrmigrations -l training.dcnlab.com/scheduled-parent -o yaml > "$EVIDENCE/member-scheduled-children.yaml"
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get fluidcrmigrations -l training.dcnlab.com/role=checkpoint-evidence -o yaml > "$EVIDENCE/karmada-checkpoint-evidence.yaml"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get fluidcrmigrations -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,GEN:.metadata.generation,OBSERVED:.status.observedGeneration,COMPLETE:.status.completionTime'
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get fluidcrmigrations -o json | jq '[.items[] | {name:.metadata.name,labels:.metadata.labels,annotations:.metadata.annotations,schedule:.spec.schedule,status:.status}]'
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" logs trainer-0 -c trainer --timestamps --since=5m --tail=30
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" logs trainer-1 -c trainer --timestamps --since=5m --tail=30
```

합격 조건:

- `trainer-realign-periodic` parent가 이번 controller에 의해 갱신된다.
- child round가 이번 실행에서 새로 관측되고 `spec.schedule`이 없다.
- member rank별 result가 `lastSuccessfulFullCheckpoint.result` 안에 보존된다.
- Coordinator가 동일 result를 `checkpoint-evidence` CR로 mirror한다.
- inflight child 또는 failed periodic child가 안전하게 처리되지 않은 동안 다음 child가 생성되지 않는다. failed periodic child는 `Pending` 대기로 보고, `Paused` ack로 오해하지 않는다.
- retained round와 evidence CR이 자동 삭제된다고 가정하지 않는다. 현재 자동 GC는 없다.
- `status.checkpoint.periodicQuiesced`와 `lastMigrationName`이 suspend 상태를 정확히 설명한다.

## 13. 계획된 group fallback의 좁은 허용 조건

이 문서는 모든 실패가 자동 복구된다고 주장하지 않는다. `Failed` 상태 하나, API error 하나, 또는 오래된 partial 흔적 하나만으로 group fallback을 시작하지 않는다. 다음 조건을 모두 만족할 때만 계획된 group fallback handoff를 검토한다.

- 원래 참여자 전체 Pod UID가 새 Pod UID로 교체되었다.
- 모든 rank에서 fresh `Runtime` 증거가 새로 수집되었다.
- 기존 partial `RestoreRequest`가 active 상태로 남아 있지 않다.
- 새 full durable scheduled round가 완료되어 `lastSuccessfulFullCheckpoint.result`와 Coordinator mirror evidence가 모두 확인되었다.
- source가 missing/not ready 상태가 아니고, survivor가 변경되지 않은 상태로 남아 있지 않다.

source가 단순히 missing, not ready, unchanged survivor 상태이거나 active partial request가 있으면 hold한다. unsafe auto cancellation이나 CR status/finalizer 조작으로 진행하지 않는다.

## 14. Partial replacement 재개

정기 full checkpoint가 새로 완료되고 Coordinator mirror까지 확인된 뒤에만 Partial을 유발한다. 기존 operation ID를 재사용하지 않는다. 새 operation은 컨트롤러가 source UID 기준으로 자동 생성한다. 기존 partial RestoreRequest가 active 상태면 중단한다.

```bash
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" patch trainingpolicy "$POLICY" --type=merge -p '{"spec":{"replacement":{"enabled":true}}}'
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" patch spotriskprofile "$RISK" --type=merge -p '{"spec":{"staticLambdaPerHour":0.5}}'
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get spotreplacements,restorerequests,spotrecoveries -o custom-columns='KIND:.kind,NAME:.metadata.name,PHASE:.status.phase,MESSAGE:.status.message'
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get spotreplacements -o json | jq '[.items[] | {name:.metadata.name,uid:.metadata.uid,spec:.spec,status:.status}]'
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get restorerequests -o json | jq '[.items[] | {name:.metadata.name,uid:.metadata.uid,spec:.spec,status:.status}]'
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get restoreplans -o yaml
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get pod trainer-0 trainer-1 -o wide
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" exec trainer-0 -c trainer -- python3 -c 'import urllib.request; print(urllib.request.urlopen("http://127.0.0.1:8298/runtime",timeout=5).read().decode())'
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" exec trainer-1 -c trainer -- python3 -c 'import urllib.request; print(urllib.request.urlopen("http://127.0.0.1:8298/runtime",timeout=5).read().decode())'
```

합격 조건:

- target rank만 이동한다.
- survivor Pod UID와 restartCount가 유지된다.
- survivorResume 증거의 checkpointID/generation/UID가 이번 Partial operation과 맞는다.
- RestoreRequest `status.verification`이 `Verified`다.
- 기존 Spot NodeProvision과 EC2 인스턴스 종료가 컨트롤러에 의해 확인된다.

`RestoreReady`는 최종 성공이 아니다.

## 15. 실패 시 보존과 되돌리기

먼저 증거를 보관한다. 실패 CR을 삭제하거나 status/finalizer를 조작하지 않는다.

```bash
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get trainingpolicy "$POLICY" -o yaml > "$EVIDENCE/trainingpolicy-failure.yaml"
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get spotreplacements,restorerequests,spotrecoveries,fluidcrmigrations -o yaml > "$EVIDENCE/karmada-failure.yaml"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get pods,pvc,nodeprovisions,nodeprovisionnetconfigs,fluidcrmigrations,restoreplans -o yaml > "$EVIDENCE/aws-failure.yaml"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system logs deployment/policy-manager --since=15m --tail=300 > "$EVIDENCE/policy-manager-failure.log"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system logs deployment/checkpoint-coordinator --since=15m --tail=300 > "$EVIDENCE/checkpoint-coordinator-failure.log"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system logs deployment/spot-recovery-controller --since=15m --tail=300 > "$EVIDENCE/spot-recovery-controller-failure.log"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n stateful-migration-system logs deployment/stateful-member --since=15m --tail=300 > "$EVIDENCE/stateful-member-failure.log"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n stateful-migration-system logs deployment/stateful-checkpoint --since=15m --tail=300 > "$EVIDENCE/stateful-checkpoint-failure.log"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n fluidcr-system logs deployment/fluidcr-webhook --since=15m --tail=300 > "$EVIDENCE/fluidcr-webhook-failure.log"
```

컨트롤러 이미지를 되돌릴 때는 2절 백업의 image/args만 사용한다. CRD를 급히 축소하지 않는다. 학습 Pod가 살아 있고 restore가 진행 중이면 이미지 혼용 상태로 바꾸지 않는다.

## 코드별 보완점

- `policy-manager`: 기존 `fluidcr-realign-121040`의 StatefulSet UID와 TrainingPolicy workloadRef UID가 계속 일치해야 한다. template UID 라벨 key는 코드 기준 `training.dcnlab.com/workload-uid`다.
- `checkpoint-coordinator`: schedule parent 이름은 `<policy>-periodic`이고 role label은 `checkpoint-schedule`이다. suspend/competing operation 중에는 parent `spec.schedule.enabled=false` ack를 기다려야 한다.
- `checkpoint-coordinator`: member parent `status.lastSuccessfulFullCheckpoint.result.spec/status`를 읽어 control-plane evidence CR `checkpoint-evidence`로 mirror한다. nested schedule이 있는 result는 거부해야 한다.
- `stateful-checkpoint`: parent는 checkpoint를 직접 실행하지 않고 child를 만든다. child는 `training.dcnlab.com/scheduled-parent` label과 scheduled-parent annotation으로 parent UID에 묶이고 `spec.schedule`이 제거되어야 한다.
- `stateful-checkpoint`: active child가 있으면 다음 child를 만들지 않는다. durable archive/export evidence가 없는 child를 `lastSuccessfulFullCheckpoint`로 쓰지 않는다.
- `stateful-checkpoint`: 기존 PVC의 과거 checkpoint를 새 성공 증거로 재사용하지 않는다. 새 round와 이번 Pod UID/step 증가를 함께 기록한다.
- `provisioner`: `NodeProvisionNetConfig`는 실제 runtime package URL/hash/commit을 보존한 뒤 `certifyRestore=true`로 설정한다. 이미 Ready인 노드는 자동 재인증되지 않는다.
- `fluidcr-webhook/payload`: namespace injection은 기존 namespace 설정을 사용하고, 새 Pod initContainer가 새 overlay digest를 참조하는지 확인한다.
- `group-control`: 계획된 full fallback 검증용 보조 이미지다. `Failed` operation, API error, source missing/not ready, unchanged survivor, active partial request를 자동 group 시작 조건으로 취급하지 않는다.

## 검증 범위

이 문서 작성 단계에서는 로컬 문서 검증과 repo code/path 확인만 수행했다. 실제 MGMT/AWS/Karmada cloud 명령, buildah build/push, rollout, runtime 복원 테스트를 실행했다고 주장하지 않는다.
