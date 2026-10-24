# Application Failover 데모 후 experiment2 복귀

MGMT Linux의 Bash에서 `/root/hybridspot-validation/System`을 작업 디렉터리로 사용한다.
이 문서는 데모 **전** 설정 기록, 데모 종료, 원래 설정 복구, checkpoint 실험 재개 순서다.
문서의 명령을 작성·검토한 것이 실제 GPU/클러스터 검증 완료를 의미하지 않는다.

## 1. 전환 조건과 API 대상

MGMT `hybridspot-system`은 이전에 일시 중지됐고 PV/Stateful 컨트롤러는 실행 중이었다.
정확한 replicas와 이미지는 아래에서 다시 읽어 보관한다. 모두 1로 되돌리지 않는다.
데모는 미리 만든 onpre1/AWS GPU On-Demand를 사용하는 새 opt-in 경로다.
실제 API 값은 `TrainingPolicy.spec.capacity.mode: Existing`이며, 외부 용량을 소유하거나
삭제하지 않는다. experiment2는 기본 Provision 경로를 유지한다. 실제 API 값은
`Provisioned`이며 mode 생략도 같은 기본값이다.
데모 policy를 실험 policy-source로 재사용하지 않는다.

| 대상 | kubeconfig | 관리 객체 |
|---|---|---|
| MGMT 호스트 | `MGMT_KUBECONFIG` | System/PV/Stateful/Karmada controller Deployment |
| Karmada API | `KARMADA_KUBECONFIG` | TrainingPolicy, 원본 StatefulSet/PropagationPolicy/ResourceBinding, Karmada webhook 등록 |
| AWS Member | `AWS_KUBECONFIG` | AWS Pod/Node, member controller/webhook, member NodeProvision |
| onpre1 Member | `ONPRE1_KUBECONFIG` | source Pod/Node, member controller/webhook |

MGMT Deployment 조작에 Karmada kubeconfig를 사용하지 않는다. `onpre1`은 Karmada cluster
이름이다. 기본 kubectl context에 의존하지 않고 모든 명령에서 대상을 명시한다.

```bash
cd /root/hybridspot-validation/System
set -euo pipefail
set +x
umask 077
# MGMT 기본 kubeconfig의 kubernetes context에서 별도 0600 파일을 만든다.
# 이미 검증한 MGMT_KUBECONFIG가 있으면 재사용한다. 인증 정보는 출력하지 않는다.
if test -z "${MGMT_KUBECONFIG:-}"; then
  mgmt_config=$(mktemp /tmp/application-failover-mgmt.XXXXXX)
  kubectl --context=kubernetes config view --minify --flatten --raw > "$mgmt_config"
  chmod 600 "$mgmt_config"
  export MGMT_KUBECONFIG="$mgmt_config"
fi
test -s "$MGMT_KUBECONFIG"
kubectl --kubeconfig="$MGMT_KUBECONFIG" config view --minify \
  -o jsonpath='{.clusters[0].cluster.server}{"\n"}'
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system get deployments
: "${MGMT_KUBECONFIG:?}" "${KARMADA_KUBECONFIG:?}"
: "${AWS_KUBECONFIG:?}" "${ONPRE1_KUBECONFIG:?}"
export NS=fluidcr-realign-121040
export DEMO_NS="$NS" DEMO_WORKLOAD=r50-fluidcr-demo DEMO_POLICY=r50-fluidcr-demo
export TRANSITION="$PWD/evidence/application-return-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$TRANSITION"
k() {
  local cfg
  case "$1" in
    mgmt) cfg="$MGMT_KUBECONFIG" ;;
    karmada) cfg="$KARMADA_KUBECONFIG" ;;
    aws) cfg="$AWS_KUBECONFIG" ;;
    onpre1) cfg="$ONPRE1_KUBECONFIG" ;;
    *) return 1 ;;
  esac
  shift
  kubectl --kubeconfig="$cfg" --request-timeout=30s "$@"
}
for cfg in "$MGMT_KUBECONFIG" "$KARMADA_KUBECONFIG" "$AWS_KUBECONFIG" "$ONPRE1_KUBECONFIG"; do
  test -s "$cfg"
  kubectl --kubeconfig="$cfg" config current-context
  kubectl --kubeconfig="$cfg" config view --minify \
    -o jsonpath='{.clusters[0].cluster.server}{"\n"}'
done
k karmada get clusters aws onpre1
k mgmt get deployments -A
k aws get nodes -o wide
k onpre1 get nodes -o wide
git rev-parse HEAD > "$TRANSITION/system.commit"
```

API server와 cluster 정체성을 확인한다. kubeconfig 원문, Secret, Bearer token,
registry 로그인 정보는 출력하거나 evidence/Git에 넣지 않는다. Deployment 백업에도
민감한 env/args가 있을 수 있으므로 비공개 보관하며 공유용 사본은 별도로 정제한다.
`kubernetes` context가 없거나 출력한 API가 MGMT가 아니면 중단하고 실제 MGMT kubeconfig의
절대 경로를 지정한다. Karmada 설정을 복사해 대신 쓰지 않는다. 위 임시 인증 파일은
runner/복구 명령이 모두 끝난 뒤 해당 파일만 삭제하며 evidence에 보관하지 않는다.

## 2. 데모 변경 전에 복구점 저장

이미 데모로 변경한 상태는 원래 설정의 증거가 아니다. 아래 백업은 변경 전에 한 번만
수행한다. 이전 백업을 덮어쓰지 않는다. 조회 실패를 빈 목록으로 간주하지 않는다.

