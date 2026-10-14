# v3.1: 연속 Partial Restore 수정 및 병렬 VM 준비

## 1. 적용 범위

이번 배포는 실패한 costb08을 이어서 실행하는 배포가 아니다. 기존 evidence와 PVC를 보존하고 새 run-id로 실행한다.

- System: 대체 VM들을 먼저 병렬로 준비하되 Partial Checkpoint/Restore는 작업 단위로 순차 실행한다.
  적용 조건은 spec.replacement.enabled=true인 planned-partial 경로이다. planned-partial=disabled로 선택하는 기존 group 복구 경로는 변경하지 않는다.
- Stateful runtime payload: Survivor의 DDP 재구성 이후 동일 모델이 중복 저장되는 문제를 수정한다.
- Stateful management/member: 완료된 Partial Restore의 증거가 다음 교체의 Pod 상태 때문에 뒤집히지 않도록 보완한다.
- 평가 runner: 정상 stop과 실패 실행의 명시적 abort를 구분한다. finalizer 제거, 성공 상태 위조, 전체 컨트롤러 중지는 하지 않는다.
- Provisioner v3.0.1의 SSH/CRIU probe 수정은 유지한다. 이번 기능 때문에 Provisioner나 CRI-O/CRIU 패키지를 다시 빌드할 필요는 없다.
- 학습 이미지 resnet18_eval_v1은 유지한다. 변경되는 Python runtime은 initContainer로 주입되는 payload이다.

병렬 준비는 VM 생성 API 요청들을 같은 조정 단계에서 발행한다는 뜻이다. AWS 가용 용량, Provisioner 처리량에 따라 실제 완료 시각은 다를 수 있다. 목표 Worker가 2개이고 둘 다 교체하면 일시적으로 기존 2개 + 대체 2개가 과금될 수 있다. 학습 중단 시간 감소나 비용 절감을 보장하는 기능은 아니다.

## 2. 사전 조건 및 소스 갱신 (MGMT, Bash)

기존 실행이 정리되었고 해당 EC2가 실제 terminated인지 확인한다. 다른 실행이 복구 중이면 배포하지 않는다. namespace는 fluidcr-realign-121040을 그대로 사용한다.

```bash
export ROOT=/root/hybridspot-validation
export NS=fluidcr-realign-121040
: "${MGMT_KUBECONFIG:?MGMT kubeconfig 필요}"
: "${KARMADA_KUBECONFIG:?Karmada kubeconfig 필요}"
: "${AWS_KUBECONFIG:?AWS member kubeconfig 필요}"
git -C "$ROOT/System" status --short
git -C "$ROOT/stateful" status --short
git -C "$ROOT/fluidcr" status --short
git -C "$ROOT/System" pull --ff-only origin restore-automation-20260928
git -C "$ROOT/stateful" pull --ff-only origin restore-automation-20260928
git -C "$ROOT/fluidcr" pull --ff-only origin restore-automation-20260928
```

stateful 디렉터리가 없다면 아래 저장소를 clone한다. 이미 있는 디렉터리를 덮어쓰지 않는다.

```bash
git clone --branch restore-automation-20260928 https://github.com/GProjectdev/Stateful-Migration-Operator-with-PV.git "$ROOT/stateful"
```

fluidcr 디렉터리가 없을 때만 아래도 실행한다.

```bash
git clone --branch restore-automation-20260928 https://github.com/GProjectdev/My_FluidCR.git "$ROOT/fluidcr"
```

