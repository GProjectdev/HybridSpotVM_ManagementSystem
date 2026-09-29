# 2026-09-29 재배포 및 검증 가이드

대상: 기존 MGMT + Karmada + AWS member, Buildah, Docker Hub,
2-rank GPU DDP, NFS 공유 checkpoint 구성.
이 문서는 과거 수동 status/lock 수정 절차를 대체한다. **실제 클러스터는 이번
코드 작업에서 조회하거나 변경하지 않았다.** 아래 이름/경로는 기존 실험 기준이다.
첫 단계에서 실제 설치와 대조한다. 현재 없는 컴포넌트에 set image를 실행하면
설치되지 않으므로 누락 설치를 먼저 해결해야 한다.

## 0. 범위와 중단 기준

- 검증 가능: AWS 신규 VM 준비, 초기 정책, runtime 계측, 주기 checkpoint/resume,
  계획된 Spot 교체, 복원 증거/삭제 게이트, 실패 시 안전 대기.
- 검증 불가: GPU 없는 온프레미스의 실제 DDP 이전. 별도 GPU 환경이 필요하다.
- 아직 미완료: whole-world 초기 자동 버스팅/원자적 예약, 완전한 논문 목적함수,
  typed load receipt, 자동 취소/보존 정리. 재설치만으로 완성되지 않는다.
- 진행 중 restore/partial pause가 있으면 업그레이드 중단. 수동 lock 삭제,
  rank/status/Verified 조작, finalizer 강제 제거 금지.
- 이 가이드는 운영 클러스터 전체 삭제나 기존 PVC 삭제를 수행하지 않는다.
  테스트는 새 namespace/새 workload identity로 수행한다.

관련 자료: [구현 상태](implementation-progress.md),
[아키텍처/PPT](architecture-workflows-ppt.md),
[기존 설치 구조](from-existing-mgmt-validation-guide.md).
기존 문서의 예전 SHA, 자동 TrainingPolicy 생성, 수동 우회 부분은 재사용하지 않는다.

## 1. MGMT에서 설치와 권한 확인

```bash
set -euo pipefail
: "${MGMT_KUBECONFIG:?}"
: "${KARMADA_KUBECONFIG:?}"
: "${AWS_KUBECONFIG:?}"
ROOT=/root/hybridspot-validation
SYS="$ROOT/System"
STATEFUL="$ROOT/stateful"
FLUID="$ROOT/fluidcr"
PROV="$ROOT/provisioner"
EVIDENCE="$ROOT/evidence/release-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$EVIDENCE"
for CFG in "$MGMT_KUBECONFIG" "$KARMADA_KUBECONFIG" "$AWS_KUBECONFIG"; do
  kubectl --kubeconfig="$CFG" config current-context
done
kubectl --kubeconfig="$KARMADA_KUBECONFIG" get clusters
kubectl --kubeconfig="$MGMT_KUBECONFIG" get deploy,ds -A -o wide
kubectl --kubeconfig="$AWS_KUBECONFIG" get deploy,ds -A -o wide
kubectl --kubeconfig="$AWS_KUBECONFIG" get nodes -o wide
kubectl --kubeconfig="$KARMADA_KUBECONFIG" get trainingpolicies,spotreplacements,restorerequests -A
kubectl --kubeconfig="$AWS_KUBECONFIG" get fluidcrmigrations,restoreplans -A
kubectl --kubeconfig="$AWS_KUBECONFIG" get nodeprovisionnetconfigs -A
kubectl --kubeconfig="$MGMT_KUBECONFIG" get deploy,ds -A -o yaml > "$EVIDENCE/mgmt-controllers.yaml"
kubectl --kubeconfig="$AWS_KUBECONFIG" get deploy,ds -A -o yaml > "$EVIDENCE/member-controllers.yaml"
```

배포 YAML에는 환경변수 등 민감한 설정이 있을 수 있으므로 evidence 디렉터리를
비공개로 보관한다. Secret/kubeconfig 내용은 출력하거나 Git에 추가하지 않는다.
위 백업은 롤백용 원본이며 그대로 전체 apply하는 용도가 아니다.

이전 실험의 NodeProvision/EC2가 남았는지 별도로 확인한다. kubectl Node의 Ready
상태나 Karmada CR 삭제만으로 EC2 종료를 판단하지 않는다.