```bash
for ctx in mgmt karmada aws onpre1; do
  mkdir "$TRANSITION/$ctx"
  k "$ctx" get deployments -A -o json > "$TRANSITION/$ctx/deployments.before.json"
  k "$ctx" get mutatingwebhookconfigurations -o json > "$TRANSITION/$ctx/mutating.before.json"
  k "$ctx" get validatingwebhookconfigurations -o json > "$TRANSITION/$ctx/validating.before.json"
  k "$ctx" get hpa -A -o json > "$TRANSITION/$ctx/hpa.before.json"
  jq '[.items[] | {namespace:.metadata.namespace,name:.metadata.name,uid:.metadata.uid,
      replicas:.spec.replicas,images:[.spec.template.spec.containers[] | {name,image}],
      initImages:[.spec.template.spec.initContainers[]? | {name,image}]}]' \
    "$TRANSITION/$ctx/deployments.before.json" > "$TRANSITION/$ctx/summary.json"
done
for ctx in aws onpre1; do
  k "$ctx" get nodes -o json > "$TRANSITION/$ctx/nodes.before.json"
  k "$ctx" -n "$NS" get pods -o json > "$TRANSITION/$ctx/pods.before.json"
done
k karmada -n "$NS" get trainingpolicies,spotriskprofiles -o json > "$TRANSITION/policies.before.json"
k karmada -n "$NS" get statefulsets,propagationpolicies,resourcebindings -o json \
  > "$TRANSITION/workloads.before.json"
k karmada -n "$NS" get restorerequests,spotreplacements,spotrecoveries -o json \
  > "$TRANSITION/operations.before.json"
k karmada get nodeprovisions -A -o json > "$TRANSITION/np-karmada.before.json"
k aws get nodeprovisions -A -o json > "$TRANSITION/np-aws.before.json"
(set -o noclobber; : > "$TRANSITION/touched-deployments.tsv")
(set -o noclobber; : > "$TRANSITION/touched-webhooks.tsv")
(set -o noclobber; : > "$TRANSITION/demo-created.tsv")
```

Deployment 전체 spec에는 replicas/이미지/args/env/volume이 포함된다. payload 및
group-control 이미지 인자, 데모 credential mount도 복구 대상이다. webhook 전체 등록에는
URL/service/CA bundle/rules/selectors/failurePolicy가 포함된다. TLS Secret은 유지한다.

변경 **전에** 아래 탭 구분 목록을 실제 inventory 이름으로 채운다. HPA/GitOps가 관리하는
Deployment는 그 관리 주체의 유지보수 절차를 먼저 적용한다.

- `touched-deployments.tsv`: API키, namespace, Deployment 이름.
- `touched-webhooks.tsv`: API키, `mutating` 또는 `validating`, 등록 이름.
- `demo-created.tsv`: API키, namespace(클러스터 범위는 `-`), resource 종류, 이름, UID.

```bash
# 실제 변경할 대상만 기록한다. PV/Stateful 이름은 inventory에서 선택한다.
printf 'mgmt\thybridspot-system\tpolicy-manager\n' >> "$TRANSITION/touched-deployments.tsv"
printf 'mgmt\thybridspot-system\tplacement-webhook\n' >> "$TRANSITION/touched-deployments.tsv"
# 다른 Deployment의 scale/image 변경도 같은 형식으로 먼저 추가한다.
# 등록을 변경/삭제할 때만 다음 행을 추가한다.
# printf 'karmada\tmutating\thybridspot-placement\n' >> "$TRANSITION/touched-webhooks.tsv"
```

System placement webhook의 Deployment는 MGMT, `hybridspot-placement` 등록은 Karmada다.
FluidCR/member restore webhook은 각 Member의 실행/등록을 확인한다. Fail webhook을
등록한 채 backend만 중지하면 admission이 막힐 수 있다. 전체 webhook 삭제 또는
일괄 failurePolicy 변경으로 우회하지 않는다.

기존 실험의 실제 config와 policy source도 보관한다. example은 실환경 설정이 아니다.

```bash
export EXPERIMENT_CONFIG="${EXPERIMENT_CONFIG:-$PWD/evaluation/config.r50.ckfull40k-risk60-fis80.local.json}"
test -s "$EXPERIMENT_CONFIG"
cp -- "$EXPERIMENT_CONFIG" "$TRANSITION/experiment-config.before.json"
python3 - "$EXPERIMENT_CONFIG" "$TRANSITION" <<'PY'
import json, pathlib, shutil, sys
config, out = map(pathlib.Path, sys.argv[1:])
c = json.loads(config.read_text())
source = (config.resolve().parent / c['policy_source']).resolve()
shutil.copyfile(source, out / 'experiment-policy-source.before.json')
PY
```

기존 실험 runner/FIS scheduler를 종료하고 활성 operation이 없는지 확인한 뒤 데모를
시작한다. `training.dcnlab.com/suspend=true`는 새 intent를 막을 뿐 진행 중 restore를
취소하지 않는다. 실패 실험은 evidence 보존 후 8절의 지원되는 abort 경로를 사용한다.
데모와 실험은 namespace를 공유하므로 **이름/UID**로 구분한다.

## 3. 복구 검증 후 UID 조건으로 데모 workload 삭제

`placement_stop.go`의 `holdStoppedPlacement`는 RB에 pending-placement,
placement-restore-request/UID 또는 application failover trigger가 남아 있거나,
policy의 group-restore-intent 또는 관련 RestoreRequest/RestorePlan이 존재하면
replicas 0 배치 변경을 거부할 수 있다. Verified 요청도 단순히 존재한다는 이유로
stop 경로를 막을 수 있다. 또한 `purgeMode: Never`, `suppressDeletion: true`로 남은
source Work는 현재 target placement와 달라 원본 scale-to-zero가 전파된다고 보장할 수 없다.

따라서 **scale StatefulSet 0을 종료 절차로 사용하지 않는다.** 복구가 Verified이고
실제 target 학습 진척까지 확인한 뒤 원본 StatefulSet과 연결된 RB/Work를 정상 삭제한다.
source fencing, RestoreRequest/RestorePlan, 안전 annotation은 양쪽 Member workload가
없어질 때까지 보존한다. annotation을 먼저 지워 admission을 우회하지 않는다.
검증 실패/진행 중 복구는 이 성공 종료 경로로 처리하지 않고 증거와 차단 상태를 유지한다.

### 3.1 정확한 복구 요청과 삭제 대상 보관