System 디렉터리가 git clone이 아니라 복사본이면 실제 clone 경로에서 갱신 후 변경 파일을 동기화한다. evaluation/*.local.json, policy-source.json, evidence는 덮어쓰지 않는다. 가능하면 별도 clone에서 빌드하고 기존 실행 디렉터리에는 evaluation 코드만 갱신한다.

```bash
cd "$ROOT/System"
python3 -m unittest discover -s evaluation -p 'test_*.py'
go test -p 2 -mod=readonly ./...
go vet -p 2 ./...
cd "$ROOT/stateful"
python3 -m unittest discover -s runtime/tests
go test -p 2 -mod=readonly ./...
go vet -p 2 ./...
```

## 3. Buildah 이미지 생성 및 Push

아래는 동일 Bash 세션에서 실행한다. 각 명령 실패 시 다음 단계로 진행하지 않는다. 태그는 v3.1이며 v.3.1이 아니다. 이미 v3.1을 배포한 후 코드를 다시 변경했다면 새 태그를 사용한다.

```bash
set -euo pipefail
export ROOT=/root/hybridspot-validation
export RELEASE_DIR="$ROOT/evidence/release-v3.1-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$RELEASE_DIR"
export POLICY_IMAGE=docker.io/jeongseungjun/hybrid-spot-vm-system:policy_manager_v3.1
export COORD_IMAGE=docker.io/jeongseungjun/hybrid-spot-vm-system:checkpoint_coordinator_v3.1
export RECOVERY_IMAGE=docker.io/jeongseungjun/hybrid-spot-vm-system:spot_recovery_controller_v3.1
export STATEFUL_IMAGE=docker.io/jeongseungjun/stateful-migration-system:v3.1
export PAYLOAD_IMAGE=docker.io/jeongseungjun/myfluidcr-operator:payload-stateful-v3.1
export PAYLOAD_BASE=docker.io/jeongseungjun/myfluidcr-operator:payload-base-v3.1
buildah login docker.io
cd "$ROOT/System"
buildah bud --arch amd64 --build-arg COMPONENT=policy-manager -f Dockerfile -t "$POLICY_IMAGE" .
buildah push "$POLICY_IMAGE" "docker://$POLICY_IMAGE"
buildah bud --arch amd64 --build-arg COMPONENT=checkpoint-coordinator -f Dockerfile -t "$COORD_IMAGE" .
buildah push "$COORD_IMAGE" "docker://$COORD_IMAGE"
buildah bud --arch amd64 --build-arg COMPONENT=spot-recovery-controller -f Dockerfile -t "$RECOVERY_IMAGE" .
buildah push "$RECOVERY_IMAGE" "docker://$RECOVERY_IMAGE"
cd "$ROOT/stateful"
buildah bud --arch amd64 -f Dockerfile -t "$STATEFUL_IMAGE" .
buildah push "$STATEFUL_IMAGE" "docker://$STATEFUL_IMAGE"
```

FluidCR 원본과 Stateful overlay의 runtime 코드를 함께 갱신한다. base payload를 먼저 빌드한 뒤 그 이미지를 overlay의 입력으로 사용한다.

```bash
cd "$ROOT/fluidcr"
buildah bud --arch amd64 -f Dockerfile.payload -t "${PAYLOAD_BASE:?}" .
buildah push "$PAYLOAD_BASE" "docker://$PAYLOAD_BASE"
cd "$ROOT/stateful"
buildah bud --arch amd64 -f Dockerfile.payload-overlay \
  --build-arg "FLUIDCR_PAYLOAD_IMAGE=${PAYLOAD_BASE:?}" -t "$PAYLOAD_IMAGE" .
buildah push "$PAYLOAD_IMAGE" "docker://$PAYLOAD_IMAGE"
git -C "$ROOT/System" rev-parse HEAD > "$RELEASE_DIR/system.commit"
git -C "$ROOT/stateful" rev-parse HEAD > "$RELEASE_DIR/stateful.commit"
git -C "$ROOT/fluidcr" rev-parse HEAD > "$RELEASE_DIR/fluidcr.commit"
```

digest 파일은 필수가 아니다. 위 명령은 완전한 태그를 그대로 사용하며 빈 digest에 @를 붙이지 않는다. 실제 Pod의 imageID도 배포 후 기록한다.

## 4. 컨트롤러 교체

기존 Deployment의 kubeconfig, 인자, Secret, ServiceAccount 설정은 유지하고 이미지만 교체한다. Stateful binary는 management/member/checkpoint/artifact가 같은 이미지를 mode 인자로 나눠 사용한다.

```bash
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system get deploy -o yaml > "$RELEASE_DIR/system-before.yaml"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n stateful-migration-system get deploy -o yaml > "$RELEASE_DIR/management-before.yaml"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n stateful-migration-system get deploy,ds -o yaml > "$RELEASE_DIR/member-before.yaml"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system set image deployment/policy-manager "manager=${POLICY_IMAGE:?}"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system set image deployment/checkpoint-coordinator "manager=${COORD_IMAGE:?}"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system set image deployment/spot-recovery-controller "manager=${RECOVERY_IMAGE:?}"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n stateful-migration-system set image deployment/stateful-management "manager=${STATEFUL_IMAGE:?}"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n stateful-migration-system set image deployment/stateful-member "manager=${STATEFUL_IMAGE:?}"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n stateful-migration-system set image deployment/stateful-checkpoint "manager=${STATEFUL_IMAGE:?}"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n stateful-migration-system set image daemonset/stateful-artifact "verifier=${STATEFUL_IMAGE:?}"
for component in policy-manager checkpoint-coordinator spot-recovery-controller; do
  kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system rollout status deployment/"$component" --timeout=300s
done
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n stateful-migration-system rollout status deployment/stateful-management --timeout=300s
kubectl --kubeconfig="$AWS_KUBECONFIG" -n stateful-migration-system rollout status deployment/stateful-member --timeout=300s
kubectl --kubeconfig="$AWS_KUBECONFIG" -n stateful-migration-system rollout status deployment/stateful-checkpoint --timeout=300s
kubectl --kubeconfig="$AWS_KUBECONFIG" -n stateful-migration-system rollout status daemonset/stateful-artifact --timeout=300s
```

## 5. FluidCR 주입 payload 교체

Webhook image 자체가 아니라 --payload-image 인자를 바꾼다. 아래 조회에서 컨테이너와 인자를 확인한 후 edit에서 해당 인자 값만 PAYLOAD_IMAGE 값으로 바꾼다. 인자가 --payload-image=...이면 = 뒤를, 두 항목이면 바로 다음 항목을 바꾼다.

```bash
kubectl --kubeconfig="$AWS_KUBECONFIG" -n fluidcr-system get deployment fluidcr-webhook -o yaml > "$RELEASE_DIR/webhook-before.yaml"
printf '%s\n' "${PAYLOAD_IMAGE:?}"
kubectl --kubeconfig="$AWS_KUBECONFIG" -n fluidcr-system get deployment fluidcr-webhook -o json | jq '.spec.template.spec.containers[] | {name,args}'
kubectl --kubeconfig="$AWS_KUBECONFIG" -n fluidcr-system edit deployment fluidcr-webhook
kubectl --kubeconfig="$AWS_KUBECONFIG" -n fluidcr-system rollout status deployment/fluidcr-webhook --timeout=300s
kubectl --kubeconfig="$AWS_KUBECONFIG" -n fluidcr-system get deployment fluidcr-webhook -o json | jq '.spec.template.spec.containers[] | {name,args}'
```

기존 Pod는 이전 payload를 계속 사용한다. 실패한 실험은 재개하지 않고 새 run-id로 준비한다. 새 두 Rank의 initContainer image와 imageID가 모두 새 payload인지 확인한다.

## 6. 새 B 실행

config.cost.local.json의 목표 Step은 A와 같은 43000을 유지한다. 위험률 변경은 학습 진행 관측 후 3600초에 0.8, minOnDemand 강제 없음, B의 정기 Checkpoint는 활성 상태이다. 병렬 준비로 전환했으므로 예전 B 결과와 같은 버전 결과로 합치지 않는다.

```bash
cd "$ROOT/System"
export RUN=costb09
test ! -e "evidence/cost/$RUN"
python3 evaluation/cost.py render --config evaluation/config.cost.local.json --run-id "$RUN" --arm B --out "evidence/cost/$RUN"
jq '{run_id,phase,goal_steps}' "evidence/cost/$RUN/run.json"
jq '{annotations:.metadata.annotations,replacement:.spec.replacement}' "evidence/cost/$RUN/policy.yaml"
python3 evaluation/cost.py prepare --evidence "evidence/cost/$RUN"
python3 evaluation/cost.py start --evidence "evidence/cost/$RUN"
```

다른 터미널에서 RUN/ROOT 변수를 다시 설정하고 관측한다.

```bash
export ROOT=/root/hybridspot-validation RUN=costb09
tail -n 20 -F "$ROOT/System/evidence/cost/$RUN/runner-events.jsonl"
```

```bash
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n fluidcr-realign-121040 get nodeprovisions -o custom-columns='NAME:.metadata.name,MARKET:.spec.marketType,PHASE:.status.phase,MESSAGE:.status.message'
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n fluidcr-realign-121040 get spotreplacements,restorerequests
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n fluidcr-realign-121040 get pods -l "training.dcnlab.com/evaluation-run=$RUN" -o json | jq '.items[] | {name:.metadata.name,init:.spec.initContainers,initStatus:.status.initContainerStatuses}'
```

검증 기준:
- 두 슬롯 모두 시장 유형 변경이 필요하면 대체 NodeProvision 두 개가 첫 번째 Restore 완료를 기다리지 않고 생성된다.
- 실제 VM Ready까지의 시간은 각각 다를 수 있다. 원본을 미리 지우지 않는다.
- Partial Checkpoint/Restore는 하나씩 진행되고, 첫 작업의 완료 증거가 다음 작업 때문에 Failed로 바뀌지 않는다.
- 두 번째 복구 이후 두 Rank의 global_step이 증가한다. 목표 Step 완료와 runner 성공 판정까지 확인한다.
- 비용은 대체 VM 준비/중복 과금 및 실제 종료까지 포함한다.
- 완료 후 cleanup은 별도 실행한다. PVC/PV는 보존된다.

```bash
python3 evaluation/cost.py cleanup --evidence "evidence/cost/$RUN" --confirm "$RUN"
```

## 7. 실패 실행의 stop / abort / cleanup

정상 실행은 기존 stop과 cleanup을 사용한다. stop이 `operations are not quiescent`로 거부되면 진행 중인 복구를 단순히 성공 처리하지 않는다. 실패한 실험을 포기할 때만 abort를 사용한다.

1. 원래 start가 실행 중이면 그 터미널에서 Ctrl+C로 종료하고 반환을 기다린다. stop 대기 오류가 나더라도 run.json의 phase가 failed인지 확인한다.
2. abort는 해당 run-id를 명시적으로 확인하고, UID로 연결된 복구 객체를 백업한 뒤 삭제한다. 다른 실험, NodeProvision, PVC/PV를 취소 대상으로 삼지 않는다.
3. 컨트롤 플레인과 멤버의 관련 작업이 사라진 것을 확인한 뒤 workload를 0으로 줄인다. 제거 대기가 시간 초과되면 finalizer를 강제로 지우지 말고 남아 있는 객체를 확인한 후 같은 abort를 재시도한다.
4. abort 성공 후 cleanup으로 VM 삭제를 요청한다. EC2 종료는 별도 확인한다.

```bash
cd /root/hybridspot-validation/System
export RUN=costb09
jq '{run_id,phase,failure_reason}' "evidence/cost/$RUN/run.json"
python3 evaluation/cost.py abort --evidence "evidence/cost/$RUN" --confirm "$RUN"
python3 evaluation/cost.py cleanup --evidence "evidence/cost/$RUN" --confirm "$RUN"
```

abort가 실패하면 다음 cleanup으로 넘어가지 않는다. 이미 정리한 costb08에는 실행할 필요 없다. 백업과 runner-events는 실패 원인 분석용으로 보존한다.

## 8. 검증 범위

로컬 회귀 테스트: System 평가 runner 74개, Stateful runtime 53개, 원본 FluidCR 관련 테스트 64개 통과. Go 전체 테스트와 vet도 두 컨트롤러 저장소에서 확인한다. 실제 PyTorch/Gloo 통합 스크립트는 별도로 제공하지만 로컬에는 torch가 없어 실행하지 않았다.

로컬 테스트는 실제 EC2 생성, CUDA/NCCL, CRIU GPU 복구를 대신하지 않는다. v3.1에서 두 번의 연속 교체가 성공하고 모든 Rank의 학습이 재개되는지 클러스터에서 확인해야 한다. costb08은 실패 기록으로 보존하며 정상 성능 결과에 포함하지 않는다.

A도 가능하면 동일 v3.1 환경에서 다시 측정한다. 기존 costa06 결과를 참고값으로 남기는 경우에는 runtime/컨트롤러 버전 차이를 명시한다. 전체 테스트 결과와 실제 클러스터 성공 여부는 구분해서 기록한다.

## 9. 배포 실패 시

rollout 실패 시 실험을 시작하지 않는다. 기존에 백업한 Deployment의 image/args와 비교하고 해당 Deployment만 rollout undo하거나 이전 이미지로 되돌린다. 진행 중인 Restore에서 여러 버전의 payload를 섞지 않는다. payload 롤백은 Webhook 인자 복구와 새 Pod 생성이 모두 필요하다. CRD/status/finalizer를 임의로 바꾸어 검증을 우회하지 않는다.