기존 TrainingPolicy가 있다면 작업이 idle인지 확인한 뒤 다음 annotation으로
새 정책 결정을 중단할 수 있다. **진행 중 작업은 계속되며 전체 freeze가 아니다.**

```bash
# 존재하는 실험 이름으로 지정한 경우에만 실행
: "${OLD_NS:?}"
: "${OLD_POLICY:?}"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$OLD_NS" +  annotate trainingpolicy "$OLD_POLICY" training.dcnlab.com/suspend=true --overwrite
```

idle 상태에서 MGMT policy-manager/checkpoint-coordinator/spot-recovery-controller의
replicas를 기록 후 0으로 내리고, 업데이트가 끝나면 기록한 값으로 복구한다.
진행 중 작업을 중단시키기 위해 이 방법을 쓰지 않는다.
legacy hybridspot-management와 split 컴포넌트를 동시에 실행하지 않는다.

## 2. 실제 커밋을 가져오기

4개 checkout이 필요하다. Provisioner가 없다면 먼저 아래 URL의 같은 브랜치를
ROOT/provisioner로 clone한다. 기존 checkout의 origin을 확인하고, 다르면 중단한다.

| 디렉터리 | origin |
|---|---|
| System | https://github.com/GProjectdev/HybridSpotVM_ManagementSystem.git |
| stateful | https://github.com/GProjectdev/Stateful-Migration-Operator-with-PV.git |
| fluidcr | https://github.com/GProjectdev/My_FluidCR.git |
| provisioner | https://github.com/GProjectdev/PublicCloud-VM-Provisioner_test.git |

이 System 릴리스와 함께 검증한 지원 저장소 revision:

| 저장소 | full commit |
|---|---|
| Stateful | e2509345d9ef17207b092b8991579500fc6d5ae9 |
| FluidCR | b47aeaebfd2c9a82b63758378224a36e86595c22 |
| Provisioner | e893e1470e8384ea5c9511c292ef8e283e9887f3 |

System revision은 이 문서를 포함하는 커밋을 최종 작업 보고와 대조한다.

```bash
BRANCH=restore-automation-20260928
for DIR in "$SYS" "$STATEFUL" "$FLUID" "$PROV"; do
  test -d "$DIR/.git"
  test -z "$(git -C "$DIR" status --porcelain)" || {
    echo "STOP: dirty checkout $DIR"; exit 1;
  }
  test "$(git -C "$DIR" branch --show-current)" = "$BRANCH"
  git -C "$DIR" remote get-url origin
  git -C "$DIR" fetch origin "$BRANCH"
  git -C "$DIR" merge --ff-only "origin/$BRANCH"
  git -C "$DIR" log -1 --format='%H %s'
done
for DIR in "$SYS" "$STATEFUL" "$FLUID" "$PROV"; do
  printf '%s ' "$DIR"
  git -C "$DIR" rev-parse HEAD
done | tee "$EVIDENCE/source-revisions.txt"
```

최종 작업 보고의 push SHA와 대조한다. 이후 브랜치가 전진한 경우 해당 SHA가
현재 HEAD의 조상인지 확인하고 추가 변경도 검토한다. 존재하지 않는 예전
짧은 SHA로 빌드를 계속하거나 ancestry 검사를 생략하지 않는다.

## 3. 이미지 빌드와 digest 고정

MGMT에서 registry 로그인과 디스크 여유를 먼저 확인한다. 아래는 이미지를
게시하지만 클러스터에는 아직 적용하지 않는다.

```bash
REG=docker.io/jeongseungjun
RELEASE="realign-$(git -C "$SYS" rev-parse --short=12 HEAD)"
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
for COMPONENT in vm-spot-risk-collector policy-manager checkpoint-coordinator +  spot-recovery-controller placement-webhook training-runtime-collector spot-watcher; do
  build_push "$COMPONENT" "$REG/hybrid-spot-vm-system" "$SYS" +    --build-arg "COMPONENT=$COMPONENT"
done
build_push stateful "$REG/stateful-migration-operator" "$STATEFUL"
build_push provisioner "$REG/my-publiccloudvm-provisioner" "$PROV"
build_push webhook "$REG/myfluidcr-operator" "$FLUID" -f "$FLUID/Dockerfile.webhook"
build_push payload-base "$REG/myfluidcr-operator" "$FLUID" -f "$FLUID/Dockerfile.payload"
build_push payload-stateful "$REG/myfluidcr-operator" "$STATEFUL" +  -f "$STATEFUL/Dockerfile.payload-overlay" +  --build-arg "FLUIDCR_PAYLOAD_IMAGE=$(cat "$IMAGES/payload-base.image")"
build_push group-control "$REG/myfluidcr-operator" "$FLUID" -f "$FLUID/Dockerfile.group-control"
```