```bash
k karmada -n "$DEMO_NS" get trainingpolicy "$DEMO_POLICY" -o json > "$TRANSITION/demo-policy.final.json"
k karmada -n "$DEMO_NS" get statefulset "$DEMO_WORKLOAD" -o json > "$TRANSITION/demo-sts.final.json"
export DEMO_STS_UID=$(jq -er '.metadata.uid' "$TRANSITION/demo-sts.final.json")
export DEMO_POLICY_UID=$(jq -er '.metadata.uid' "$TRANSITION/demo-policy.final.json")
# 새로 만든 데모 원본이라는 생성 기록까지 확인한다. 기존 실험 객체는 대상이 아니다.
grep -Fqx -- "$(printf 'karmada\t%s\tstatefulset\t%s\t%s' "$DEMO_NS" "$DEMO_WORKLOAD" "$DEMO_STS_UID")" \
  "$TRANSITION/demo-created.tsv"
k karmada -n "$DEMO_NS" get resourcebindings -o json > "$TRANSITION/bindings.final.json"
jq -e --arg uid "$DEMO_STS_UID" --arg name "$DEMO_WORKLOAD" \
  '[.items[] | select(.spec.resource.kind=="StatefulSet" and
    .spec.resource.name==$name and .spec.resource.uid==$uid)] |
   if length==1 then .[0] else error("exact demo binding required") end' \
  "$TRANSITION/bindings.final.json" > "$TRANSITION/demo-rb.final.json"
export DEMO_RR=$(jq -er '.metadata.annotations["training.dcnlab.com/placement-restore-request"]' \
  "$TRANSITION/demo-rb.final.json")
export DEMO_RR_UID=$(jq -er '.metadata.annotations["training.dcnlab.com/placement-restore-request-uid"]' \
  "$TRANSITION/demo-rb.final.json")
k karmada -n "$DEMO_NS" get restorerequest "$DEMO_RR" -o json > "$TRANSITION/demo-rr.final.json"
jq -e --arg uid "$DEMO_RR_UID" --arg sts "$DEMO_STS_UID" --arg policy "$DEMO_POLICY_UID" '
  . as $r | .status.verification as $v |
  .metadata.uid==$uid and .metadata.deletionTimestamp==null and
  .metadata.labels["training.dcnlab.com/policy-uid"]==$policy and
  .spec.workloadRef.uid==$sts and .spec.groupRestore.sourceWorldUID==$sts and
  .spec.sourceCluster=="onpre1" and .spec.targetCluster=="aws" and
  .status.phase=="Verified" and .status.observedGeneration==.metadata.generation and
  $v.requestUID==$uid and ($v.operation | type)=="string" and ($v.operation | length)>0 and
  $v.operation==$r.spec.groupRestore.operationUID and
  ($v.checkpointID | type)=="string" and ($v.checkpointID | length)>0 and
  $v.checkpointID==$r.spec.checkpointRef.checkpointID and
  $v.sourceCluster==$r.spec.sourceCluster and $v.targetCluster==$r.spec.targetCluster and
  ($v.trainingRuntimeRef.name | type)=="string" and ($v.trainingRuntimeRef.name | length)>0 and
  ($v.trainingRuntimeRef.uid | type)=="string" and ($v.trainingRuntimeRef.uid | length)>0 and
  $v.trainingRuntimeRef==$r.spec.trainingRuntimeRef and
  ($v.verifiedAt | type)=="string" and ($v.verifiedAt | length)>0 and
  $v.sourceFenced==true and $v.sourceFence.fenced==true and
  $v.sourceFence.operation==$v.operation and
  ($v.sourceFence.evidenceID | type)=="string" and ($v.sourceFence.evidenceID | length)>0 and
  ($v.sourceFence.observedAt | type)=="string" and ($v.sourceFence.observedAt | length)>0
' "$TRANSITION/demo-rr.final.json" >/dev/null
```

RB에 요청 참조가 없거나 일치하지 않으면 최근 이름으로 추측하지 않고 실제 operation의
참조 관계를 먼저 확인한다. 위 검사는 저장된 Verified 신원 확인이며 실제 복원 검증을
대체하지 않는다. target Pod UID/checkpointID와 최소 세 번의 step 증가, PVMigration 완료,
source fence 증거를 보관한다. 다른 진행 중 checkpoint/restore/placement 작업이 있으면
삭제하지 않는다. 다음 조회에서 PVC 삭제 정책과 source/target Work 연결도 확인한다.

```bash
export DEMO_BINDING_ID=$(jq -er '.metadata.labels["resourcebinding.karmada.io/permanent-id"]' \
  "$TRANSITION/demo-rb.final.json")
k karmada get works -A -o json > "$TRANSITION/works.final.json"
# 해당 binding의 Work 전부와 같은 이름의 StatefulSet manifest를 가진 Work를 함께 검사한다.
jq --arg id "$DEMO_BINDING_ID" --arg ns "$DEMO_NS" --arg name "$DEMO_WORKLOAD" \
  '{items:[.items[] | select(.metadata.labels["resourcebinding.karmada.io/permanent-id"]==$id or
    any(.spec.workload.manifests[]?; .kind=="StatefulSet" and
      .metadata.namespace==$ns and .metadata.name==$name))]}' \
  "$TRANSITION/works.final.json" > "$TRANSITION/demo-works.final.json"
# 혼합 manifest/PVC/PV Work나 다른 binding이 있으면 자동 삭제하지 않고 중단한다.
jq -e --arg id "$DEMO_BINDING_ID" --arg ns "$DEMO_NS" --arg name "$DEMO_WORKLOAD" '
  (.items | length)>0 and all(.items[];
    .metadata.labels["resourcebinding.karmada.io/permanent-id"]==$id and
    (.spec.workload.manifests | length)>0 and all(.spec.workload.manifests[];
      .apiVersion=="apps/v1" and .kind=="StatefulSet" and
      .metadata.namespace==$ns and .metadata.name==$name and
      (.spec.persistentVolumeClaimRetentionPolicy.whenDeleted // "Retain")=="Retain"))
' "$TRANSITION/demo-works.final.json" >/dev/null
jq -e '(.spec.persistentVolumeClaimRetentionPolicy.whenDeleted // "Retain")=="Retain"' \
  "$TRANSITION/demo-sts.final.json" >/dev/null
for ctx in aws onpre1; do
  k "$ctx" -n "$DEMO_NS" get pods,fluidcrmigrations,restoreplans -o json \
    > "$TRANSITION/$ctx/demo.final.json"
  k "$ctx" -n "$DEMO_NS" get statefulset "$DEMO_WORKLOAD" --ignore-not-found -o json \
    > "$TRANSITION/$ctx/demo-sts.final.json"
  if test -s "$TRANSITION/$ctx/demo-sts.final.json"; then
    jq -e '(.spec.persistentVolumeClaimRetentionPolicy.whenDeleted // "Retain")=="Retain"' \
      "$TRANSITION/$ctx/demo-sts.final.json" >/dev/null
    jq -e --slurpfile works "$TRANSITION/demo-works.final.json" '. as $sts |
      any($works[0].items[];
        .metadata.name==$sts.metadata.annotations["work.karmada.io/name"] and
        .metadata.namespace==$sts.metadata.annotations["work.karmada.io/namespace"])' \
      "$TRANSITION/$ctx/demo-sts.final.json" >/dev/null
  fi
done
# 새 intent만 중지한다. 기존 안전 annotation은 그대로 유지한다.
k karmada -n "$DEMO_NS" get trainingpolicy "$DEMO_POLICY" -o json \
  > "$TRANSITION/demo-policy.suspend.json"
jq --arg uid "$DEMO_POLICY_UID" '
  [{op:"test",path:"/metadata/uid",value:$uid},
   {op:"test",path:"/metadata/resourceVersion",value:.metadata.resourceVersion},
   {op:"add",path:"/metadata/annotations",value:
     ((.metadata.annotations // {}) + {"training.dcnlab.com/suspend":"true"})}]
' "$TRANSITION/demo-policy.suspend.json" > "$TRANSITION/demo-suspend.patch.json"
k karmada -n "$DEMO_NS" patch trainingpolicy "$DEMO_POLICY" --type=json \
  --patch-file "$TRANSITION/demo-suspend.patch.json"
```

