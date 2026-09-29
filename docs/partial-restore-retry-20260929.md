# Partial 복원 재배포 및 검증 가이드

> 최신 Partial 재개 증거·준비 상태 수정본의 빌드/배포는
> [한국어 재배포 가이드](partial-resume-release-20260930.md)를 우선한다.
> 아래는 이전 검증 당시의 기록이며, 새 검증에는 과거 operation/PVC를 재사용하지 않는다.

> **2026-09-30:** 기존 Group 작업 정리와 replicas=0 확인을 끝낸 현재 환경에서는
> [정리 후 재배포 가이드](partial-redeploy-20260930.md)를 따른다.
> 아래 기존 절차는 이전 기록이다. 함수/반복문과 정리 순서를 다시 실행하지 않는다.

## 0. 이번 시험의 목표와 현재 상태

목표는 **교체 대상 rank만 새 노드에서 복원하고, survivor rank의 Pod와 프로세스는 보존하는 것**이다.
Periodic checkpoint는 전체 rank 대상으로 실행해도 된다. 전체 checkpoint와 전체 Pod 교체는 다른 동작이다.

이번 수정으로 Partial 준비 조건 미충족 시 그룹 복원으로 자동 전환하지 않는다.
`training.dcnlab.com/planned-partial=disabled`를 명시한 경우에만 기존 그룹 선택 경로를 사용한다.
이번 시험에는 이 값을 사용하지 않는다. 상태 `partial_replacement_waiting`은 준비 대기다.

**현재 실패한 group-prepare 작업은 이 수정으로 자동 복구되지 않는다.**
기존 두 source Pod가 이미 삭제되었다면 survivor 증거가 없으므로 해당 작업을 Partial로 변경할 수 없다.
기존 객체/PVC를 보존하고, 기존 작업의 정리를 확인한 다음 새로운 학습 실행과 checkpoint로 시험한다.
아래 1~3절은 먼저 진행할 수 있다. 4절 이후에는 기존 작업 정리 확인이 필요하다.

변경 대상:
- System: policy-manager, checkpoint-coordinator 이미지.
- Stateful: payload-stateful 이미지. 현재 FluidCR payload-base를 먼저 빌드한다.
- 이번 수정만으로는 CRD, Provisioner, stateful-member 바이너리, group-control 이미지를 다시 배포할 필요가 없다.
- 기존 trainer Pod의 주입된 Python 파일은 webhook 업데이트만으로 바뀌지 않는다.
- 아래 명령은 MGMT의 Bash에서 실행한다. AWS control-plane 기본 context와 혼용하지 않는다.

## 1. 환경 지정과 실패 증거 보존

각 절의 명령은 같은 MGMT 셸에서 실행한다. 오류가 발생하면 다음 절로 진행하지 않는다.
kubeconfig는 실제 파일의 절대경로가 이미 설정되어 있어야 한다.

```bash
set -euo pipefail
ROOT=/root/hybridspot-validation
SYS="$ROOT/System"
STATEFUL="$ROOT/stateful"
FLUID="$ROOT/fluidcr"
NS=fluidcr-realign-121040
POLICY=trainer-realign
BRANCH=restore-automation-20260928
REG=docker.io/jeongseungjun
EVIDENCE="$ROOT/evidence/partial-retry-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$EVIDENCE"
: "${KARMADA_KUBECONFIG:?}"
: "${AWS_KUBECONFIG:?}"
: "${MGMT_KUBECONFIG:?}"
for CFG in "$KARMADA_KUBECONFIG" "$AWS_KUBECONFIG" "$MGMT_KUBECONFIG"; do
  test -f "$CFG"
  kubectl --kubeconfig="$CFG" config view --minify \
    -o jsonpath='{.clusters[0].cluster.server}{"\n"}'
done
k() { kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" "$@"; }
a() { kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" "$@"; }
k get trainingpolicy "$POLICY" -o yaml > "$EVIDENCE/policy-before.yaml"
k get spotreplacements,restorerequests,restoreplans,fluidcrmigrations -o yaml \
  > "$EVIDENCE/control-before.yaml"
a get pods,statefulsets,pvc,restoreplans,fluidcrmigrations -o yaml \
  > "$EVIDENCE/member-before.yaml"
a get events --sort-by=.metadata.creationTimestamp > "$EVIDENCE/events-before.txt"
k annotate trainingpolicy "$POLICY" training.dcnlab.com/suspend=true --overwrite
echo "EVIDENCE=$EVIDENCE"
```