System은 COMPONENT별 다른 실행 파일이다. policy-manager 이미지에 Stateful의
--mode 인자를 주지 않는다. Stateful만 한 이미지에서 --mode를 구분한다.

## 4. CRD, RBAC, RIC 순서

Karmada API에 적용하는 CRD/RBAC와 실제 controller Pod가 있는 MGMT API를 혼동하지 않는다.
기존 인증서/Secret/NodePort/NFS/PV 설정은 보존한다. 전체 kustomize apply로
dev 이미지나 샘플 주소를 덮어쓰지 않는다.

```bash
for CFG in "$KARMADA_KUBECONFIG" "$AWS_KUBECONFIG"; do
  kubectl --kubeconfig="$CFG" apply -k "$SYS/config/crd/"
  kubectl --kubeconfig="$CFG" apply -k "$STATEFUL/config/crd/"
  kubectl --kubeconfig="$CFG" apply +    -f "$PROV/config/crd/bases/ml.dcn.ssu.ac.kr_nodeprovisions.yaml" +    -f "$PROV/config/crd/bases/ml.dcn.ssu.ac.kr_nodeprovisionnetconfigs.yaml"
  kubectl --kubeconfig="$CFG" wait --for=condition=Established +    crd/trainingpolicies.training.dcnlab.com crd/trainingruntimes.training.dcnlab.com +    crd/fluidcrmigrations.fluidcr.dcnlab.com --timeout=120s
done
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f "$SYS/config/karmada/access.yaml"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply +  -f "$SYS/config/karmada/runtime-interpreter.yaml" +  -f "$SYS/config/runbook/nodeprovision-status.yaml" +  -f "$STATEFUL/config/karmada/ric/restoreplan_resource_interpreter.yaml" +  -f "$STATEFUL/config/karmada/ric/fluidcrmigration_resource_interpreter.yaml"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply +  -f "$STATEFUL/config/karmada/role.yaml" -f "$STATEFUL/config/karmada/binding.yaml"
kubectl --kubeconfig="$AWS_KUBECONFIG" apply -f "$SYS/config/member/rbac.yaml"
for AREA in checkpoint member; do
  kubectl --kubeconfig="$AWS_KUBECONFIG" apply +    -f "$STATEFUL/config/$AREA/role.yaml" -f "$STATEFUL/config/$AREA/binding.yaml"
done
kubectl --kubeconfig="$AWS_KUBECONFIG" apply +  -f "$STATEFUL/config/member/artifact-role.yaml" +  -f "$STATEFUL/config/member/artifact-binding.yaml"
kubectl --kubeconfig="$AWS_KUBECONFIG" auth can-i create pods --subresource=exec +  --as=system:serviceaccount:stateful-migration-system:stateful-checkpoint +  -n fluidcr-demo
```

마지막 권한 확인의 기대값은 yes다. 다른 namespace/ServiceAccount라면 실명으로 바꾼다.
CRD 적용은 기존 status를 새 증거로 변환하지 않는다. 이전 operation을 이름만으로 재사용하지 않는다.

## 5. Controller / payload 更新

각 Deployment/DS의 실제 이름, container명, args를 1절에서 확인한다.
아래는 기존 구성의 이름이며 실패하면 다음 단계로 진행하지 않는다.