`whenDeleted: Delete` 또는 삭제 중인 PVC가 보이면 여기서 멈춘다. PV/PVC/NFS를 보존할
별도 절차가 필요하다. Member UID는 Karmada 원본 UID와 다르므로 각각 저장한 것을 사용한다.
source Member 객체/Work가 이전부터 없었다면 그 부재와 source Pod 종료 증거를 기록한다.
기존에 남아 있던 unrelated 객체를 데모 대상으로 추가하지 않는다.

### 3.2 원본, RB/Work, 잔존 Member 순서로 정상 삭제

아래 함수는 Kubernetes DELETE 본문에 **UID precondition**을 보낸다.
먼저 이름을 조회한 뒤 이름만 삭제하는 방식과 달리 재생성된 객체는 API가 거부한다.
`--raw`는 kubectl delete의 실제 옵션이며 삭제 완료 대기는 별도 조회로 수행한다.
권한/timeout/conflict 오류를 성공으로 간주하지 않는다. 로컬에서 만든 DeleteOptions에는
인증 정보를 넣지 않으며 kubectl의 기존 kubeconfig 인증을 사용한다.

```bash
delete_uid() {
  local ctx="$1" resource="$2" file="$3" ns name uid api uri live
  ns=$(jq -er '.metadata.namespace' "$file")
  name=$(jq -er '.metadata.name' "$file")
  uid=$(jq -er '.metadata.uid' "$file")
  api=$(jq -er '.apiVersion' "$file")
  live=$(k "$ctx" -n "$ns" get "$resource" "$name" --ignore-not-found -o json)
  test -n "$live" || return 0
  test "$(jq -r '.metadata.uid' <<< "$live")" = "$uid" || return 1
  if test "$api" = v1; then uri=/api/v1; else uri="/apis/$api"; fi
  uri="$uri/namespaces/$ns/${resource%%.*}/$name"
  jq -n --arg uid "$uid" '{apiVersion:"v1",kind:"DeleteOptions",
    propagationPolicy:"Foreground",preconditions:{uid:$uid}}' > "$file.delete-options.json"
  k "$ctx" delete --raw "$uri" -f "$file.delete-options.json" >/dev/null
}
wait_absent() {
  local ctx="$1" resource="$2" file="$3" ns name uid live
  ns=$(jq -er '.metadata.namespace' "$file")
  name=$(jq -er '.metadata.name' "$file")
  uid=$(jq -er '.metadata.uid' "$file")
  for attempt in $(seq 1 60); do
    live=$(k "$ctx" -n "$ns" get "$resource" "$name" --ignore-not-found -o json) || return 1
    test -n "$live" || return 0
    test "$(jq -r '.metadata.uid' <<< "$live")" = "$uid" || return 1
    sleep 5
  done
  echo "Deletion incomplete: $ctx/$ns/$resource/$name" >&2
  return 1
}
delete_uid karmada statefulsets.apps "$TRANSITION/demo-sts.final.json"
# 원본 DELETE가 접수된 뒤 연결된 정확한 RB도 삭제한다. annotation은 수정하지 않는다.
delete_uid karmada resourcebindings.work.karmada.io "$TRANSITION/demo-rb.final.json"
# Never로 보존된 source Work도 포함한다. 검증한 동일 binding의 STS 전용 Work만 대상이다.
for index in $(jq -r '.items | keys[]' "$TRANSITION/demo-works.final.json"); do
  file="$TRANSITION/demo-work-$index.json"
  jq --argjson i "$index" '.items[$i]' "$TRANSITION/demo-works.final.json" > "$file"
  delete_uid karmada works.work.karmada.io "$file"
done
wait_absent karmada resourcebindings.work.karmada.io "$TRANSITION/demo-rb.final.json"
for index in $(jq -r '.items | keys[]' "$TRANSITION/demo-works.final.json"); do
  wait_absent karmada works.work.karmada.io "$TRANSITION/demo-work-$index.json"
done
```

Karmada가 정상적으로 지운 객체는 NotFound로 통과한다. Work 보존 설정 때문에 남은
Member StatefulSet은 **재생성할 RB/Work가 없어졌음을 확인한 후** 해당 Member API에서
기록한 UID로 삭제한다. source fencing을 이때도 유지한다. 원본 foreground 삭제 완료를
기다리느라 아래 잔존 Member 정리를 건너뛰지 않는다.

