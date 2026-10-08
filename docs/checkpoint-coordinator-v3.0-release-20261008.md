# Checkpoint Coordinator v3.0 빌드·배포 가이드 (2026-10-08)

MGMT의 Bash에서 순서대로 실행하는 절차다. 빌드 대상은 검증한 코드 commit `933878e10abb3b0a16d4ec4e40e17bba782f2a53`으로 고정한다. Go 전체 테스트·정적 검사, Linux amd64 빌드, Python 배포 스크립트 테스트 9개를 통과했다. Docker 이미지 push와 실클러스터 배포는 아직 수행하지 않았으므로 아래 절차로 진행한다. 명령은 코드 commit의 브랜치 포함 여부를 검증한 뒤 detached checkout한다. 별도 Git tag는 사용하지 않는다.

기존 `fluidcr-realign-121040` namespace와 workload/PVC를 유지한다. namespace 생성, 학습 Pod·StatefulSet·PVC 재생성, worker scaling은 수행하지 않는다. 변경 대상은 Karmada의 TrainingPolicy CRD와 MGMT `hybridspot-system`의 두 Deployment 이미지다. `config/management` 전체 apply나 이전 가이드의 재생성·강제 120초 설정 절차를 함께 실행하지 않는다.

## 1. v3.0 모델과 기대 동작

운영자가 지정한 후보 배열에서 고르던 방식을 해석적 최적화로 바꾼다. `spec.checkpoint.candidateIntervalSeconds`와 `spec.checkpoint.paperProfile.candidateIterations`는 deprecated 호환 필드이며 solver가 무시한다. `candidateIterations`는 더 이상 필수 입력이 아니다. 기존 배열이 남아 있어도 이를 삭제하거나 새 후보를 작성할 필요가 없다.

`minIntervalSeconds` 생략 시 하한은 1초, `maxIntervalSeconds` 생략 시 상한은 `max(600, 유효 하한)`초다. 따라서 둘 다 생략하면 1~600초, min만 900이면 900~900초다. 명시한 유효 범위가 우선하며 하한은 상한 이하여야 한다. 스케줄의 int32 상한은 `2147483647`초다. 기존 `min=120, max=120` 정책은 후보 배열을 무시해도 계속 120초로 제한된다. 생략 시 기본값은 기존 명시값을 덮어쓰지 않는다.

| 상태 | 예상 intervalSource | 의미 |
| --- | --- | --- |
| 비용 측정 부재·만료 또는 모델 입력/제약 불충족 | `risk-band-bootstrap` | 범위 안의 초기 위험 구간 간격. 최적값이 아니다. |
| 기본 경로에서 유효한 실측 checkpoint/copy 비용 확보 | `checkpoint-cost-analytic` | 동기식 실측 비용 모델의 해석적 적응 |
| 활성화한 paper 경로에서 신선한 실제 비동기 측정과 실행 가능 제약 확보 | `paper-equations-1-5-analytic` | 논문 목적함수와 제약에 대한 정수 해 선택 |

논문 자체가 최적 주기의 직접적인 닫힌형 공식을 제시했다고 이해하면 정확하지 않다. Eq.5는 제약이 있는 정수 `argmin`이며 논문에서는 sweep으로 설명한다. v3.0은 **Eq.3에서 구간별 정지점을 유도하여 Eq.5를 푸는 구현**이다. 운영자의 후보 목록 없이 경계와 정지점 주변 정수를 비교한다는 의미에서 해석적 solver라고 부른다.

논문 경로에서 `f`는 양의 정수 iteration 간격, `t=T/Ndp`는 실측 iteration 시간, `D`는 GPU→DRAM 시간, `S_t`는 storage 시간, `S`는 checkpoint 크기, `C`는 DRAM buffer 용량이다. 크기 단위는 동일해야 한다.

```text
lambda_iter = lambda_hour * t * Ndp / 3600
Eq.3: cost(f) = t + D/f + max(0, S_t/f - t) + lambda_iter*f*t/2
Eq.4: f >= ceil(S_t*S/(C*t))
Eq.5: 위 제약과 시간 범위 안에서 cost(f)를 최소화하는 정수 f 선택
schedule intervalSeconds = ceil(f*t)
```

