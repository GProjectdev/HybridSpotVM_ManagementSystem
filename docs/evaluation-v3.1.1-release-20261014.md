# v3.1.1 복원 검증 및 abort 수정 배포

기존 실패 실행은 재개하지 않는다. evidence와 PVC/PV는 보존하고 새 run-id를 사용한다.
명령은 MGMT의 Bash에서 실행한다. 실제 GPU/CRIU 복원은 아래 짧은 검증으로 확인한다.

## 1. 변경 범위

- Stateful 관리: 완료된 복원 증거가 이후 교체/학습 종료로 뒤집히지 않도록 보완.
- Stateful Checkpoint: 이미 재개된 과거 Checkpoint 삭제 시 현재 partial 복원에 잘못 resume하지 않도록 보완.
- FluidCR payload: 복원 후 현재 Pod 식별 정보 사용. UID/round 검증은 유지.
- System 평가 runner: abort 삭제 순서와 남은 전파 정책의 안전한 재시도.
- Karmada RBAC: stateful-management의 restoreplans patch 권한.

병렬 대체 VM 준비와 순차 Partial Checkpoint/Restore는 유지한다.
Provisioner, CRI-O/CRIU 패키지, resnet18_eval_v1 재빌드는 필요하지 않다.
System 컨트롤러는 변경하지 않으므로 기존 v3.1을 유지하고 평가 Python 파일을 갱신한다.
Stateful 컨트롤러, FluidCR 주입 webhook과 payload는 기존 태그를 덮어쓰지 않고 v3.1.1로 배포한다.

## 2. 코드 갱신 및 테스트

다른 정상 실행이 진행 중이면 배포하지 않는다. 기존 start 프로세스를 종료한다.
기존 abort가 막혀 있다면 코드/컨트롤러 갱신 뒤 7절을 재시도하고 정리 완료 후 새 실험을 시작한다.
로컬 설정과 evidence는 보존한다. git clean/reset을 사용하지 않는다.

```bash
export ROOT=/root/hybridspot-validation
export NS=fluidcr-realign-121040
: "${MGMT_KUBECONFIG:?}"
: "${KARMADA_KUBECONFIG:?}"
: "${AWS_KUBECONFIG:?}"
git -C "$ROOT/System" status --short
git -C "$ROOT/stateful" status --short
git -C "$ROOT/fluidcr" status --short
git -C "$ROOT/System" pull --ff-only origin restore-automation-20260928
git -C "$ROOT/stateful" pull --ff-only origin restore-automation-20260928
git -C "$ROOT/fluidcr" pull --ff-only origin restore-automation-20260928
cd "$ROOT/System"
python3 -m unittest discover -s evaluation -p 'test_*.py'
cd "$ROOT/stateful"
go test -p 2 -mod=readonly ./...
go vet -p 2 ./...
python3 -m unittest discover -s runtime/tests
```