```bash
# 새 UID의 RB/Work가 생겼다면 별도 소유자가 있으므로 자동으로 따라가며 지우지 않는다.
k karmada -n "$DEMO_NS" get resourcebindings -o json | jq -e --arg uid "$DEMO_STS_UID" \
  'all(.items[]; .spec.resource.uid!=$uid)' >/dev/null
k karmada get works -A -o json | jq -e --arg id "$DEMO_BINDING_ID" \
  --arg ns "$DEMO_NS" --arg name "$DEMO_WORKLOAD" 'all(.items[];
    .metadata.labels["resourcebinding.karmada.io/permanent-id"]!=$id and
    all(.spec.workload.manifests[]?; .kind!="StatefulSet" or
      .metadata.namespace!=$ns or .metadata.name!=$name))' >/dev/null
for ctx in aws onpre1; do
  file="$TRANSITION/$ctx/demo-sts.final.json"
  if test -s "$file"; then
    delete_uid "$ctx" statefulsets.apps "$file"
    wait_absent "$ctx" statefulsets.apps "$file"
  fi
done
wait_absent karmada statefulsets.apps "$TRANSITION/demo-sts.final.json"
stopped=false
for attempt in $(seq 1 60); do
  count=0
  for ctx in aws onpre1; do
    k "$ctx" -n "$DEMO_NS" get pods -o json > "$TRANSITION/$ctx/pods.stopped.json"
    n=$(jq --arg sts "$DEMO_WORKLOAD" '[.items[] | select(any(.metadata.ownerReferences[]?;
      .kind=="StatefulSet" and .name==$sts))] | length' "$TRANSITION/$ctx/pods.stopped.json")
    count=$((count+n))
    test -z "$(k "$ctx" -n "$DEMO_NS" get statefulset "$DEMO_WORKLOAD" --ignore-not-found -o name)"
  done
  if test "$count" -eq 0; then stopped=true; break; fi
  sleep 5
done
test "$stopped" = true
```

삭제 요청만으로 종료됐다고 판단하지 않는다. API 불통, Terminating Pod, 재생성, timeout은
실패다. 추가 Job/별도 writer와 snapshot의 Pod UID도 각각 대조해 종료를 확인한다.
양쪽 실제 Pod가 모두 사라지기 전에는 source cordon/fence와 RR/RestorePlan을 유지한다.
finalizer 강제 제거, grace-period 0, 안전 annotation 제거는 하지 않는다.

## 4. 종료 확인 후 operation 정리와 source 복귀

3절 통과 후에만 데모 전용 policy/서비스/종료된 operation을 정리한다. 해당 RR/RestorePlan의
source 차단 표식도 이때 지원되는 정상 controller cleanup에 맡긴다. 공유 annotation을
수동 일괄 제거하지 않는다. 추가 객체는 실제 데모 소유 UID를 확인한 ledger에 한정한다.
활성 policy의 SpotRecovery retirement 증거부터 지우지 않는다.

```bash
delete_demo_object() {
  local ctx="$1" ns="$2" resource="$3" name="$4" uid="$5" file
  case "$resource" in
    services|propagationpolicies|trainingpolicies|spotriskprofiles|restorerequests|restoreplans|fluidcrmigrations|secrets) ;;
    *) return 1 ;;
  esac
  grep -Fqx -- "$(printf '%s\t%s\t%s\t%s\t%s' "$ctx" "$ns" "$resource" "$name" "$uid")" \
    "$TRANSITION/demo-created.tsv"
  file="$TRANSITION/delete-$ctx-$resource-$name.json"
  # metadata만 저장한다. Secret data는 저장하거나 출력하지 않는다.
  k "$ctx" -n "$ns" get "$resource" "$name" -o json |
    jq '{apiVersion,kind,metadata:{name:.metadata.name,namespace:.metadata.namespace,uid:.metadata.uid}}' > "$file"
  test "$(jq -r '.metadata.uid' "$file")" = "$uid"
  delete_uid "$ctx" "$resource" "$file"
  wait_absent "$ctx" "$resource" "$file"
}
# ledger는 이 함수의 resource 복수형과 일치시킨다. 한 객체씩 종료 상태/소유권을 확인한다.
# delete_demo_object karmada "$DEMO_NS" trainingpolicies "$DEMO_POLICY" "$DEMO_POLICY_UID"
```

원래 존재하던 객체는 삭제하지 않고 저장한 설정으로 복구한다. 새로 만든
`application-failover-members` Secret은 6절에서 Deployment mount를 복구한 뒤 삭제한다.
기존 Secret/TLS Secret과 PVC/PV/NFS는 보존한다. operation cleanup이 실패하면 source를
계속 차단한 채 조사한다. **양쪽 Pod 종료 및 operation cleanup 완료 후에만** 아래를 실행한다.
데모 전부터 cordon된 노드 또는 UID가 바뀐 노드는 uncordon하지 않는다.

```bash
: "${SOURCE_NODE:?}"
source_uid=$(jq -er --arg n "$SOURCE_NODE" '.items[] | select(.metadata.name==$n) | .metadata.uid' \
  "$TRANSITION/onpre1/nodes.before.json")
test "$(k onpre1 get node "$SOURCE_NODE" -o jsonpath='{.metadata.uid}')" = "$source_uid"
if jq -e --arg n "$SOURCE_NODE" \
  'any(.items[]; .metadata.name==$n and (.spec.unschedulable // false)==false)' \
  "$TRANSITION/onpre1/nodes.before.json" >/dev/null; then
  k onpre1 uncordon "$SOURCE_NODE"
fi
```

Namespace/CRD 삭제, `delete --all`, finalizer stripping, `--force`, 공유 PVC/PV/NFS 삭제,
외부 Node 삭제는 금지한다. cleanup이 막히면 원인을 조사하고 증거를 유지한다.

### 다음 데모의 스토리지 이름과 operation UID

Existing 경로는 PVMigration Completed로 준비된 PV를 확인한 뒤, target Prepared 전에
VCT에서 target PVC를 CREATE-only로 생성한다. 이 PVC는 해당 operation의 requestUID로
표시되며 데모 종료 후에도 PVC/PV와 함께 보존한다. 이를 위해 PV 시스템 코드를 바꾸지는 않는다.
다음 데모가 보존된 claim 이름을 재사용하면 이전 claim의 operation/requestUID와 새 요청이
충돌할 수 있다. 기존 label을 새 UID로 바꾸거나 NFS 내용을 덮어써서 우회하지 않는다.