```bash
set_image() {
  local CFG="$1" NS="$2" KIND="$3" NAME="$4" CONTAINER="$5" KEY="$6"
  kubectl --kubeconfig="$CFG" -n "$NS" set image "$KIND/$NAME" +    "$CONTAINER=$(cat "$IMAGES/$KEY.image")"
  kubectl --kubeconfig="$CFG" -n "$NS" rollout status "$KIND/$NAME" --timeout=600s
}
set_image "$MGMT_KUBECONFIG" stateful-migration-system deployment stateful-management manager stateful
set_image "$AWS_KUBECONFIG" stateful-migration-system deployment stateful-checkpoint manager stateful
set_image "$AWS_KUBECONFIG" stateful-migration-system deployment stateful-member manager stateful
set_image "$AWS_KUBECONFIG" stateful-migration-system daemonset stateful-artifact verifier stateful
set_image "$AWS_KUBECONFIG" remote-cluster-provisioner-system deployment remote-cluster-provisioner manager provisioner
set_image "$AWS_KUBECONFIG" fluidcr-system deployment fluidcr-webhook webhook webhook

# 다른 인자를 보존하고 대상 flag만 수정한다. resourceVersion 충돌 시 중단한다.
update_arg() {
  local NS="$1" DEPLOY="$2" CONTAINER="$3" FLAG="$4" VALUE="$5"
  local OBJ PATCH
  OBJ="$(kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get deployment "$DEPLOY" -o json)"
  PATCH="$(printf '%s' "$OBJ" | jq -ce --arg c "$CONTAINER" --arg flag "$FLAG" --arg value "$VALUE" '
    . as $d | [.spec.template.spec.containers | to_entries[] | select(.value.name==$c)] as $m |
    select(($m|length)==1) | $m[0] as $c |
    ($c.value.args // []) as $a |
    select(all($a[]; . != $flag)) |
    [
      {op:"test",path:"/metadata/resourceVersion",value:$d.metadata.resourceVersion},
      {op:"add",path:("/spec/template/spec/containers/"+($c.key|tostring)+"/args"),
       value:([$a[] | select(startswith($flag+"=")|not)] + [$flag+"="+$value])}
    ]')"
  kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" patch deployment "$DEPLOY" --type=json -p "$PATCH"
  kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" rollout status deployment/"$DEPLOY" --timeout=600s
}
update_arg fluidcr-system fluidcr-webhook webhook --payload-image "$(cat "$IMAGES/payload-stateful.image")"
update_arg stateful-migration-system stateful-member manager --group-control-image "$(cat "$IMAGES/group-control.image")"

for COMPONENT in vm-spot-risk-collector policy-manager checkpoint-coordinator spot-recovery-controller placement-webhook; do
  set_image "$MGMT_KUBECONFIG" hybridspot-system deployment "$COMPONENT" manager "$COMPONENT"
done
set_image "$AWS_KUBECONFIG" hybridspot-system deployment training-runtime-collector manager training-runtime-collector
set_image "$AWS_KUBECONFIG" hybridspot-system daemonset spot-watcher manager spot-watcher
```

flag/value가 두 요소로 나뉜 형식이면 update_arg가 중단한다. 기존 형식을 확인하고
해당 인자를 개별 수정한다. 신구 Coordinator/Replacement 혼재를 해소한 뒤
기록한 replicas로 복귀한다. replicas=0의 rollout 성공은 기동 검증이 아니다.
복귀 후 readiness, restartCount, actual imageID, args를 다시 확인한다.

**기존 trainer Pod 안의 Python 코드는 자동 업데이트되지 않는다.**
새 테스트 workload를 생성해 새 payload를 주입받는다. 이전 checkpoint를 가져와
새 runtime의 성공 증거로 사용하지 않는다.

## 6. Provisioner 확인

실제 사용하는 NodeProvisionNetConfig의 nodeSoftware가 StatefulMigration이고,
migrationRuntime.certifyRestore=true인지 확인한다. 인증 옵션은 검토된
restore-runtime package와 그 SHA/commit 설정이 준비되었을 때만 활성화한다.
설정 예시는 Provisioner config/samples/aws-vpc-netconfig-stateful-1.37.yaml,
패키지 계약은 Provisioner docs/restore-runtime-rollout.md를 참조한다.

이번 변경은 신규 노드 검증 시 NVIDIA driver root를 linker 설정에 연결하고
clean environment에서 libcuda/libnvidia-ml 로딩 및 cuda-checkpoint --help를 검사한다.
실패하면 능력 라벨을 새로 부여하지 않는다. 이미 Ready인 노드의 바이너리/설정을
자동 재설치하거나 예전 인증 라벨을 재검증했다고 간주하지 않는다.

```bash
kubectl --kubeconfig="$AWS_KUBECONFIG" get nodes -o json |
jq '[.items[] | {name:.metadata.name,uid:.metadata.uid,
  capability:.metadata.labels["migration.dcnlab.com/restore-from-file"],
  runtime:.status.nodeInfo.containerRuntimeVersion,
  ready:[.status.conditions[]|select(.type=="Ready")]}]'
kubectl --kubeconfig="$AWS_KUBECONFIG" -n remote-cluster-provisioner-system +  logs deployment/remote-cluster-provisioner -c manager --since=15m --tail=150
```