`b=lambda_iter*t/2`라 두면 storage 지연이 노출되는 구간은 `(D+S_t)/f+b*f`, 숨겨지는 구간은 `t+D/f+b*f`다. `b>0`일 때 각각의 연속 정지점은 `sqrt((D+S_t)/b)`, `sqrt(D/b)`다. solver는 구간 경계 `S_t/t`, buffer 제약, 시간 범위, 인접 정수를 함께 고려한다. 위험률 0과 실행 가능한 정수가 없는 경우도 별도로 처리한다. 모델 안의 최적화가 실제 시스템의 전역 최적 성능을 보증하지는 않는다.

논문 충실성을 위해 이 경로의 `Ndp`는 runtime world-size로 유지한다. 논문의 `lambda_hour`는 예측한 pool VM-loss COUNT/hour이며 명시적인 per-VM 정규화 위험률이 아니다. 위 식의 Ndp는 `T=t*Ndp`에서 나오며 별도의 hazard 집계 인자를 추가한 것이 아니다. `paperProfile.enabled` 사용은 입력 위험률의 의미가 논문과 일치한다는 전제를 포함한다. 현재 static risk의 의미가 다르면 식을 구현했더라도 논문의 실험적 재현이라고 주장할 수 없다. 기본 실측 동기식 경로도 별도의 비용 적응 모델이다. 최종 식과 반올림·동률 처리는 고정할 릴리스 commit의 `internal/policy/paper.go`, `internal/policy/contract.go` 및 테스트를 기준으로 확인한다.

또한 member scheduler는 이전 checkpoint **완료 후** intervalSeconds만큼 기다린다. `ceil(f*t)`는 iteration 해를 정수 초 스케줄로 옮기는 적응이며 정확히 f번째 iteration에 trigger하거나 시작부터 다음 시작까지 같은 간격을 보장하지 않는다. checkpoint 실행 시간, 스케줄 지연, 변화하는 iteration 시간 때문에 실제 시작 간격은 다를 수 있다. 수학적 모델의 정수 최적해와 운영 스케줄의 실제 최적성은 구별해야 한다.

논문 출처: Desai, Pei, Bhimani, Kim, EuroMLSys '26, pp. 31–40, [DOI: 10.1145/3805621.3807617](https://doi.org/10.1145/3805621.3807617). 위 설명은 논문의 Eq.3–5와 v3.0의 구현상 적응을 구분한다.

paper 경로에는 실제 `gpuToDramSeconds`, `storageSeconds`, `checkpointGiB`, `bufferGiB`, 측정 시각과 정렬된 world iteration 시간 측정이 필요하다. CRIU 시작·완료 timestamp만으로 GPU→DRAM 비용, 비동기 storage overlap, DRAM buffer 용량을 알 수 없다. `asynchronous=true`를 적거나 timestamp를 현재 시각으로 바꾸는 것으로 증거를 만들 수 없다. 검증되지 않은 `paperProfile`을 강제로 채우지 않는다. 최초 실제 checkpoint 비용을 기다리는 동안 bootstrap일 수 있으며, 측정 만료 후에도 fallback할 수 있다.

`policy-manager`도 공통 `Decide`/`DecideAt`를 사용해 status와 economics를 계산하므로 두 이미지를 같은 commit에서 빌드한다. interval의 `intervalCostEvaluated`와 economics의 `costEvaluated`는 별개다.

## 2. 환경과 격리된 소스 확보

Linux MGMT Bash에서 Git, Go 1.24 이상(고정 commit의 go.mod 충족), Docker daemon/Buildx, kubectl, jq를 준비한다. 별도 wrapper 없이 전용 Bash 세션에서 순서대로 실행하고 실패하면 다음 단계로 넘어가지 않는다. kubeconfig 경로 세 개를 실제 파일로 바꾼다. kubeconfig 내용과 Secret data는 출력·백업하지 않는다.