`FLUID`는 예시 경로다. 실제 FluidCR 소스 디렉터리가 다르면 수정한다.
이 중단 annotation은 **새 정책 결정을 중단할 뿐, 이미 진행 중인 복원을 취소하지 않는다.**
기존 작업이 정리되기 전에는 trainer Pod 삭제, lock 삭제, status 변경, 자동화 재개를 하지 않는다.

## 2. 원격 코드 동기화

체크아웃 경로와 origin 저장소가 맞는지 출력으로 확인한다.
로컬 수정이 있으면 덮어쓰지 않고 중단한다.

```bash
for DIR in "$SYS" "$STATEFUL" "$FLUID"; do
  test -d "$DIR/.git"
  test -z "$(git -C "$DIR" status --porcelain)"
  test "$(git -C "$DIR" branch --show-current)" = "$BRANCH"
  git -C "$DIR" remote get-url origin
  git -C "$DIR" fetch origin "$BRANCH"
  git -C "$DIR" merge --ff-only "origin/$BRANCH"
  git -C "$DIR" log -1 --oneline
done
git -C "$STATEFUL" merge-base --is-ancestor d0a7470 HEAD
git -C "$FLUID" merge-base --is-ancestor b47aeaebfd2c9a82b63758378224a36e86595c22 HEAD
grep -n 'errPartialReplacementNotReady' "$SYS/internal/management/capacity.go"
grep -n 'group_restore.publish_round' "$STATEFUL/runtime/fluidcr/backends/pytorch.py"
test -f "$FLUID/fluidcr/iteration.py"
for DIR in "$SYS" "$STATEFUL" "$FLUID"; do
  printf '%s ' "$DIR"
  git -C "$DIR" rev-parse HEAD
done | tee "$EVIDENCE/source-revisions.txt"
```

System 커밋은 이번 push 완료 보고의 SHA도 `git merge-base --is-ancestor <SHA> HEAD`로 확인한다.
`Not a valid object name`이 나오면 빌드하지 말고 저장소 URL/브랜치/fetch 결과를 확인한다.

## 3. 이미지 빌드와 registry push

이 단계는 아직 클러스터에 적용하지 않는다. Buildah 로그인과 디스크 여유를 먼저 확인한다.
각 이미지 이름에 두 저장소의 revision을 포함하고, 배포에는 digest를 사용한다.

```bash
RELEASE="partial-$(git -C "$SYS" rev-parse --short=8 HEAD)-$(git -C "$STATEFUL" rev-parse --short=8 HEAD)"
IMAGES="$EVIDENCE/images"
mkdir -p "$IMAGES"
build_push() {
  local NAME="$1" REPO="$2" CONTEXT="$3"
  shift 3
  local IMAGE="$REPO:$NAME-$RELEASE"
  buildah bud --arch amd64 -t "$IMAGE" "$@" "$CONTEXT"
  buildah push --digestfile "$IMAGES/$NAME.digest" "$IMAGE" "docker://$IMAGE"
  printf '%s@%s\n' "$REPO" "$(cat "$IMAGES/$NAME.digest")" > "$IMAGES/$NAME.image"
}
for COMPONENT in policy-manager checkpoint-coordinator; do
  build_push "$COMPONENT" "$REG/hybrid-spot-vm-system" "$SYS" \
    --build-arg "COMPONENT=$COMPONENT"
done
build_push payload-base "$REG/myfluidcr-operator" "$FLUID" \
  -f "$FLUID/Dockerfile.payload"
build_push payload-stateful "$REG/myfluidcr-operator" "$STATEFUL" \
  -f "$STATEFUL/Dockerfile.payload-overlay" \
  --build-arg "FLUIDCR_PAYLOAD_IMAGE=$(cat "$IMAGES/payload-base.image")"
cat "$IMAGES/"*.image
```

