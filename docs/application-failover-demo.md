# ResNet-50 Application Failover: onpre1 -> AWS

## 범위와 실행 조건

기존 namespace `fluidcr-realign-121040`, 단일 rank ResNet-50, 미리 만든 OnDemand
GPU 노드를 사용한다. EC2 신규 생성, FIS 선점, Spot 할당은 수행하지 않는다.

데모는 애플리케이션 checkpoint에서 복구한다. 선택한 checkpoint 이후 작업은 다시
실행될 수 있으므로 장애 직전까지 무손실로 이어진다고 설명하지 않는다. 영상에는
checkpoint 식별자, source Pod 종료, Karmada의 실제 대상 선택, 복원 후 step 증가가 보여야 한다.

source Pod를 수동 삭제하지 않는다. 명시적인 데모 trigger가 기존 source RestorePlan
컨트롤러에 정확한 UID의 Pod 정상 삭제를 요청한다. cordon만으로 Karmada cluster failover가
발생하지 않는다. 양쪽 Member가 Ready인 상태에서 Karmada **application failover**를 사용한다.

구현 및 fake-client 테스트만으로 실제 GPU 리허설을 대체하지 않는다.
아래 선행 조건이 하나라도 빠지면 장애 주입 전에 중단한다.

## 1. 현재 환경 보관

먼저 [experiment2 복귀 가이드](application-failover-return-to-experiment2.md)의 백업 절차를
실행한다. restore 진행 중에는 이전 이미지 복구나 webhook 삭제를 하지 않는다.

별도 표시가 없는 명령은 MGMT의 Bash에서 실행한다.

```bash
cd /root/hybridspot-validation/System
export ONPRE1_KUBECONFIG=/root/on-premise/member/onpre1.kubeconfig
set -euo pipefail
set +x
umask 077
# 검증한 파일이 없으면 MGMT의 kubernetes context에서 비공개 kubeconfig를 만든다.
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
# API가 MGMT인지 확인한다. context 부재/대상 불일치면 실제 MGMT 파일 경로를 지정한다.
# 임시 인증 파일은 출력/공유하지 않고 데모와 복귀 작업이 모두 끝난 뒤 해당 파일만 삭제한다.
: "${MGMT_KUBECONFIG:?}" "${AWS_KUBECONFIG:?}" "${KARMADA_KUBECONFIG:?}"
export DEMO_NS=fluidcr-realign-121040
export DEMO_WORKLOAD=r50-fluidcr-demo
export DEMO_POLICY=r50-fluidcr-demo
mkdir -p /root/gpu-fluidcr-demo/setup /root/gpu-fluidcr-demo/evidence
umask 077
git status --short
git pull --ff-only origin restore-automation-20260928
export DEMO_TAG="appfailover-$(git rev-parse --short HEAD)"
```

`git pull`에서 로컬 충돌이 발생하면 파일을 보존하고 중단한다. worktree를 reset하지 않는다.
명령, manifest, 로그에 access token을 붙여 넣지 않는다.

## 2. 변경한 이미지 배포 준비

Git push 후에도 컨테이너 이미지는 별도로 빌드·게시해야 한다. registry 인증과 Docker가
준비된 Linux 호스트에서 현재 checkout한 commit으로 빌드한다. 다른 호스트에서 빌드했다면
게시한 태그를 MGMT에도 동일하게 설정한다.

```bash
for COMPONENT in policy-manager placement-webhook; do
  IMAGE="docker.io/jeongseungjun/hybrid-spot-vm-system:${COMPONENT}-${DEMO_TAG}"
  docker build --build-arg COMPONENT="$COMPONENT" -t "$IMAGE" .
  docker push "$IMAGE"
done
```

새 자동 경로에서 변경되는 바이너리는 이 두 개다. checkpoint 생성 정지와 group 완료에는
기존 `checkpoint-coordinator`, `spot-recovery-controller`도 각각 필요하며 기존 v3.2.0
이미지를 재사용할 수 있다. 데모 중 risk collector는 일시 중지 상태로 유지한다.

갱신한 schema와 관리 권한은 **Karmada API**에 적용한다. Deployment는 MGMT에서 변경한다.

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f config/crd/trainingpolicies.yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f config/karmada/access.yaml
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system set image deployment/policy-manager \
  manager="docker.io/jeongseungjun/hybrid-spot-vm-system:policy-manager-$DEMO_TAG"
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system set image deployment/placement-webhook \
  manager="docker.io/jeongseungjun/hybrid-spot-vm-system:placement-webhook-$DEMO_TAG"