```bash
set -euo pipefail
umask 077
export MGMT_KUBECONFIG=/absolute/path/to/mgmt.kubeconfig
export KARMADA_KUBECONFIG=/absolute/path/to/karmada.kubeconfig
export AWS_KUBECONFIG=/absolute/path/to/aws.kubeconfig
test -s "$MGMT_KUBECONFIG"
test -s "$KARMADA_KUBECONFIG"
test -s "$AWS_KUBECONFIG"
export NS=fluidcr-realign-121040
export POLICY=trainer-realign
export RELEASE_ROOT="$(mktemp -d "$HOME/checkpoint-v3.0-20261008.XXXXXXXX")"
export EVIDENCE="$RELEASE_ROOT/evidence"
mkdir "$EVIDENCE"
git clone --single-branch --branch restore-automation-20260928 https://github.com/GProjectdev/HybridSpotVM_ManagementSystem.git "$RELEASE_ROOT/System"
cd "$RELEASE_ROOT/System"
git checkout restore-automation-20260928
export RELEASE_COMMIT=933878e10abb3b0a16d4ec4e40e17bba782f2a53
printf '%s\n' "$RELEASE_COMMIT" | grep -Eq '^[0-9a-f]{40}$'
git cat-file -e "${RELEASE_COMMIT}^{commit}"
git merge-base --is-ancestor "$RELEASE_COMMIT" origin/restore-automation-20260928
git show --no-patch --format=fuller "$RELEASE_COMMIT"
git checkout --detach "$RELEASE_COMMIT"
test "$(git rev-parse HEAD)" = "$RELEASE_COMMIT"
test -z "$(git status --porcelain)"
git rev-parse HEAD > "$EVIDENCE/release.commit"
git remote get-url origin
go version
docker version
docker buildx version
kubectl version --client
jq --version
printf 'Release evidence: %s\n' "$EVIDENCE"
```

`mktemp`로 만든 새 디렉터리만 사용하므로 기존 dirty checkout은 건드리지 않는다. SHA를 가져올 수 없거나 해당 브랜치에 포함되지 않으면 중단하고 공개된 최종 commit을 확인한다.

## 3. 테스트·빌드와 Docker Hub push

```bash
go test -mod=readonly ./...
go vet -mod=readonly ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=readonly -trimpath -o "$EVIDENCE/checkpoint-coordinator" ./cmd/checkpoint-coordinator
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=readonly -trimpath -o "$EVIDENCE/policy-manager" ./cmd/policy-manager
docker login --username jeongseungjun
docker buildx inspect --bootstrap
export COORD_TAG=jeongseungjun/hybrid-spot-vm-system:checkpoint_coordinator_v3.0
export POLICY_TAG=jeongseungjun/hybrid-spot-vm-system:policy_manager_v3.0
docker buildx build --platform linux/amd64 --build-arg COMPONENT=checkpoint-coordinator --label "org.opencontainers.image.revision=$RELEASE_COMMIT" --tag "$COORD_TAG" --push --metadata-file "$EVIDENCE/checkpoint-coordinator-build.json" .
docker buildx build --platform linux/amd64 --build-arg COMPONENT=policy-manager --label "org.opencontainers.image.revision=$RELEASE_COMMIT" --tag "$POLICY_TAG" --push --metadata-file "$EVIDENCE/policy-manager-build.json" .
export COORD_DIGEST="$(jq -er '."containerimage.digest" | select(type == "string") | select(test("^sha256:[0-9a-f]{64}$"))' "$EVIDENCE/checkpoint-coordinator-build.json")"
export POLICY_DIGEST="$(jq -er '."containerimage.digest" | select(type == "string") | select(test("^sha256:[0-9a-f]{64}$"))' "$EVIDENCE/policy-manager-build.json")"
test -n "$COORD_DIGEST"
test -n "$POLICY_DIGEST"
export COORD_IMAGE="${COORD_TAG}@${COORD_DIGEST}"
export POLICY_IMAGE="${POLICY_TAG}@${POLICY_DIGEST}"
docker buildx imagetools inspect "$COORD_IMAGE"
docker buildx imagetools inspect "$POLICY_IMAGE"
printf '%s\n' "$COORD_IMAGE" > "$EVIDENCE/checkpoint-coordinator.image"
printf '%s\n' "$POLICY_IMAGE" > "$EVIDENCE/policy-manager.image"
```