## 4. 기존 그룹 작업 정리 확인: 여기서 먼저 판단

```bash
k get spotreplacements,restorerequests,restoreplans \
  -o custom-columns='KIND:.kind,NAME:.metadata.name,PHASE:.status.phase,MESSAGE:.status.message'
a get restoreplans -o json | jq '[.items[] | {
  name:.metadata.name, uid:.metadata.uid, spec:.spec, status:.status
}]'
a get statefulset trainer -o json | jq '{
  replicas:.spec.replicas, annotations:.metadata.annotations,
  templateAnnotations:.spec.template.metadata.annotations
}'
a get pods -o wide
k get trainingpolicy "$POLICY" -o json | jq '{
  annotations:.metadata.annotations, checkpoint:.status.checkpoint
}'
```

**이 결과에 기존 group RestoreRequest/RestorePlan, group-restore-intent가 남으면 아래로 진행하지 않는다.**
Failed 상태도 단순히 무시하지 않는다. annotation 중단만으로 member actuator가 멈추지 않는다.

이전 로그만으로 현재 객체 UID, finalizer, 실제 pause 상태를 확정할 수 없으므로 이 가이드는
무조건 삭제하는 명령을 제공하지 않는다. 4절 결과로 기존 작업의 소유관계와 정리 순서를 확정한다.
정리 시 원칙:
- 실패 증거를 저장하고 해당 작업을 담당하는 controller의 진행을 제어한 뒤 정확한 작업만 제거한다.
- Karmada 원본과 AWS 전파본 모두 정리됐는지 확인한다.
- 기존 PVC/NodeProvision은 보존한다. PVC 루트의 lock/manifest를 재사용한 채 새 학습을 시작하지 않는다.
- 필요하면 Pod가 모두 정지한 상태에서 기존 checkpoint 작업영역을 백업하거나 새 PVC를 사용한다.
- 기존 그룹 작업을 Partial로 patch하거나 pause 증거를 만들어 넣지 않는다.

현재 사용자가 보내야 할 출력은 이 4절이다. **이미지를 빌드하는 것과 실패 작업을 정리하는 것은 별도 단계**다.

## 5. 정리 완료 후 수정 이미지 배포

아래는 기존 설치 이름 기준이다. 이름/container가 다르면 조회 후 맞춘다.
클러스터 전역 Deployment이므로 다른 학습 작업이 checkpoint/restore 중이면 배포하지 않는다.

```bash
for COMPONENT in policy-manager checkpoint-coordinator; do
  kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system \
    set image deployment/"$COMPONENT" "manager=$(cat "$IMAGES/$COMPONENT.image")"
  kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system \
    rollout status deployment/"$COMPONENT" --timeout=300s
done
OBJ="$(kubectl --kubeconfig="$AWS_KUBECONFIG" -n fluidcr-system \
  get deployment fluidcr-webhook -o json)"
PATCH="$(printf '%s' "$OBJ" | jq -ce \
  --arg value "$(cat "$IMAGES/payload-stateful.image")" '
  . as $d |
  [.spec.template.spec.containers | to_entries[] |
   select(.value.name=="webhook")] as $matches |
  select(($matches|length)==1) | $matches[0] as $c |
  ($c.value.args // []) as $a |
  select(all($a[]; . != "--payload-image")) |
  [
    {op:"test",path:"/metadata/resourceVersion",value:$d.metadata.resourceVersion},
    {op:"add",path:("/spec/template/spec/containers/"+($c.key|tostring)+"/args"),
     value:([$a[] | select(startswith("--payload-image=")|not)] +
            ["--payload-image="+$value])}
  ]')"
kubectl --kubeconfig="$AWS_KUBECONFIG" -n fluidcr-system \
  patch deployment fluidcr-webhook --type=json -p "$PATCH"
kubectl --kubeconfig="$AWS_KUBECONFIG" -n fluidcr-system \
  rollout status deployment/fluidcr-webhook --timeout=300s
```