라벨만 true로 patch하지 않는다. 새 NodeProvision Ready, actual runtime/hash/config,
GPU allocatable, artifact agent Ready를 함께 확인한다.

## 7. 새 2-rank DDP와 사용자 CR

아래는 과거 3개 StatefulSet 예제가 아닌, 하나의 StatefulSet/2개 rank 구성이다.
기존 검증 학습 코드를 ConfigMap으로 사용한다. 새 namespace의 PVC는 새로 생성하며
이전 archive/lock을 복사하지 않는다. NFS StorageClass, GPU RuntimeClass,
namespace의 AWS credentials/NetConfig 전파 및 artifact store 접근은 먼저 준비한다.
credential Secret은 기존 보안 설치 절차로 만들고 출력/문서에 키를 기록하지 않는다.
이 단계부터 실제 VM 비용이 발생한다.

```bash
export NS="fluidcr-realign-$(date -u +%H%M%S)"
export APP=trainer POLICY_NAME=trainer-realign RUNTIME_NAME=trainer-realign-runtime
export SOURCE_CLUSTER=aws AWS_CLUSTER=aws
export STATEFUL FLUID EVIDENCE
export TRAINER_IMAGE=docker.io/pytorch/pytorch:2.4.1-cuda12.1-cudnn9-runtime
export GPU_RUNTIME_CLASS=""  # 기존 정상 GPU Pod의 runtimeClassName; 없으면 빈 값
export STORAGE_CLASS=nfs-client
for CFG in "$KARMADA_KUBECONFIG" "$AWS_KUBECONFIG"; do
  kubectl --kubeconfig="$CFG" create namespace "$NS"
done
python3 - <<'PY'
import json, os
from pathlib import Path
import yaml
ns = os.environ["NS"]
docs = list(yaml.safe_load_all((Path(os.environ["STATEFUL"]) /
    "config/samples/two-replica/workload.yaml").read_text()))
items = [d for d in docs if d["kind"] in ("Service", "StatefulSet")]
for d in items:
    d["metadata"]["namespace"] = ns
sts = next(d for d in items if d["kind"] == "StatefulSet")
pod = sts["spec"]["template"]["spec"]
pod.pop("runtimeClassName", None)
if os.environ["GPU_RUNTIME_CLASS"]:
    pod["runtimeClassName"] = os.environ["GPU_RUNTIME_CLASS"]
pod["automountServiceAccountToken"] = False
pod["nodeSelector"] = {"ml.dcn.ssu.ac.kr/provider": "AWS"}
pod["affinity"] = {"podAntiAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": [{
    "labelSelector": {"matchLabels": {"app": "trainer"}},
    "topologyKey": "kubernetes.io/hostname"}]}}
sts["spec"]["template"]["metadata"]["annotations"]["fluidcr.dcnlab.com/checkpoint-claim"] = "fluidcr-checkpoint-shared"
c = pod["containers"][0]
c["image"] = os.environ["TRAINER_IMAGE"]
c["command"] = ["python", "-u", "/workspace/train_ddp.py"]
c["resources"]["limits"]["memory"] = "14Gi"
c["resources"]["limits"]["cpu"] = "4"
for e in c["env"]:
    if e["name"] == "FLUIDCR_DISTRIBUTED":
        e["value"] = "1"
c["env"] += [
    {"name": "RANK", "valueFrom": {"fieldRef": {"fieldPath": "metadata.labels['apps.kubernetes.io/pod-index']"}}},
    {"name": "WORLD_SIZE", "value": "2"}, {"name": "LOCAL_RANK", "value": "0"},
    {"name": "MASTER_ADDR", "value": f"trainer-0.trainer.{ns}.svc.cluster.local"},
    {"name": "MASTER_PORT", "value": "29500"}, {"name": "NCCL_SOCKET_IFNAME", "value": "eth0"}]
c["volumeMounts"] = [{"name": "rank-data", "mountPath": "/data"},
    {"name": "training-script", "mountPath": "/workspace"}]
pod["volumes"] = [{"name": "training-script", "configMap": {"name": "fluidcr-ddp-train-script"}}]
sts["spec"]["volumeClaimTemplates"] = [{"metadata": {"name": "rank-data"}, "spec": {
    "accessModes": ["ReadWriteMany"], "storageClassName": os.environ["STORAGE_CLASS"],
    "resources": {"requests": {"storage": "5Gi"}}}}]
cm = yaml.safe_load((Path(os.environ["FLUID"]) / "examples/ray/with-fluidcr/training-script.yaml").read_text())
cm["metadata"]["namespace"] = ns
items.append(cm)
items.append({"apiVersion": "policy.karmada.io/v1alpha1", "kind": "PropagationPolicy",
    "metadata": {"name": "trainer-support", "namespace": ns}, "spec": {
    "resourceSelectors": [{"apiVersion": "v1", "kind": "Service", "name": "trainer"},
                          {"apiVersion": "v1", "kind": "ConfigMap", "name": "fluidcr-ddp-train-script"}],
    "placement": {"clusterAffinity": {"clusterNames": ["aws"]}}}})
Path(os.environ["EVIDENCE"], "workload.json").write_text(json.dumps(
    {"apiVersion": "v1", "kind": "List", "items": items}, indent=2))
PY
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: fluidcr-checkpoint-shared
spec:
  accessModes: [ReadWriteMany]
  storageClassName: $STORAGE_CLASS
  resources:
    requests:
      storage: 50Gi
EOF
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f "$EVIDENCE/workload.json"
export WORKLOAD_UID="$(kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get sts trainer -o jsonpath='{.metadata.uid}')"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" label sts trainer +  "training.dcnlab.com/workload-uid=$WORKLOAD_UID" --overwrite
envsubst < "$SYS/config/samples/20-workload-propagationpolicy.yaml" |
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f -
envsubst < "$SYS/config/samples/11-spot-risk-profile-static.yaml" |
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f -
envsubst < "$SYS/config/samples/12-training-policy.yaml" > "$EVIDENCE/training-policy.yaml"
```