로그인은 대화형으로 수행하고 비밀번호/token을 명령행에 넣지 않는다. 두 build/push와 digest 검사가 모두 성공해야 배포한다. 실제 참조는 `checkpoint_coordinator_v3.0@sha256:...`, `policy_manager_v3.0@sha256:...`로 tag가 보이면서 내용은 immutable digest로 고정된다. Buildx가 manifest index를 만들었다면 Pod imageID는 플랫폼 manifest digest일 수 있으므로 index digest와 무조건 문자열 동등 비교하지 않는다.

## 4. 배포 대상 확인·이미지 백업·작업 게이트

| kubeconfig | 용도 |
| --- | --- |
| `MGMT_KUBECONFIG` | 두 controller Deployment 이미지 변경·rollout |
| `KARMADA_KUBECONFIG` | `config/crd/trainingpolicies.yaml` 적용, TrainingPolicy/status 조회 |
| `AWS_KUBECONFIG` | 기존 member workload/PVC, checkpoint schedule·실행 증거 조회 |

```bash
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system get deployment checkpoint-coordinator policy-manager -o wide
kubectl --kubeconfig="$KARMADA_KUBECONFIG" get namespace "$NS"
kubectl --kubeconfig="$AWS_KUBECONFIG" get namespace "$NS"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingpolicy "$POLICY" -o json | jq '{name:.metadata.name,uid:.metadata.uid,generation:.metadata.generation,suspend:.metadata.annotations["training.dcnlab.com/suspend"],checkpoint:.spec.checkpoint,status:.status}' > "$EVIDENCE/policy-before.json"
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get statefulsets,pods,pvc -o json | jq '[.items[] | {kind:.kind,name:.metadata.name,uid:.metadata.uid,phase:.status.phase}] | sort_by(.kind,.name)' > "$EVIDENCE/workloads-before.json"
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system get deployment checkpoint-coordinator policy-manager -o json | jq '[.items[] | {name:.metadata.name,uid:.metadata.uid,images:[.spec.template.spec.containers[] | {name,image}]}]' > "$EVIDENCE/deployment-images-before.json"
jq -e 'length == 2 and all(.[]; ([.images[] | select(.name == "manager") | .image] | length == 1) and all(.images[]; .image | type == "string" and length > 0))' "$EVIDENCE/deployment-images-before.json"
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system get deployment checkpoint-coordinator policy-manager -o json | jq '[.items[] | {name:.metadata.name,replicas:.spec.replicas,strategy:.spec.strategy.type,containers:[.spec.template.spec.containers[] | {name,command,args}]}]'
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get spotreplacements,restorerequests,spotrecoveries -o json | jq '[.items[] | {kind:.kind,name:.metadata.name,status:.status}]'
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get restoreplans,fluidcrmigrations -o json | jq '[.items[] | {kind:.kind,name:.metadata.name,schedule:.spec.schedule,status:.status}]'
```

이미지 백업에는 Deployment 이름·UID와 container 이름·image만 저장한다. Deployment 전체 YAML, env, Secret, kubeconfig는 백업하지 않는다. command/args는 로컬에서 확인하고 로그 공유 시 민감값을 노출하지 않는다. 아래 `set image`는 `manager` image만 바꿔 기존 args, volume, RBAC, replica 설정을 보존한다. 두 Deployment가 기존 RollingUpdate 전략인지 확인한다. 다르거나 필수 객체가 없으면 전체 manifest로 덮어써 맞추지 말고 중단한다.

진행 중 replacement/restore와 checkpoint round가 끝난 안정 구간에서만 변경한다. `Running`, `AwaitingPartialCheckpoint`, `RestoreReady`, 미완료 verification, active child 등이 있으면 완료될 때까지 기다리고 다시 조회한다. `RestoreReady`만으로 복원 완료라 판단하지 않는다. 실패한 operation도 소유권·정리 상태 확인 전에는 안전한 완료로 보지 않는다. 공유 controller가 다른 정책도 처리한다면 그 정책의 진행 중 작업도 확인한다.

새 임시 suspend annotation을 추가하거나 기존 annotation을 제거하지 않는다. 코드의 suspend는 새 intent를 제한하면서 기존 replacement의 checkpoint coordination을 계속할 수 있어 즉시 정지나 원자적 배포 잠금이 아니다. 기존 suspend가 켜져 있으면 그 상태를 보존하고 정기 checkpoint가 자동 시작한다고 기대하지 않는다. 이미지 변경 직전에도 위 상태를 재확인하며 새 operation이 시작되면 변경을 미룬다.