payload 인자가 `--payload-image IMAGE` 두 항목 형태면 위 스크립트가 중단한다.
기존 args를 조회하고 해당 값만 변경한다. 다른 args 전체를 덮어쓰지 않는다.
webhook의 namespaceSelector/allowlist에 시험 namespace가 포함되어야 한다.

## 6. 새 학습과 새 checkpoint 확인

기존 그룹 작업 정리, 작업영역 보존, 신규 payload 적용이 끝난 다음 학습 Pod를 새로 생성한다.
정책의 workload UID는 실제 Karmada StatefulSet UID와 일치해야 한다.
StatefulSet을 삭제·재생성했다면 이전 workload UID/TrainingRuntime을 그대로 사용하지 않는다.

같은 StatefulSet/정책 UID를 유지한 깨끗한 실행이 준비된 경우:
```bash
k annotate trainingpolicy "$POLICY" training.dcnlab.com/planned-partial=enabled --overwrite
k patch trainingpolicy "$POLICY" --type=merge -p '{"spec":{"replacement":{"enabled":false}}}'
k patch spotriskprofile trainer-risk --type=merge -p '{"spec":{"staticLambdaPerHour":0}}'
k annotate trainingpolicy "$POLICY" training.dcnlab.com/suspend-
a wait pod/trainer-0 pod/trainer-1 --for=condition=Ready --timeout=300s
a get pods -l app=trainer -o json > "$EVIDENCE/pods-before-partial.json"
for POD in trainer-0 trainer-1; do
  a get pod "$POD" -o json | jq '{
    name:.metadata.name, uid:.metadata.uid,
    initImages:[.spec.initContainers[]? | {name,image}]
  }'
  a logs "$POD" -c trainer --timestamps --since=3m --tail=20
done
a get fluidcrmigrations --sort-by=.metadata.creationTimestamp \
  -o custom-columns='NAME:.metadata.name,CREATED:.metadata.creationTimestamp,PHASE:.status.phase,MESSAGE:.status.message'
```

0 위험률은 이번 **정적 실험 설정**이다. 새 주기 checkpoint까지 최대 600초가 걸릴 수 있다.
새로 생성된 CR의 이름을 확인하고 다음을 실행한다. 과거 Completed를 합격으로 쓰지 않는다.

```bash
read -r -p "이번에 생성된 checkpoint CR 이름: " NEW_MIG
a get fluidcrmigration "$NEW_MIG" -o json | tee "$EVIDENCE/new-checkpoint.json" |
  jq '{name:.metadata.name, created:.metadata.creationTimestamp, spec:.spec, status:.status}'
for POD in trainer-0 trainer-1; do
  a exec "$POD" -c trainer -- ls -l "/checkpoint/rounds/$NEW_MIG/ranks"
  a logs "$POD" -c trainer --since=2m --tail=15
done
```

합격: 신규 CR Completed, 두 rank 학습 step 증가, 전체 checkpoint의 ranks/0.json·1.json 존재.
Partial checkpoint에는 전체 그룹용 ranks 메타데이터가 없어도 된다.

## 7. Partial 교체 트리거와 관찰

실제 VM 생성과 학습 중단을 유발한다. 6절 통과 및 기존 작업 없음 확인 후 실행한다.
worker 목록에 Spot이 있어야 한다. 이미 모두 OnDemand이면 위험률을 높여도 교체 대상이 없다.