training-policy.yaml의 __REPLACE 값은 실제 region/AZ/g5.xlarge/AMI/VPC/subnet/SG/
IAM profile/credentialsRef로 채운다. 이전 실험 값을 무검증 재사용하지 않는다.
새 namespace에도 해당 credential 및 NetConfig 계약이 유효해야 한다.
계획 교체 시험을 위해 spec.replacement.enabled=true를 추가한다.
runtime 및 runtime propagation CR은 직접 만들지 않는다. Policy Manager가 생성한다.

```bash
if grep -n '__REPLACE' "$EVIDENCE/training-policy.yaml"; then
  echo "STOP: fill cloud inputs"; exit 1
fi
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply --dry-run=server -f "$EVIDENCE/training-policy.yaml"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f "$EVIDENCE/training-policy.yaml"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingpolicy "$POLICY_NAME" -o json |
  jq '{spec:.spec,discovery:.status.discovery,policy:.status.policy}'
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingruntimes,nodeprovisions
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get pods,pvc -o wide
```

GPU Operator/device plugin, restore package가 신규 VM에도 준비되고 artifact agent가
배치되어야 한다. 기존 문서 7.1의 설치/노드 selector 점검을 참고하되 기존 GPU
Operator를 중복 설치하지 않는다. source/target 간 공유 파일 store와 PVMetadata
경로도 기존 PV 시스템에서 준비되어야 한다.

## 8. 합격 기준과 관찰 명령

### A. 초기 배포와 계측

- 계측 전 Bootstrap이며 VM 생성이 진행된다. checkpoint cost가 없다는 이유로 멈추지 않는다.
- static 0.05는 실험 위험 입력이다. Collector 학습 예측/가격 조회의 성공 증거가 아니다.
- Ready rank 2, worldSize 2, rank 0/1, 서로 다른 worker, step 증가.
- 새 workerSession과 같은 window의 rank 평균 최대값으로 iterationTimeSeconds가 생성된다.
- Runtime 생성 주체는 사용자 아닌 Policy Manager. TrainingPolicy spec은 관측값으로 재작성되지 않는다.

```bash
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" wait pod/trainer-0 pod/trainer-1 +  --for=condition=Ready --timeout=1200s
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingruntime "$RUNTIME_NAME" -o json |
  jq '{spec:.spec,status:.status}'
for POD in trainer-0 trainer-1; do
  kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" logs "$POD" -c trainer +    --timestamps --since=2m --tail=20
done
```

VM/GPU 준비가 오래 걸리면 timeout을 반복 늘리기 전에 NodeProvision phase,
Provisioner log, cloud-init, GPU allocatable과 Pending 이유를 조사한다.

### B. 주기 checkpoint 두 회 연속

```bash
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get fluidcrmigrations +  -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,MESSAGE:.status.message' --watch
```

