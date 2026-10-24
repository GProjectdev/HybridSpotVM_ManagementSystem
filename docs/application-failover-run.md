# ResNet-50 Application Failover 실행

[시스템 설치 가이드](application-failover-demo.md)와 Member 준비가 끝난 후 MGMT Bash에서 실행한다.
이 절차는 실제로 실행한 결과가 아니라 새 자동 연계 경로의 리허설 절차다.
공유 namespace는 만들거나 삭제하지 않는다. 아래 작업은 새 데모 이름으로 한 번만 실행한다.
기존 같은 이름의 StatefulSet/Policy/PVC/체크포인트가 있으면 먼저 복귀 가이드로 정리 여부를 확인한다.

## 1. 대상과 초기 상태

```bash
cd /root/hybridspot-validation/System
export ONPRE1_KUBECONFIG=/root/on-premise/member/onpre1.kubeconfig
: "${KARMADA_KUBECONFIG:?}" "${AWS_KUBECONFIG:?}" "${ONPRE1_KUBECONFIG:?}"
export NS=fluidcr-realign-121040
export APP=r50-fluidcr-demo
export MANIFESTS=config/demo/application-failover
export EVIDENCE="/root/gpu-fluidcr-demo/evidence/$(date -u +%Y%m%dT%H%M%SZ)"
umask 077
mkdir -p "$EVIDENCE"

for KC in "$ONPRE1_KUBECONFIG" "$AWS_KUBECONFIG" "$KARMADA_KUBECONFIG"; do
  kubectl --kubeconfig="$KC" get namespace "$NS"
done
kubectl --kubeconfig="$KARMADA_KUBECONFIG" get clusters aws onpre1
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" get node onpre1-gpu-worker-01
kubectl --kubeconfig="$AWS_KUBECONFIG" get node aws-gpu-demo-worker-01
```

양쪽 GPU가 Ready/GPU=1이고 다른 학습 Pod가 없어야 한다. AWS 대기 GPU를 다른 작업에
동시에 배정하지 않는다. 새 용량 검사는 GPU 예약 기능을 대체하지 않는다.
FluidCR injection selector는 양쪽에서 이 namespace를 포함해야 한다.
AWS에 workload용 VCT PVC를 미리 만들지 않는다. source static PV만 준비하고
AWS claim 준비는 PVMigration 경로에서 검증한다.
PVMigration은 대상 PV를 복제하고 System은 VCT에서 대상 PVC를 생성한다.
대상 StatefulSet보다 먼저 claim을 준비하므로 복원 준비와 Pod 배포가 서로 기다리지 않는다.
동일 이름의 기존/다른 operation PVC는 덮어쓰지 않고 중단한다.

```bash
kubectl --kubeconfig="$AWS_KUBECONFIG" auth can-i get persistentvolumes
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" auth can-i get persistentvolumeclaims
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" auth can-i create persistentvolumeclaims
```

위 권한은 컨트롤러에 mount한 AWS kubeconfig 기준으로도 모두 yes여야 한다.

## 2. source 저장소와 Karmada workload 등록

```bash
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" apply -f "$MANIFESTS/01-onpre1-prepare.yaml"
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" -n "$NS" wait \
  --for=condition=complete job/r50-fluidcr-demo-prepare --timeout=300s
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" apply -f "$MANIFESTS/02-onpre1-storage.yaml"

kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f "$MANIFESTS/03-karmada-script.yaml"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f "$MANIFESTS/04-karmada-placement.yaml"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f "$MANIFESTS/05-karmada-workload.yaml"

python3 scripts/application_failover_demo.py enroll \
  --kubeconfig "$KARMADA_KUBECONFIG" \
  --target-kubeconfig "$AWS_KUBECONFIG" \
  --target-node aws-gpu-demo-worker-01 --apply
```

이 명령은 현재 StatefulSet UID와 AWS Node UID를 조회해 TrainingPolicy를 생성한다.
`capacity.mode=Existing`, `replacement.enabled=false`이며 주기적 checkpoint는
데모 동안 disabled다. 새 EC2 생성이나 기존 NodeProvision 소유권 변경은 하지 않는다.
이미 등록된 정책을 덮어쓰지 않는다.

첫 Pod가 생성되기 전에 workload UID label이 전달됐는지 확인한다.

```bash
(
  set -euo pipefail
  UID_EXPECTED=$(kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get sts "$APP" -o jsonpath='{.metadata.uid}')
  ready=false
  for attempt in $(seq 1 60); do
    UID_ACTUAL=$(kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get sts "$APP" \
      -o jsonpath='{.spec.template.metadata.labels.training\.dcnlab\.com/workload-uid}')
    if [ "$UID_ACTUAL" = "$UID_EXPECTED" ]; then ready=true; break; fi
    sleep 2
  done
  test "$ready" = true
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" scale sts "$APP" --replicas=1
)
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" -n "$NS" get pods,pvc -o wide
```

Running 이후 source Pod의 trainer 로그와 주입 결과를 확인한다.