```bash
k get nodeprovisions -o json | jq '[.items[] | {
  name:.metadata.name, uid:.metadata.uid, market:.spec.marketType, status:.status
}]'
k patch trainingpolicy "$POLICY" --type=merge -p '{"spec":{"replacement":{"enabled":true}}}'
k patch spotriskprofile trainer-risk --type=merge -p '{"spec":{"staticLambdaPerHour":0.5}}'
while true; do
  date -u
  k get spotreplacements,restorerequests \
    -o custom-columns='KIND:.kind,NAME:.metadata.name,PHASE:.status.phase,MESSAGE:.status.message'
  a get pods -l app=trainer -o wide
  sleep 5
done
```

Ctrl+C는 조회만 멈춘다. 복원 작업은 취소하지 않는다.
조회 후 실제 작업명을 입력하고 연결된 체크포인트를 확인한다.

```bash
read -r -p "새 SpotReplacement 이름: " OP
k get spotreplacement "$OP" -o json | tee "$EVIDENCE/replacement.json" |
  jq '{uid:.metadata.uid, spec:.spec, status:.status}'
PARTIAL="$(k get spotreplacement "$OP" -o jsonpath='{.status.partialCheckpointRef.name}')"
test -n "$PARTIAL"
a get fluidcrmigration "$PARTIAL" -o json | tee "$EVIDENCE/partial-checkpoint.json" |
  jq '{phase:.status.phase, message:.status.message, pods:.status.pods}'
a get restoreplans -o json > "$EVIDENCE/restoreplans.json"
```

기대 결과:
- SpotReplacement의 partialCheckpoint/partialRestore에 교체 대상 rank만 지정.
- preservedSurvivors에 나머지 rank의 기존 Pod UID.
- 대상 ContainerCheckpointed, survivor SurvivorPaused 및 live pause 증거.
- 대상만 교체 노드로 이동. 이번 시도에 새 group-prepare 작업이 생기면 Partial 합격이 아니다.
- 작업이 빠르게 진행되므로 관찰 명령은 삭제 방지 장치가 아니다. 문제가 보이면 증거를 남기고 중단 절차를 확인한다.

## 8. 복원 합격과 캡처

```bash
date -u
a get pods -l app=trainer -o json > "$EVIDENCE/pods-after-partial.json"
a get pods -l app=trainer \
  -o custom-columns='POD:.metadata.name,UID:.metadata.uid,NODE:.spec.nodeName,READY:.status.containerStatuses[0].ready,RESTARTS:.status.containerStatuses[0].restartCount'
for POD in trainer-0 trainer-1; do
  a logs "$POD" -c trainer --timestamps --since=3m --tail=20 |
    tee "$EVIDENCE/$POD-after.log"
done
k get spotreplacements,restorerequests -o json > "$EVIDENCE/restore-control-after.json"
a get restoreplans -o json > "$EVIDENCE/restoreplans-after.json"
k get trainingruntime trainer-realign-runtime -o json > "$EVIDENCE/runtime-after.json"
```

서로 다른 증거를 구분해서 판단한다.
1. **학습 재개:** 두 rank의 step이 증가하고 작업에 연결된 checkpoint ID가 일치.
2. **Partial 유지:** survivor Pod UID와 컨테이너 restartCount가 이전과 동일.
3. **대상 이동:** 대상 새 Pod UID와 replacement node가 RestorePlan과 일치.
4. **복원 검증:** RestoreRequest의 검증 증거가 현재 작업/체크포인트/Pod에 연결됨.
5. **이전 VM 정리:** 명시적 복원 검증 게이트 통과 후 controller가 처리.

Running/Ready만으로 CRIU 복원 검증 완료라고 표시하지 않는다.
이전 노드를 수동 삭제하거나 Verified/status/lock을 조작해 검증을 통과시키지 않는다.

## 9. 로컬 검증 범위

System Go 전체 테스트·vet·build, overlay 38개 테스트 및 checkpoint load 회귀 테스트,
기본 FluidCR 비GPU 테스트 90개를 확인했다. 실제 이미지 빌드, AWS GPU DDP, CRIU/NCCL
Partial 복원은 재배포 후 위 절차로 검증해야 한다.