두 개의 새로운 round 각각 Completed, 파일/hash/export 증거, 자동 in-place resume,
두 rank step 증가를 확인한다. 재개 직후 Ready만 보고 합격하지 않는다.
체크포인트 API를 수동 호출하거나 lock을 지우지 않는다.
기존 failed CR이 남는 것은 이력 보존일 수 있다. 자동 retention 완성으로 설명하지 않는다.

### C. 계획 교체

이 절은 **실제 VM 생성/학습 중단/기존 VM 삭제**를 유발할 수 있는 시험이다.
앞 단계 합격, survivor 정상, 대상 runtime 인증/store 준비 및 비용 한도를 확인한 뒤 실행한다.

```bash
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get pods -l app=trainer -o json > "$EVIDENCE/pods-before.json"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get nodeprovisions -o json > "$EVIDENCE/nodes-before.json"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" patch spotriskprofile trainer-risk +  --type=merge -p '{"spec":{"staticLambdaPerHour":0.5}}'
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get spotreplacements,restorerequests +  -o custom-columns='KIND:.kind,NAME:.metadata.name,PHASE:.status.phase,MESSAGE:.status.message' --watch
```

새 operation UID를 기록하고 다음을 순서대로 확인한다.

1. fresh risk observedGeneration, 하나의 UID-bound SpotReplacement.
2. Coordinator만 해당 operation의 FluidCRMigration을 생성하며 이전 attempt를 채택하지 않음.
3. archive Completed가 후속 survivor 재개 대기로 Pending으로 회귀하지 않음.
4. target SHA verified, source UID fencing, replacement Pod의 새 UID/대상 노드.
5. restore-owned 재개, 전체 rank의 checkpointID 일치와 step 증가.
6. 현재 generation/UID/checkpoint/runtime에 연결된 RestoreRequest 검증 결과.
7. 삭제 게이트 통과 후 이전 NodeProvision 처리와 실제 EC2 terminated 확인.

Running만으로 5~7을 대체하지 않는다. survivor 재시작, pause 증거 소실, 이전
세대의 검증 결과에서는 안전 대기를 기대하며 status를 변경해 진행시키지 않는다.
신규 노드 준비는 20분을 조사 시점으로 삼고 멈춘 단계의 events/log를 보관한다.

### D. 경제성과 논문 경로

- 실제 feed/가격 없는 static 시험에서 priceEvaluated나 학습 예측을 성공으로 표시하지 않는다.
- risk-feed.md의 identity/freshness 계약을 만족하는 feed로 가격/예측을 별도 검증한다.
- economics.enabled=true, 수동 lossCost 생략 시 현재 Verified 복원과 평가된
  checkpoint 간격이 모두 있어야 proxy 보정을 사용한다. Verified가 없으면 기다리는 것이 맞다.
- paperProfile에는 실제 측정한 async GPU→DRAM/storage/size/buffer만 넣는다.
  동기 CRIU 시간을 대신 입력하지 않는다. 적격 입력이 없으면 Bootstrap을 지속한다.
- 기존 risk-band/bootstrap은 논문식의 누락 항을 0으로 만든 식이 아니다.
  현재 Eq.1~5 구현과 완전한 Eq.6~7 목적함수는 구분해서 보고한다.

## 9. 증거 수집 및 PPT 캡처

관리 터미널에서 실행하되 member 상태는 AWS_KUBECONFIG, 집계/정책은
KARMADA_KUBECONFIG로 조회한다. 시간과 전체 message를 잘라내지 않는다.

```bash
date -u
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" +  get trainingpolicies,trainingruntimes,spotriskprofiles,spotreplacements,restorerequests,nodeprovisions +  -o json > "$EVIDENCE/control.json"
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" +  get pods,statefulsets,pvc,fluidcrmigrations,restoreplans,nodeprovisions +  -o json > "$EVIDENCE/member.json"
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get events +  --sort-by=.metadata.creationTimestamp > "$EVIDENCE/events.txt"
for POD in trainer-0 trainer-1; do
  kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" logs "$POD" -c trainer +    --timestamps --since=10m > "$EVIDENCE/$POD.log"
done
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get pods -l app=trainer +  -o custom-columns='NAME:.metadata.name,UID:.metadata.uid,NODE:.spec.nodeName,READY:.status.containerStatuses[0].ready,RESTARTS:.status.containerStatuses[0].restartCount'
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" +  get spotreplacements,restorerequests +  -o custom-columns='KIND:.kind,NAME:.metadata.name,PHASE:.status.phase,MESSAGE:.status.message'
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get restoreplans -o json |
  jq '[.items[]|{name:.metadata.name,uid:.metadata.uid,generation:.metadata.generation,
    phase:.status.phase,message:.status.message,targets:.spec.pods,
    pods:.status.pods,artifacts:.status.artifacts,sourceFences:.status.sourceFences}]'
```