```bash
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" -n "$NS" get pod "$APP-0" -o json |
  jq '{uid:.metadata.uid,node:.spec.nodeName,initContainers:.spec.initContainers,
       containers:[.spec.containers[]|{name,command,args,env}]}'
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" -n "$NS" logs "$APP-0" -c trainer --tail=50
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingruntimes -o yaml
```

ResNet-50 step 증가, world_size=1, source runtime의 현재 Pod UID/observedGeneration을
확인한다. 이미지 이름이 resnet18_eval_v1이어도 ConfigMap 스크립트와 EVAL_MODEL=resnet50이
ResNet-50을 선택한다. 이미지 태그 이름만 보고 모델을 판정하지 않는다.

## 3. 수동 checkpoint

```bash
export CP="r50-demo-cp-$(date -u +%Y%m%d%H%M%S)"
python3 scripts/application_failover_demo.py checkpoint \
  --kubeconfig "$KARMADA_KUBECONFIG" --name "$CP" --apply
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" \
  get fluidcrmigration "$CP" -o yaml
```

완료될 때까지 같은 조회를 반복한다. source cluster 보고의 현재 observedGeneration,
Completed, checkpoint ID, Pod UID, 파일 SHA-256, durableRef, exportedAt가 필요하다.
단순히 파일 하나가 보이거나 Pod가 Running인 것만으로 다음 단계로 넘어가지 않는다.
trigger 도구도 동일한 durable 전체 round 조건을 검사하며 부족하면 거절한다.

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get fluidcrmigration "$CP" -o json \
  > "$EVIDENCE/checkpoint.json"
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" -n "$NS" logs "$APP-0" -c trainer --timestamps \
  > "$EVIDENCE/source-before-failover.log"
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" -n "$NS" exec "$APP-0" -c trainer -- \
  sh -c 'find /checkpoint -maxdepth 3 -type f -ls'
```

## 4. application failover 활성화와 장애 시연

양쪽 Cluster 자체는 Ready로 둔다. source GPU를 cordon하고 정확한 UID에 연결된
source 종료를 시스템에 요청한다. **kubectl delete pod를 추가 실행하지 않는다.**
이는 API/노드 전체 단절을 주입하는 실험이 아니라, source 노드가 통신 가능한 상태에서
애플리케이션 실행을 중단하고 Karmada의 애플리케이션 장애 재배치를 검증하는 시연이다.

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" patch propagationpolicy \
  r50-fluidcr-demo-onpre1 --type=merge \
  --patch-file "$MANIFESTS/application-failover.patch.yaml"

kubectl --kubeconfig="$ONPRE1_KUBECONFIG" cordon onpre1-gpu-worker-01
python3 scripts/application_failover_demo.py trigger \
  --kubeconfig "$KARMADA_KUBECONFIG" \
  --source-kubeconfig "$ONPRE1_KUBECONFIG" \
  --confirm-workload "$NS/$APP"
```

trigger가 거절되면 이유를 확인하고 멈춘다. annotation을 수동으로 위조하거나 배치를
AWS로 강제 patch하지 않는다. source 종료는 RestorePlan이 담당하며, 종료 확인 후
PVMigration과 target 준비가 먼저 완료돼야 배치를 풀어 준다.
Karmada의 300초 toleration과 controller reconcile 시간 때문에 즉시 이동하지 않는다.
Never eviction은 원본 Work를 보존한다. 시연 종료 정리는 복귀 가이드의 별도 절차다.

## 5. 관찰과 합격 기준

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" \
  get resourcebindings,restorerequests,restoreplans,pvmigrations -o yaml \
  > "$EVIDENCE/recovery-progress.yaml"
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" -n "$NS" get pods -o wide
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get pods,pvc -o wide
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" logs "$APP-0" -c trainer --tail=100
```

진행 중에는 위 조회를 반복한다. 실패 상태가 반복되면 CR의 message와 해당 controller
로그를 보관한다. 강제 삭제, finalizer 제거, webhook 우회로 성공처럼 만들지 않는다.

합격 증거는 다음을 모두 포함한다.
- source Pod UID에 대한 SourceGone/source fence와 source 학습 중단.
- PVMigration 완료 및 source VCT 두 개에 대응하는 AWS PVC 준비.
- Karmada ResourceBinding의 ApplicationFailure eviction task와 실제 AWS 선택.
- 해당 UID-bound RestoreRequest의 Verified, AWS의 새로운 Pod UID.
- checkpoint 복원 이벤트와 restored step, 이후 최소 세 번의 step 증가.

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" \
  get trainingpolicies,trainingruntimes,fluidcrmigrations,restorerequests,restoreplans,pvmigrations,resourcebindings \
  -o json > "$EVIDENCE/final-resources.json"
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" logs "$APP-0" -c trainer --timestamps \
  > "$EVIDENCE/target-after-restore.log"
```

저장 checkpoint 이후의 일부 작업은 다시 수행할 수 있다. 장애 직전 무손실 이어하기나
full-DDP 수치 동등성으로 표현하지 않는다.
종료 후 [experiment2 복귀 가이드](application-failover-return-to-experiment2.md)를 따른다.