## 5. CRD와 두 controller 이미지 적용

먼저 CRD 서버 검증을 실행한다. 클러스터 범위 스키마 변경이므로 다른 TrainingPolicy에도 영향을 줄 수 있다. 고정 commit에서 candidateIterations 필수 해제, deprecated 필드 호환성과 interval 범위를 확인한다. workload와 TrainingPolicy 본문은 덮어쓰지 않는다.

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply --dry-run=server -f config/crd/trainingpolicies.yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f config/crd/trainingpolicies.yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" wait --for=condition=Established --timeout=60s crd/trainingpolicies.training.dcnlab.com
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system set image deployment/checkpoint-coordinator "manager=$COORD_IMAGE"
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system rollout status deployment/checkpoint-coordinator --timeout=180s
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system set image deployment/policy-manager "manager=$POLICY_IMAGE"
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system rollout status deployment/policy-manager --timeout=180s
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system get deployment checkpoint-coordinator policy-manager -o json | jq -e --arg coord "$COORD_IMAGE" --arg policy "$POLICY_IMAGE" 'all(.items[]; ([.spec.template.spec.containers[] | select(.name == "manager") | .image][0]) == (if .metadata.name == "checkpoint-coordinator" then $coord else $policy end))'
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system get pods -l 'app in (checkpoint-coordinator,policy-manager)' -o json | jq '[.items[] | {name:.metadata.name,containers:.status.containerStatuses}]'
```

두 Deployment 업데이트는 원자적이지 않다. 첫 rollout이 실패하면 다음 변경으로 진행하지 않는다. 중간에 replacement/restore가 시작되면 추가 변경을 멈추고 완료·안전 상태를 확인한다. 롤링되는 Pod는 관리 controller이며 학습 worker를 rollout/restart하지 않는다. 기존 정책 reconcile은 계속되므로 workload UID 변화가 있으면 외부 작업과 recovery operation을 확인한다.

## 6. status·member schedule·첫 실측 확인

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingpolicy "$POLICY" -o json | jq '{generation:.metadata.generation,checkpointSpec:.spec.checkpoint,policy:.status.policy,checkpoint:.status.checkpoint}'
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingruntime -o json | jq '[.items[] | {name:.metadata.name,status:.status}]'
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get fluidcrmigrations -l training.dcnlab.com/role=checkpoint-schedule -o json | jq '[.items[] | {name:.metadata.name,schedule:.spec.schedule,status:.status}]'
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get fluidcrmigrations -o json | jq '[.items[] | {name:.metadata.name,uid:.metadata.uid,labels:.metadata.labels,schedule:.spec.schedule,currentRun:.status.currentRun,lastSuccessfulFullCheckpoint:.status.lastSuccessfulFullCheckpoint,phase:.status.phase}]'
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get fluidcrmigrations -l training.dcnlab.com/role=checkpoint-evidence -o json | jq '[.items[] | {name:.metadata.name,status:.status}]'
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system logs deployment/checkpoint-coordinator -c manager --since=10m --tail=150
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system logs deployment/policy-manager -c manager --since=10m --tail=150
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get statefulsets,pods,pvc -o json | jq '[.items[] | {kind:.kind,name:.metadata.name,uid:.metadata.uid,phase:.status.phase}] | sort_by(.kind,.name)' > "$EVIDENCE/workloads-after.json"
diff -u "$EVIDENCE/workloads-before.json" "$EVIDENCE/workloads-after.json"
```

diff가 있으면 UID와 phase를 구분해 확인한다. phase 차이만으로 재생성이라 판단하지 않는다. Pod/PVC UID 변화가 있으면 의도한 controller image 업데이트 외의 작업·복구가 있었는지 조사한다. `set -e` 세션에서는 diff 발견 시 종료될 수 있으므로 후속 작업은 동일 RELEASE_ROOT/EVIDENCE와 이미지 변수를 확인하고 재개한다.

정상 작동 판단은 다음 증거를 함께 본다.