캡처 1: 교체 전/후 Pod UID·노드·restartCount.
캡처 2: checkpointID·SHA·새 operation 및 source fencing.
캡처 3: 양 rank의 복원 후 step 증가와 runtime observation.
캡처 4: RestoreRequest의 검증 상태와 provider 종료 결과.
4가 미완료라면 “교체 노드에서 학습 재개 확인, 자동 복원 검증/정리 미완료”로 쓴다.

## 10. 실패 분류

| 증상 | 우선 확인 | 금지할 우회 |
|---|---|---|
| Not a valid object name | origin/branch/full SHA/fetch 결과 | 존재하지 않는 SHA로 이미지 이름만 붙여 배포 |
| CrashLoopBackOff | previous log, 종료 코드, COMPONENT 빌드와 args | 모든 이미지에 --mode 추가 |
| runtime 503 / worker 없음 | workerSession, registry, API/학습 자식 프로세스, payload 버전 | 학습 중 임의 PID 등록/lock 삭제 |
| rank 0 누락 | 새 CRD의 rank 직렬화 및 새 checkpoint 상태 | 오래된 status를 증거 없이 patch |
| archive fields incomplete | export/hash/durableRef, artifact agent/store | 파일 경로만 보고 복원 승인 |
| restore capability 없음 | 신규 노드 인증, GPU library/package/hash/config | capability 라벨 강제 true |
| Prepared, Ready=false | restored launcher lock, restore-owned 재개, member log | 일반 /resume로 partial 작업 해제 |
| AwaitingCheckpoint/Verification | UID/generation/RIC/현재 session 증거 | Running을 Verified로 바꾸기 |
| capacity_overshoot | policy UID 소유 NodeProvision/이전 replacement | 학습 중 VM 직접 종료 |

## 11. 시험 종료와 보존

현재 suspend는 취소/종료 기능이 아니다. 기존 attempt가 끝났는지 먼저 확인한다.
새 실험 namespace에만 작업하고 provider 삭제를 수행하는 controller를 먼저 내리지 않는다.

1. evidence를 저장하고 해당 TrainingPolicy를 suspend한다.
2. 활성 replacement/restore가 있으면 완료 또는 명시적으로 검토한 취소 절차 전까지 중단한다.
3. 새 workload의 StatefulSet을 Karmada에서 명시적 이름으로 삭제하고 member Pod 종료를 확인한다.
4. 해당 TrainingPolicy를 삭제해 재생성을 막는다.
5. evidence에 기록한 이 실험 operation/request/plan/checkpoint/runtime/risk 이름만 삭제한다.
6. 이 policy UID가 소유한 NodeProvision 이름과 EC2 instance ID를 대조한 후 명시적 삭제한다.
7. member finalizer 완료, 실제 EC2 terminated, 잔여 Work/Binding/Node를 확인한다.
8. PVC/PV/NFS archive는 기본 보존한다. 데이터 삭제는 참조가 없다는 확인과 별도 삭제 결정 후 수행한다.

NodeProvision이 남으면 deletionTimestamp/finalizers/Provisioner log/AWS 오류를 먼저
확인한다. --force, finalizer 제거, namespace 전체 삭제로 VM 종료를 대체하지 않는다.
자동 terminal-CR 보존 기간과 일괄 cleanup은 아직 완성되지 않았다.

## 12. 로컬 검증과 남은 통합 검증

릴리스 코드 검증: 세 Go 저장소 전체 unit/fake-client 테스트, vet/build,
FluidCR runtime/group/timing/load 테스트, 가이드 Bash 문법 검사.
이 PC에는 torch/pytest/CUDA가 없어 전체 Python/GPU E2E는 수행하지 못한다.
Mock checkpoint-load 테스트는 실제 tensor/optimizer/RNG 복원 정확도 시험을 대체하지 않는다.
본 가이드를 통해 새로운 이미지/CRD 배포 후 AWS 결과를 수집해야 한다.