재실행에는 workload/VCT에서 파생되는 PVC/PV 이름과 실제 checkpoint/NFS 저장 경로를
이전 실행과 분리한다. 같은 이름을 꼭 써야 한다면 양쪽 writer 종료 및 사용 참조 부재를
확인하고, 먼저 데이터와 PVC/PV·operation 증거를 백업한 뒤 소유권이 확인된 데모 전용
스토리지만 별도 범위 제한 절차로 정리한다. 공유 dataset/NFS는 정리 대상이 아니다.
이 runbook은 PVC/PV/NFS를 자동 삭제하거나 초기화하지 않는다.

## 5. 선택: On-Demand quota 확보

전달받은 AWS GPU On-Demand quota는 **8 vCPU**이며 데모 `g5.xlarge` 두 대(각 4 vCPU)가
모두 점유한다. Provision 실험은 OD 1 + Spot 1을 새로 만들므로 여유 확인 전 시작하지 않는다.
Spot quota는 별도이며 OD 여유를 대체하지 않는다. 실제 quota/다른 G·VT 사용량과 복구 중
추가 용량도 확인한다.

```bash
aws service-quotas list-service-quotas --service-code ec2 --region ap-northeast-2 \
  --query 'Quotas[?contains(QuotaName, `G and VT`)].{Name:QuotaName,Value:Value,Code:QuotaCode}'
aws ec2 describe-instance-types --region ap-northeast-2 --instance-types g5.xlarge \
  --query 'InstanceTypes[].{Type:InstanceType,VCpu:VCpuInfo.DefaultVCpus}'
aws ec2 describe-instances --region ap-northeast-2 \
  --filters Name=instance-state-name,Values=pending,running,stopping,stopped \
  --query 'Reservations[].Instances[].{ID:InstanceId,Type:InstanceType,State:State.Name,Market:InstanceLifecycle,Tags:Tags}' \
  > "$TRANSITION/ec2.before-return.json"
```

용량을 유지하려면 quota 증가가 반영될 때까지 실험을 보류한다. 반환을 선택한 경우에도
**데모를 위해 생성한 NP/EC2임이 확인된 객체만** 소유 API를 통해 정상 삭제한다.
Karmada 원본에서 전파된 NP는 Karmada에서, AWS에 직접 생성한 NP는 AWS에서 삭제한다.
기존 외부 노드나 소유 원본을 모르는 노드는 삭제하지 않는다.

```bash
: "${NP_CTX:?}" "${NP_NS:?}" "${NP_NAME:?}" "${NP_UID:?}" "${INSTANCE_ID:?}"
case "$NP_CTX" in karmada|aws) ;; *) exit 1 ;; esac
k "$NP_CTX" -n "$NP_NS" get nodeprovision "$NP_NAME" -o json > "$TRANSITION/np-$NP_NAME.before-delete.json"
jq -e --arg uid "$NP_UID" --arg id "$INSTANCE_ID" \
  '.metadata.uid==$uid and .status.instanceId==$id' "$TRANSITION/np-$NP_NAME.before-delete.json"
aws ec2 describe-instances --region ap-northeast-2 --instance-ids "$INSTANCE_ID" \
  > "$TRANSITION/ec2-$INSTANCE_ID.before-delete.json"
```

NP owner/labels, member 전파 관계, instanceId/nodeName, EC2 tags를 대조한다. 해당 노드의
모든 사용자 workload와 restore/PV 작업이 종료됐다는 증거를 보관한다. 소유 policy도
종료해 NP가 재생성되지 않는 것을 확인한 다음 아래 명령을 하나씩 실행한다.

```bash
k "$NP_CTX" -n "$NP_NS" delete -f "$TRANSITION/np-$NP_NAME.before-delete.json" --wait=true --timeout=300s
aws ec2 wait instance-terminated --region ap-northeast-2 --instance-ids "$INSTANCE_ID"
aws ec2 describe-instances --region ap-northeast-2 --instance-ids "$INSTANCE_ID" \
  --query 'Reservations[].Instances[].{ID:InstanceId,State:State.Name}'
```

timeout이면 Provisioner/EC2 상태를 조사한다. finalizer 제거, 직접 Node 삭제, 강제 EC2
종료로 우회하지 않는다. 삭제 요청 성공만으로 quota 반환을 단정하지 않는다.

## 6. 기록한 Deployment와 webhook 복구

데모 직전 0이던 MGMT hybridspot은 0으로, 실행 중이던 PV/Stateful은 저장한 값으로 복구한다.
전체가 아닌 touched 목록만 변경한다. 아래 함수는 UID를 확인하고 저장한 전체 spec을
복원하여 이미지/args/credential mount/replicas를 함께 되돌린다.

```bash
restore_deployment() {
  local ctx="$1" ns="$2" name="$3" baseline="$4" file
  file="$TRANSITION/restore-$ctx-$ns-$name.json"
  jq -e --arg ns "$ns" --arg name "$name" \
    '[.items[] | select(.metadata.namespace==$ns and .metadata.name==$name)] |
     if length==1 then .[0] else error("baseline missing") end' "$baseline" > "$file"
  jq '[{op:"test",path:"/metadata/uid",value:.metadata.uid},
       {op:"replace",path:"/spec",value:.spec}]' "$file" > "$file.patch"
  k "$ctx" -n "$ns" patch deployment "$name" --type=json --patch-file "$file.patch"
  k "$ctx" -n "$ns" get deployment "$name" -o json > "$file.live"
  diff -u <(jq -S '.spec' "$file") <(jq -S '.spec' "$file.live")
  if test "$(jq -r '.spec.replicas' "$file")" -gt 0; then
    k "$ctx" -n "$ns" rollout status deployment/"$name" --timeout=300s
  fi
}
while IFS=$'\t' read -r ctx ns name; do
  test -n "$ctx" || continue
  restore_deployment "$ctx" "$ns" "$name" "$TRANSITION/$ctx/deployments.before.json"
done < "$TRANSITION/touched-deployments.tsv"
```