```

## 3. 기존 용량 조회 권한

placement 컨트롤러는 AWS에서 대상 Node의 UID, Ready 상태, scheduling 가능 여부,
GPU 할당량과 충돌하는 GPU Pod를 직접 확인한다. Karmada의 cluster 자원 집계만으로는 부족하다.

데모는 mount한 kubeconfig 디렉터리를 명시적으로 사용하며 현재 kubectl context로
대체하지 않는다. 전용 AWS identity에는 Node/PV get/list, Pod list와 데모 namespace의
PVC get/create가 필요하다. PVC는 완료된 PVMigration의 PV에 한정하여 최초 생성하며
기존 claim을 수정하거나 삭제하지 않는다. 가능하면 이 범위로 제한된 identity를 사용한다.
아래 관리자 kubeconfig 사용은 임시 데모용이며 이후 mount와 새로 만든 Secret을 정리한다.

```bash
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system create secret generic \
  application-failover-members \
  --from-file=aws.kubeconfig="$AWS_KUBECONFIG" \
  --dry-run=client -o yaml | kubectl --kubeconfig="$MGMT_KUBECONFIG" apply -f -

kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system patch deployment policy-manager \
  --type=strategic -p '{
    "spec":{"template":{"spec":{
      "containers":[{"name":"manager",
        "env":[{"name":"EXISTING_CAPACITY_KUBECONFIG_DIR","value":"/etc/existing-capacity"}],
        "volumeMounts":[{"name":"application-failover-members",
          "mountPath":"/etc/existing-capacity","readOnly":true}]}],
      "volumes":[{"name":"application-failover-members",
        "secret":{"secretName":"application-failover-members"}}]
    }}}
  }'
```

kubeconfig에는 컨테이너에서 사용할 수 있는 CA/client 인증 정보가 있어야 한다.
MGMT에만 있는 파일 경로를 참조하면 동작하지 않는다. 파일 참조 방식이라면 Secret 생성 전에
`kubectl --kubeconfig="$AWS_KUBECONFIG" config view --raw --flatten` 출력을 권한 제한된
임시 파일로 직접 저장하여 사용한다. 화면이나 로그에 출력하거나 Git에 추가하지 않는다.

## 4. workload 시작 전 admission 검증 복구

MGMT Deployment와 Karmada MutatingWebhookConfiguration은 서로 다른 객체다.
Deployment를 scale해도 이전에 삭제한 webhook 등록이 다시 생성되지는 않는다.
아래 replicas=1은 이번 데모의 실행 설정이다. 종료 시에는 백업한 replicas로 복구한다.

```bash
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system scale \
  deployment/policy-manager deployment/placement-webhook \
  deployment/checkpoint-coordinator deployment/spot-recovery-controller --replicas=1
for DEPLOY in policy-manager placement-webhook checkpoint-coordinator spot-recovery-controller; do
  kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system rollout status \
    deployment/"$DEPLOY" --timeout=180s
done
kubectl --kubeconfig="$KARMADA_KUBECONFIG" get mutatingwebhookconfiguration hybridspot-placement -o yaml
```

등록이 없다면 legacy 데모 전에 보관한 **Karmada** webhook 백업을 복구한다.
URL로 MGMT에 접근할 수 있고 CA가 serving 인증서와 일치하며 failurePolicy가 Fail인지 확인한다.
`config/karmada/placement-webhook.yaml`에는 URL/CA placeholder가 있으므로 그대로 적용하지 않는다.
webhook의 `PLACEMENT_RELEASE_USERNAME`은 policy-manager의 Karmada kubeconfig identity와
일치해야 한다. MGMT 셸 관리자 이름을 대신 넣지 않는다.

admission endpoint에 접근할 수 없거나 등록이 없으면 진행하지 않는다.
`failurePolicy: Ignore`로 우회하지 않는다.

## 5. Member 준비 확인

양쪽 GPU가 이미 존재해야 한다. 다음 조건을 각각 확인한다.

| 위치 | 필수 구성 |
| --- | --- |
| MGMT | stateful-management, pv-migration-management, 위 네 컨트롤러 |
| onpre1 | stateful-checkpoint, restore webhook을 포함한 stateful-member, stateful-artifact, pv-migration-member, training-runtime-collector |
| AWS | 동일한 Member 구성, 사용 중이지 않고 uncordon된 AWS GPU |
| Karmada | FluidCRMigration/RestorePlan/TrainingRuntime status interpreter, 양쪽 Member 등록과 Ready 상태 |
| 양쪽 GPU 노드 | NVIDIA runtime, custom CRI-O/CRIU, 유효한 kubelet TLS, artifact-node label, 공통 NFS 접근 |

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" get clusters
for KC in "$ONPRE1_KUBECONFIG" "$AWS_KUBECONFIG"; do
  kubectl --kubeconfig="$KC" get nodeprovisions -A
  kubectl --kubeconfig="$KC" get nodes -o wide
  kubectl --kubeconfig="$KC" -n stateful-migration-system get deploy,ds,pods,pvc
  kubectl --kubeconfig="$KC" -n pv-migration-system get deploy,pods
  kubectl --kubeconfig="$KC" -n hybridspot-system get deploy/training-runtime-collector
  kubectl --kubeconfig="$KC" get mutatingwebhookconfiguration stateful-restore-mutate
  kubectl --kubeconfig="$KC" get validatingwebhookconfiguration stateful-restore-validate
done
```