System이 복사본이면 실제 clone에서 갱신 후 evaluation 코드 파일을 동기화한다.
evaluation/*.local.json, policy-source.json, evidence는 덮어쓰지 않는다.

## 3. Buildah 빌드와 push

같은 Bash 세션에서 실행한다. 각 명령 실패 시 다음 단계로 진행하지 않는다.
빈 digest에 @를 붙이지 않고 새 태그를 그대로 사용한다.

```bash
set -euo pipefail
export RELEASE_DIR="$ROOT/evidence/release-v3.1.1-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$RELEASE_DIR"
export STATEFUL_IMAGE=docker.io/jeongseungjun/stateful-migration-system:v3.1.1
export PAYLOAD_BASE=docker.io/jeongseungjun/myfluidcr-operator:payload-base-v3.1.1
export PAYLOAD_IMAGE=docker.io/jeongseungjun/myfluidcr-operator:payload-stateful-v3.1.1
export WEBHOOK_IMAGE=docker.io/jeongseungjun/myfluidcr-operator:webhook-v3.1.1
buildah login docker.io
cd "$ROOT/stateful"
buildah bud --arch amd64 -f Dockerfile -t "$STATEFUL_IMAGE" .
buildah push "$STATEFUL_IMAGE" "docker://$STATEFUL_IMAGE"
cd "$ROOT/fluidcr"
buildah bud --arch amd64 -f Dockerfile.webhook -t "$WEBHOOK_IMAGE" .
buildah push "$WEBHOOK_IMAGE" "docker://$WEBHOOK_IMAGE"
buildah bud --arch amd64 -f Dockerfile.payload -t "$PAYLOAD_BASE" .
buildah push "$PAYLOAD_BASE" "docker://$PAYLOAD_BASE"
cd "$ROOT/stateful"
buildah bud --arch amd64 -f Dockerfile.payload-overlay \
  --build-arg "FLUIDCR_PAYLOAD_IMAGE=$PAYLOAD_BASE" -t "$PAYLOAD_IMAGE" .
buildah push "$PAYLOAD_IMAGE" "docker://$PAYLOAD_IMAGE"
git -C "$ROOT/System" rev-parse HEAD > "$RELEASE_DIR/system.commit"
git -C "$ROOT/stateful" rev-parse HEAD > "$RELEASE_DIR/stateful.commit"
git -C "$ROOT/fluidcr" rev-parse HEAD > "$RELEASE_DIR/fluidcr.commit"
```

## 4. RBAC와 컨트롤러 교체

RBAC 대상은 Karmada API다. AWS 클러스터에만 적용해서는 관리 컨트롤러 권한이 바뀌지 않는다.

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" get clusterrole stateful-management -o yaml > "$RELEASE_DIR/rbac-before.yaml"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f "$ROOT/stateful/config/karmada/role.yaml"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f "$ROOT/stateful/config/karmada/binding.yaml"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" auth can-i patch restoreplans.migration.dcnlab.com \
  --as=system:serviceaccount:stateful-migration-system:stateful-management -n "$NS"
```

마지막 출력은 yes여야 한다. impersonation 권한 오류는 검사자 권한 문제이므로 binding과 컨트롤러 로그도 확인한다.

```bash
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n stateful-migration-system get deploy -o yaml > "$RELEASE_DIR/management-before.yaml"
kubectl --kubeconfig="$AWS_KUBECONFIG" -n stateful-migration-system get deploy,ds -o yaml > "$RELEASE_DIR/member-before.yaml"
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n stateful-migration-system set image deployment/stateful-management "manager=${STATEFUL_IMAGE:?}"
kubectl --kubeconfig="$AWS_KUBECONFIG" -n stateful-migration-system set image deployment/stateful-member "manager=${STATEFUL_IMAGE:?}"
kubectl --kubeconfig="$AWS_KUBECONFIG" -n stateful-migration-system set image deployment/stateful-checkpoint "manager=${STATEFUL_IMAGE:?}"
kubectl --kubeconfig="$AWS_KUBECONFIG" -n stateful-migration-system set image daemonset/stateful-artifact "verifier=${STATEFUL_IMAGE:?}"
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n stateful-migration-system rollout status deployment/stateful-management --timeout=300s
kubectl --kubeconfig="$AWS_KUBECONFIG" -n stateful-migration-system rollout status deployment/stateful-member --timeout=300s
kubectl --kubeconfig="$AWS_KUBECONFIG" -n stateful-migration-system rollout status deployment/stateful-checkpoint --timeout=300s
kubectl --kubeconfig="$AWS_KUBECONFIG" -n stateful-migration-system rollout status daemonset/stateful-artifact --timeout=300s
```

## 5. 주입 payload 교체

이번에는 fluidcr-webhook 이미지와 --payload-image 인자를 모두 변경한다.
새 injector가 initContainer에서 현재 Pod 식별 정보를 전달하므로 payload만 바꾸면 충분하지 않다.
기존 args의 나머지는 유지한다. --payload-image=...이면 = 오른쪽, 분리 형태면 바로 다음 원소를 변경한다.

```bash
kubectl --kubeconfig="$AWS_KUBECONFIG" -n fluidcr-system get deploy fluidcr-webhook -o yaml > "$RELEASE_DIR/webhook-before.yaml"
printf '%s\n' "${PAYLOAD_IMAGE:?}"
kubectl --kubeconfig="$AWS_KUBECONFIG" -n fluidcr-system get deploy fluidcr-webhook -o json | jq '.spec.template.spec.containers[] | {name,args}'
kubectl --kubeconfig="$AWS_KUBECONFIG" -n fluidcr-system edit deploy fluidcr-webhook
kubectl --kubeconfig="$AWS_KUBECONFIG" -n fluidcr-system set image deploy/fluidcr-webhook "webhook=${WEBHOOK_IMAGE:?}"
kubectl --kubeconfig="$AWS_KUBECONFIG" -n fluidcr-system rollout status deploy/fluidcr-webhook --timeout=300s
kubectl --kubeconfig="$AWS_KUBECONFIG" -n fluidcr-system get deploy fluidcr-webhook -o json | jq '.spec.template.spec.containers[] | {name,args}'
```

기존 Pod에는 새 payload가 반영되지 않는다. 새 실행의 두 Pod initContainer image/imageID를 확인한다.
rollout 실패 시 새 실험을 시작하지 않고 백업 image/args를 확인해 해당 배포를 복구한다.

## 6. 먼저 짧은 복원 검증

이는 정식 비용 평가가 아닌 두 번의 연속 교체 검증이다.
3분은 VM 생성 시작이 아니라 all_ranks_progress_observed 이후 기준이다.
18000 step은 과거 약 30분 학습량이다. 교체/복원까지 포함한 30분 완료를 보장하지 않는다.

```bash
cd "$ROOT/System"
jq '.goal_steps=18000 | .max_seconds=5400 | .scenario="risk-schedule" |
    .initial_risk=0.05 | .changed_risk=0.8 | .risk_change_after_seconds=180 |
    .risk_schedule=[{"after_seconds":180,"lambda_per_hour":0.8}]' \
  evaluation/config.cost.local.json > evaluation/config.cost.restore-check.local.json
export RUN=restorecheck03
test ! -e "evidence/cost/$RUN"
python3 evaluation/cost.py render --config evaluation/config.cost.restore-check.local.json \
  --run-id "$RUN" --arm B --out "evidence/cost/$RUN"
jq '{run_id,phase}' "evidence/cost/$RUN/run.json"
python3 evaluation/cost.py prepare --evidence "evidence/cost/$RUN"
python3 evaluation/cost.py start --evidence "evidence/cost/$RUN"
```

별도 터미널에서도 ROOT/RUN/NS와 kubeconfig 변수를 설정한다.

```bash
export ROOT=/root/hybridspot-validation RUN=restorecheck03 NS=fluidcr-realign-121040
tail -n 20 -F "$ROOT/System/evidence/cost/$RUN/runner-events.jsonl"
```

```bash
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get spotreplacements,restorerequests,spotrecoveries -o json |
  jq --arg prefix "eval-$RUN-" '.items[] | select(.metadata.name | startswith($prefix)) | {kind,name:.metadata.name,phase:.status.phase,message:.status.message}'
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get pods -l "training.dcnlab.com/evaluation-run=$RUN" -o wide
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get nodeprovisions -o custom-columns='NAME:.metadata.name,MARKET:.spec.marketType,PHASE:.status.phase,MESSAGE:.status.message'
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get pods -l "training.dcnlab.com/evaluation-run=$RUN" -o json |
  jq '.items[] | {name:.metadata.name,uid:.metadata.uid,node:.spec.nodeName,init:.spec.initContainers,images:.status.initContainerStatuses}'
```

성공 조건:
아래 파일은 최초 생성뿐 아니라 두 번째 교체에서도 현재 Pod UID/nodeName과 일치해야 한다.

```bash
for rank in 0 1; do
  POD="eval-$RUN-$rank"
  kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get pod "$POD" -o json | jq '{name:.metadata.name,uid:.metadata.uid,node:.spec.nodeName}'
  kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" exec "$POD" -c trainer -- sh -c 'for f in name uid node; do printf "%s=" "$f"; cat "/opt/fluidcr/pod-identity/$f"; printf "\n"; done'
done
```

최종 성공 조건:
- 두 RestoreRequest가 Verified이고 이후 교체/학습 종료에도 유지.
- 두 SpotReplacement 및 해당 SpotRecovery가 Completed, 원본 Spot VM 종료.
- 두 Rank가 두 번째 복원 이후에도 step 증가 후 목표 step 도달.
- all_ranks_completed와 workload_stopped 기록, run_failed 없음.
- 복원 후 survivorResume 증거의 UID/nodeName이 해당 현재 Pod와 일치.

정상 종료 후에도 VM 과금은 남을 수 있으므로 cleanup하고 EC2 terminated를 확인한다.

```bash
cd "$ROOT/System"
jq '{run_id,phase,failure_reason}' "evidence/cost/$RUN/run.json"
python3 evaluation/cost.py cleanup --evidence "evidence/cost/$RUN" --confirm "$RUN"
```

## 7. 실패 실행 abort / cleanup

start가 실행 중이면 Ctrl+C 후 종료를 기다린다. RUN에는 폐기할 실패 실행 이름을 정확히 지정한다.
abort 실패 시 cleanup으로 넘어가지 않는다. finalizer 제거나 성공 상태 위조는 하지 않는다.

```bash
cd "$ROOT/System"
export RUN=restorecheck02
jq '{run_id,phase,failure_reason}' "evidence/cost/$RUN/run.json"
python3 evaluation/cost.py abort --evidence "evidence/cost/$RUN" --confirm "$RUN"
python3 evaluation/cost.py cleanup --evidence "evidence/cost/$RUN" --confirm "$RUN"
```

PVC/PV와 evidence는 보존된다. namespace 전체 삭제로 다른 실행의 리소스를 지우지 않는다.

## 8. 검증 성공 후 정식 A/B

같은 버전/학습 설정으로 A 후 B를 순차 실행한다. 각 실행 cleanup 및 EC2 종료 후 다음 실행을 시작한다.
A 정기 Checkpoint 비활성, B 계산 주기 설정은 renderer가 적용한다.

```bash
cd "$ROOT/System"
jq '.goal_steps=43000 | .max_seconds=10800 | .scenario="risk-schedule" |
    .initial_risk=0.05 | .changed_risk=0.8 | .risk_change_after_seconds=3600 |
    .risk_schedule=[{"after_seconds":3600,"lambda_per_hour":0.8}]' \
  evaluation/config.cost.local.json > evaluation/config.cost.full.local.json
export RUN=costa07
test ! -e "evidence/cost/$RUN"
python3 evaluation/cost.py render --config evaluation/config.cost.full.local.json --run-id "$RUN" --arm A --out "evidence/cost/$RUN"
python3 evaluation/cost.py prepare --evidence "evidence/cost/$RUN"
python3 evaluation/cost.py start --evidence "evidence/cost/$RUN"
```

A 성공 확인 및 cleanup 후:

```bash
export RUN=costb10
test ! -e "evidence/cost/$RUN"
python3 evaluation/cost.py render --config evaluation/config.cost.full.local.json --run-id "$RUN" --arm B --out "evidence/cost/$RUN"
python3 evaluation/cost.py prepare --evidence "evidence/cost/$RUN"
python3 evaluation/cost.py start --evidence "evidence/cost/$RUN"
```

이미 사용한 run-id라면 새 이름을 쓴다. 43000 step은 동일 유효 학습량 비교이며 B의 120분 이내 종료를 보장하지 않는다.
위험률 0.8은 정책 입력 변경이지 실제 선점 발생 자체가 아니다. 강제 선점 실험과 구분해 기록한다.