UID 변경이나 spec 차이가 있으면 원인을 확인한다. 데모에서 바꾼 Deployment metadata가
있다면 기록한 키만 원래 값/부재로 되돌린다. 다른 작업의 annotations를 지우지 않는다.
webhook은 backend/service/endpoints/TLS를 먼저 준비한 후 등록을 복구한다.

```bash
while IFS=$'\t' read -r ctx type name; do
  test -n "$ctx" || continue
  case "$type" in
    mutating) resource=mutatingwebhookconfiguration ;;
    validating) resource=validatingwebhookconfiguration ;;
    *) exit 1 ;;
  esac
  file="$TRANSITION/restore-$ctx-$type-$name.json"
  jq -e --arg n "$name" '[.items[] | select(.metadata.name==$n)] |
    if length==1 then .[0] else error("baseline missing") end' \
    "$TRANSITION/$ctx/$type.before.json" > "$file"
  live=$(k "$ctx" get "$resource" "$name" --ignore-not-found -o json)
  if test -n "$live"; then
    test "$(jq -r '.metadata.uid' <<< "$live")" = "$(jq -r '.metadata.uid' "$file")"
    jq --arg rv "$(jq -r '.metadata.resourceVersion' <<< "$live")" \
      'del(.metadata.managedFields,.metadata.creationTimestamp,.metadata.generation,.status) |
       .metadata.resourceVersion=$rv' "$file" > "$file.restore"
    k "$ctx" replace -f "$file.restore"
  else
    jq 'del(.metadata.uid,.metadata.resourceVersion,.metadata.managedFields,
            .metadata.creationTimestamp,.metadata.generation,.status)' "$file" > "$file.restore"
    k "$ctx" create -f "$file.restore"
  fi
  k "$ctx" get "$resource" "$name" -o json > "$file.live"
  diff -u <(jq -S '{webhooks,labels:.metadata.labels,annotations:.metadata.annotations}' "$file") \
    <(jq -S '{webhooks,labels:.metadata.labels,annotations:.metadata.annotations}' "$file.live")
done < "$TRANSITION/touched-webhooks.tsv"
```

데모에서 새로 추가한 webhook은 생성 ledger와 현재 UID를 대조하여 해당 등록만 개별 삭제한다.
TLS Secret이 바뀌었다면 옛 CA만 적용하지 않는다. 원래 backend가 0이었다면 이 단계는
직전 상태 복구일 뿐 admission 준비 완료가 아니다. 다음 실험용 정상 baseline이 필요하다.

## 7. experiment2 재개

### 7.1 일시 중지와 정상 실행 baseline 구분

직전 백업이 이미 일시 중지 상태라면 실행 replicas를 추측하지 않는다. 실험이 정상 실행되던
시점에 보관한 Deployment baseline과 대상 TSV를 지정한다. baseline이 없으면 정상 배포 설정을
확정할 때까지 재개하지 않는다. 일괄 `replicas=1`은 사용하지 않는다.

```bash
: "${EXPERIMENT_BASELINE:?}" "${EXPERIMENT_DEPLOYMENTS_TSV:?}"
# EXPERIMENT_BASELINE/<API키>/deployments.before.json 구조를 사용한다.
while IFS=$'\t' read -r ctx ns name; do
  test -n "$ctx" || continue
  restore_deployment "$ctx" "$ns" "$name" "$EXPERIMENT_BASELINE/$ctx/deployments.before.json"
done < "$EXPERIMENT_DEPLOYMENTS_TSV"
k mgmt -n hybridspot-system get deployments
k karmada -n "$NS" get trainingpolicies
k aws -n "$NS" get pods,nodeprovisions
```

이전 baseline의 이미지와 현재 CRD/runtime/payload 호환성도 확인한다. CRD를 구버전으로
덮어쓰지 않는다. 기존 policy는 자동 unsuspend하지 않고 새 run만 재개한다.
runner는 다른 활성 TrainingPolicy를 거부한다. webhook admission과 member 구성이 정상이어야 한다.

### 7.2 비교 조건과 코드 근거

| 항목 | 복귀 실험 설정 |
|---|---|
| 학습 | ResNet-50, batch 32, 40,000 steps, 기존 image/seed/dataset 유지 |
| VM | OD 1 + Spot 1, fixedOnDemand=1, 기본 Provision 경로(mode=Provisioned) |
| 위험률 | 시작 0.05/hour, 3600초(60분)에 0.8/hour로 한 번 변경 |
| FIS | 4800초(80분)에 통지 요청, PT2M action의 실제 중단은 이후 |
| F300 | minIntervalSeconds=maxIntervalSeconds=300초 |
| F1200 | minIntervalSeconds=maxIntervalSeconds=1200초 |
| D | 분석식 기반 60~3600초, 실제 관측값으로 결정 |

근거는 [run.py](../evaluation/run.py)의 render,
[checkpoint_suite.py](../evaluation/checkpoint_suite.py)의 configure/interrupt,
[ResNet 설정](../evaluation/config.resnet50.example.json),
[비교 가이드](checkpoint-resnet50-comparison-20261022.md)다.
기존 가이드/생성기의 22,000 step, 600·1800초 위험률, 1500초 FIS 기본값을 그대로 쓰지 않는다.
지원되는 `--steps`와 JSON 편집으로 이번 값을 지정한다. 존재하지 않는 CLI 옵션은 추가하지 않는다.

```bash
export CONFIG="$TRANSITION/experiment2.local.json"
jq -e '(.spec.capacity.mode // "Provisioned")=="Provisioned"' \
  "$TRANSITION/experiment-policy-source.before.json" >/dev/null
jq --arg source "$TRANSITION/experiment-policy-source.before.json" '.policy_source=$source' \
  "$TRANSITION/experiment-config.before.json" > "$TRANSITION/experiment2.base.json"
python3 evaluation/checkpoint_suite.py configure \
  --base "$TRANSITION/experiment2.base.json" --out "$TRANSITION/experiment2.generated.json" \
  --steps 40000 --timeout 21600 --scenario interruption
(set -o noclobber
 jq '.initial_risk=0.05 | .changed_risk=0.8 | .risk_change_after_seconds=3600 |
     .risk_schedule=[{after_seconds:3600,lambda_per_hour:0.8}]' \
   "$TRANSITION/experiment2.generated.json" > "$CONFIG")
jq -e '.model=="resnet50" and .goal_steps==40000 and .batch_size==32 and
       .scenario=="interruption" and .initial_risk==0.05 and
       .risk_schedule==[{after_seconds:3600,lambda_per_hour:0.8}] and .max_seconds==21600' "$CONFIG"
```