이전 출력은 onpre1의 checkpoint/artifact 구성만 확인했다. stateful-member와 PV member
설치까지 확인한 것은 아니다. [Member 설치 가이드](application-failover-member-setup.md)에
따라 누락된 구성을 설치한 뒤 진행한다.

FluidCR injection webhook selector에 `fluidcr-realign-121040`이 포함되는지 확인한다.
기존 `gpu-fluidcr-demo`만 선택하면 부족하다. 다른 환경에 필요한 namespace selector는
보존한다. 실제 source Pod를 조회하여 주입 결과를 확인한다.

공유 artifact-store의 NFS 읽기/쓰기는 이전에 검증했다. workload의 dataset/checkpoint
VCT claim은 artifact PVC와 별개다. source static PV가 이 claim에 바인딩되고 PVMigration이
대응하는 target PV를 준비한 뒤 System이 PVC를 생성한다. migration을 우회하려고 target workload PVC를
미리 만들지 않는다.

## 자동 실행 순서와 증거

실제 workload 배포와 checkpoint/trigger 명령은 [실행 가이드](application-failover-run.md)를 따른다.

trigger는 설정된 대기 GPU를 대상으로 UID가 연결된 RestoreRequest를 준비한다.
placement를 직접 AWS로 patch하지 않으며 대체 cluster는 Karmada가 선택한다.

1. 정기 checkpoint 생성을 정지하고 durable 저장이 완료된 round를 선택한다.
2. source RestorePlan이 현재 Pod의 정확한 UID를 확인해 삭제하고 source 실행 차단을 확립한다.
3. source placement가 남아 있는 동안 PVMigration이 모든 VCT claim을 준비한다.
   정확히는 PVMigration이 대상 PV를 준비하고, System이 동일 VCT spec의 PVC를 생성해
   해당 PV와 UID-bound Bound 상태가 됐는지 확인한다. StatefulSet을 먼저 실행하지 않는다.
4. target RestorePlan이 artifact를 검증하여 Prepared에 도달한다.
5. `purgeMode: Never`, `suppressDeletion: true`로 Karmada application failover의 source 이탈을 허용한다.
6. 이후 Karmada의 AWS 배치를 기록하고 RestoreRequest, source 차단, volume, target 준비 증거가 일치할 때만 배포 보류를 해제한다.
7. 새 AWS Pod가 선택한 checkpoint를 복원하고 학습을 재개했는지 확인한다.

Karmada v1.19는 실패 cluster를 먼저 제거하고 eviction task를 기록한 뒤 별도 갱신으로
대체 배치를 결정한다. 새 연동 경로는 이 중간의 빈 placement도 처리한다.
근거: [v1.19 application-failover controller](https://github.com/karmada-io/karmada/blob/v1.19.0/pkg/controllers/applicationfailover/rb_application_failover_controller.go).

Running/Ready만으로 성공을 판단하지 않는다. 요청의 Verified 상태, source 차단 보고,
PVMigration 완료, 복원 step/checkpoint ID와 이후 최소 세 번의 step 증가를 기록한다.
양쪽 Pod UID와 cluster 이름도 남긴다. 단일 rank 데모가 전체 DDP 수치 동등성을 입증하지는 않는다.

## 리허설 종료 후

[experiment2 복귀 가이드](application-failover-return-to-experiment2.md)를 따른다.
공유 namespace/NFS 데이터/CRD와 관련 없는 컨트롤러를 삭제하지 않는다.
데모 workload 정지와 해당 operation의 정리가 완료될 때까지 이전 source의 차단을 유지한다.