- 두 Deployment manager가 검증한 tag@digest를 사용하고 rollout이 완료된다.
- status.policy와 status.checkpoint의 checkpointIntervalSeconds, intervalSource, intervalCostEvaluated, 관측 시각을 읽는다. reconcile 시점·확보한 증거가 달라 일시적으로 값이 다를 수 있다.
- 초기 risk-band-bootstrap과 intervalCostEvaluated=false는 최적화 완료가 아니다. 실제 비용이 수집되면 기본 경로가 checkpoint-cost-analytic으로 전환되는지 확인한다. 유효한 입력이 없는데 analytic을 기대하며 숫자나 status를 조작하지 않는다.
- 일반적인 parent 이름은 `<policy>-periodic`이다. Karmada 의도뿐 아니라 AWS member의 spec.schedule과 현재 round를 확인한다. parent가 아직 없으면 discovery/runtime/risk freshness, 기존 suspend, active operation, controller 로그부터 확인한다.
- 실제 첫 round 완료 후 member lastSuccessfulFullCheckpoint와 Karmada checkpoint-evidence, 비용 측정 시각이 새 실행과 연결되는지 확인한다. 기존 PVC의 오래된 checkpoint를 이번 성공 증거로 사용하지 않는다.
- risk_not_fresh, runtime_not_ready, waiting_for_discovery, policy_suspended, replacement/restore 대기 reason은 각각의 선행조건을 의미한다. 스케줄이나 bootstrap interval만으로 checkpoint 성공을 주장하지 않는다.

raw spec에 candidate 배열이 남는 것은 호환 필드 보존이다. 선택 제약이 아니므로 fake paperProfile이나 임의 candidate 배열을 patch할 필요가 없다. 실제 비동기 계측이 없다면 논문 async 경로는 미검증이라고 기록하고 기본 경로의 실측 적응만 검증한다.

## 7. 이미지 롤백

롤백도 진행 중 replacement/restore가 끝난 안정 구간에서 수행한다. 백업한 이미지 참조를 사용하며 revision이나 과거 tag를 추측하지 않는다. 기존 참조가 mutable tag뿐이었다면 이전 bytes 재현은 보장되지 않는다. 그 경우 사전에 보관한 이전 digest가 필요하며 임의로 만들지 않는다.

```bash
export OLD_COORD_IMAGE="$(jq -er '[.[] | select(.name == "checkpoint-coordinator") | .images[] | select(.name == "manager") | .image] | select(length == 1) | .[0] | select(type == "string" and length > 0)' "$EVIDENCE/deployment-images-before.json")"
export OLD_POLICY_IMAGE="$(jq -er '[.[] | select(.name == "policy-manager") | .images[] | select(.name == "manager") | .image] | select(length == 1) | .[0] | select(type == "string" and length > 0)' "$EVIDENCE/deployment-images-before.json")"
test -n "$OLD_COORD_IMAGE"
test -n "$OLD_POLICY_IMAGE"
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system set image deployment/checkpoint-coordinator "manager=$OLD_COORD_IMAGE"
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system rollout status deployment/checkpoint-coordinator --timeout=180s
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system set image deployment/policy-manager "manager=$OLD_POLICY_IMAGE"
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system rollout status deployment/policy-manager --timeout=180s
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system get deployment checkpoint-coordinator policy-manager -o json | jq '[.items[] | {name:.metadata.name,containers:[.spec.template.spec.containers[] | {name,image}]}]'
```

CRD 삭제나 구버전 CRD 덮어쓰기, workload/PVC 삭제, status/finalizer 조작은 롤백 수단이 아니다. 호환 필드를 유지한 새 CRD를 남기고 이미지를 되돌린 뒤 6절 조회로 확인한다. 구버전 바이너리는 기존 candidate 필드를 다시 사용할 수 있으므로 v3.0과 같은 간격 선택을 기대하지 않는다.

완료 기록에는 고정 commit, 두 build metadata/digest, 기존 이미지 백업, 실제 rollout 결과와 관측한 interval source를 남긴다. 빌드·push, 클러스터 rollout, 첫 checkpoint, 논문 async 검증 중 실행하지 않은 항목은 각각 미검증으로 표시한다.