21600초(6시간)는 기존 full config와 같은 준비 포함 실행 상한이며 자동 VM 종료 시간이 아니다. 목표 step이 80분 전에
끝나면 유효한 FIS 비교가 아니다. 사전 실행으로 길이를 검증하고 조건 변경은 모든 arm에
동일하게 적용한다. 시간 기준은 `all_ranks_progress_observed`이며 VM 생성/Pod Running이 아니다.

### 7.3 새 run으로 순차 실행

예: `e2fast01/F300`, `e2dynamic01/D`, `e2slow01/F1200`. run-id는 1~17자이며
소문자로 시작하는 소문자/숫자/하이픈이다. 기존 evidence를 덮어쓰지 않는다.

```bash
export RUN=e2fast01 ARM=F300
export EVIDENCE="$PWD/evidence/checkpoint/$RUN"
python3 evaluation/checkpoint.py render --config "$CONFIG" \
  --run-id "$RUN" --arm "$ARM" --out "$EVIDENCE"
jq '{run_id,arm,model,goal_steps,initial_risk,risk_schedule}' "$EVIDENCE/run.json"
jq -e '(.spec.capacity.mode // "Provisioned")=="Provisioned" and .spec.policy.fixedOnDemand==1' \
  "$EVIDENCE/policy.yaml" >/dev/null
jq '.spec.checkpoint' "$EVIDENCE/policy.yaml"
k karmada apply --dry-run=server -f "$EVIDENCE/policy.yaml" >/dev/null
python3 evaluation/checkpoint.py prepare --evidence "$EVIDENCE"
python3 evaluation/checkpoint.py start --evidence "$EVIDENCE"
```

prepare 전에 arm별 min/max가 표와 일치하는지 확인한다. 별도 MGMT 터미널에서 동일한
kubeconfig/RUN/EVIDENCE를 설정한다. phase=running과 해당 policy UID의 Spot 한 대를 확인한다.
다음 interrupt 명령은 실제 선점 실행 승인이다. 데모 OD 노드는 FIS 대상이 아니다.

```bash
: "${FIS_ROLE_ARN:?}" "${FIS_ALARM_ARN:?}" "${RUN:?}" "${EVIDENCE:?}"
jq -e '.experiment=="checkpoint" and .phase=="running" and .scenario=="interruption"' "$EVIDENCE/run.json"
python3 evaluation/fis.py plan --evidence "$EVIDENCE" \
  --role-arn "$FIS_ROLE_ARN" --alarm-arn "$FIS_ALARM_ARN"
jq . "$EVIDENCE/fis-target.json"
jq . "$EVIDENCE/fis-template.json"
# 계정/region/Spot instance/stop alarm 확인 후 한 scheduler만 실행한다.
python3 evaluation/checkpoint_suite.py interrupt \
  --evidence "$EVIDENCE" --confirm "$RUN" \
  --after-seconds 4800 --tolerance-seconds 30 --wait-seconds 7200
```

위험률은 runner가 변경하므로 수동 risk 명령을 중복 실행하지 않는다. 30초 허용 구간을
놓쳤거나 fis-started.json이 있으면 재시도/시점 이동하지 않는다. 불명확한 API 응답은 실제
AWS experiment 상태로 확인한다. FIS 중지 요청이 이미 보낸 선점을 취소한다고 가정하지 않는다.

## 8. 결과 보관과 다음 arm

```bash
jq '{phase,failure_reason,analytic_interval_observed,risk_schedule_observations}' "$EVIDENCE/run.json"
tail -n 20 "$EVIDENCE/runner-events.jsonl"
k karmada -n "$NS" get trainingpolicy "eval-$RUN" -o json > "$EVIDENCE/policy.final.json"
k karmada -n "$NS" get restorerequests -o json > "$EVIDENCE/restores.final.json"
tar -czf "$TRANSITION/$RUN.before-cleanup.tar.gz" -C "$(dirname "$EVIDENCE")" "$RUN"
# 정상 완료 run만 다음 cleanup을 실행한다.
python3 evaluation/checkpoint.py cleanup --evidence "$EVIDENCE" --confirm "$RUN"
```

실패 run은 runner/FIS scheduler 종료와 evidence 보존 후 다음 경로를 사용한다.

```bash
python3 evaluation/checkpoint.py abort --evidence "$EVIDENCE" --confirm "$RUN"
python3 evaluation/checkpoint.py cleanup --evidence "$EVIDENCE" --confirm "$RUN"
```

cleanup은 run ledger/policy UID로 범위를 제한하고 PVC/PV를 보존한다. active operation/UID
변경 때문에 거부되면 강제 정리하지 않는다. 실제 EC2 종료 후 다음 arm을 시작한다.

완료 조건:

- 데모 writer가 양쪽에서 종료됐고 source scheduling 상태가 데모 전과 같다.
- 변경한 Deployment의 spec/replicas/이미지와 webhook이 선택한 baseline에 일치한다.
- 실험은 Provision으로 새 run을 사용하며 quota 여유와 OD 1 + Spot 1을 확인했다.
- F300/F1200의 실제 정기 저장 간격을 측정했다. D는 checkpoint-cost-analytic 출처와
  analytic_interval_observed=true를 확인했다. bootstrap만 유지되면 검증 실패다.
- 위험률 3600초 요청/소비, FIS 4800초 요청, 실제 중단, group RestoreRequest Verified,
  복원 후 step 증가, 이전 노드 정리 증거가 있다. 긴급/정기 checkpoint를 구분한다.
- Namespace/CRD/공유 NFS/외부 노드는 보존했고 다음 run 전에 VM 실제 종료를 확인했다.

누락된 검증은 누락으로 기록한다. 백업과 실험 evidence를 지우지 않는다.
