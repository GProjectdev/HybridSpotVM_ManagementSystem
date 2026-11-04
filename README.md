# Hybrid Spot VM Management System

## E3 Controlled Evaluation

See [E3 preparation, pilot, execution, evidence and cleanup](docs/e3-controlled-comparison.md).
The new runner compares fixed On-Demand, fixed hybrid, and risk-controlled hybrid arms.
Run the no-FIS pilot before approving the preregistered measurement plan.
No controller image or upstream Karmada change is required by these evaluation scripts.

## 설치 범위와 지원 조건

이 저장소는 고정 크기 DDP 학습의 Spot/On-Demand 용량 선택, 체크포인트 요청,
배치 변경 조정 및 복원 검증을 담당하는 Kubernetes 컨트롤러 모음입니다.
**일반 Kubernetes를 기반으로 하지만 단일 클러스터에 설치하면 하이브리드
클라우드가 자동 구성되는 제품은 아닙니다. 하이브리드 클라우드를 먼저 구성해야 합니다.**

`main`에는 `restore-automation-20260928`의 구현이 통합되어 있습니다.
새 설치는 `main`을 사용하고, 재현 실험은 기록한 commit SHA로 고정하세요.
System, Provisioner, FluidCR, Stateful Migration, PV Migration의 버전을 별도로 기록합니다.
이 저장소를 업데이트했다고 다른 저장소의 컨트롤러나 노드 런타임이 갱신되지는 않습니다.

### 사전 구성 체크리스트

| 영역 | 설치 전에 준비할 것 / 통과 기준 |
|---|---|
| 클러스터 | MGMT 실행 클러스터, 변경하지 않은 upstream Karmada, 등록된 소스/대상 Kubernetes Member. Karmada Cluster가 Ready여야 합니다. MGMT는 컨트롤러 실행 위치이고 Karmada는 별도 API입니다. |
| 하이브리드 네트워크 | VPN/전용망/라우팅, 중복되지 않는 노드·Pod·Service 주소 설계, 방화벽, DNS, 시간 동기화. API, webhook, NFS, artifact 및 DDP 통신을 실제 양방향으로 시험합니다. 특정 WireGuard 구성은 필수가 아닙니다. |
| 전송 성능 | 노드 간 ping뿐 아니라 Pod 간 대용량 전송, MTU, 재전송, VPN 대역폭/버스트 한도와 DDP collective를 검증합니다. Ready나 작은 ping 성공은 성능 검증이 아닙니다. |
| 클라우드 | 현재 용량 공급 코드는 AWS NodeProvision 계약에 연결됩니다. VPC/subnet/보안 그룹/AMI/region/AZ, IAM, Spot 및 On-Demand vCPU quota와 가용 용량을 준비합니다. 다른 클라우드는 어댑터 개발이 필요합니다. |
| GPU와 런타임 | GPU Operator 또는 동등한 드라이버·device plugin·container toolkit, GPU allocatable, 호환되는 CUDA/CRIU/CRI-O checkpoint/restore 경로. 일반 GPU Pod 실행만으로 프로세스 복원 지원이 증명되지는 않습니다. |
| 스토리지 | 양쪽 노드에서 접근 가능한 NFS 및 export rw 권한, NFS client, namespace별 PVC와 실제 PV binding. 현재 이관 경로는 동일 NFS server/path 재바인딩이며 임의 CSI 볼륨의 데이터 복사 기능이 아닙니다. |
| Artifact | 체크포인트 archive 저장소, 대상 노드 접근, SHA256 및 durableRef 검증, 충분한 디스크. 학습 PVC와 artifact 저장소는 별도로 점검합니다. |
| 외부 시스템 | 아래 연동 계약을 충족하는 Provisioner, FluidCR, Stateful Migration, PV Migration의 CRD·컨트롤러·admission·RIC. CRD만 설치해 두는 것으로 충분하지 않습니다. |
| 보안 | 관리자 kubeconfig와 컨트롤러용 제한된 kubeconfig 분리, Secret 접근 제한, TLS 인증서·토큰 갱신, registry pull 권한. Secret/token/private key는 Git이나 실험 증거에 넣지 않습니다. |
| 작업 환경 | Linux 관리 단말, git, kubectl, Bash, Python 3와 PyYAML, jq, Buildah. 개발 검증은 go.mod 및 Dockerfile에 맞는 Go toolchain을 사용합니다. |

Kubernetes/Karmada 버전 전체에 대한 호환성을 보장하지 않습니다. 사용할 버전과 노드 이미지 조합에서
CRD admission, status reflection, checkpoint 및 실제 restore를 검증한 뒤 운영하세요.
네트워크·보안·비용 정책은 운영자가 결정하며 아래 설치 스크립트는 이를 생성하거나 변경하지 않습니다.

## 구성과 연동 시스템

| 실행 위치 | 컴포넌트 | 역할 |
|---|---|---|
| MGMT | vm-spot-risk-collector | 외부 hazard / 명시적 실험 입력을 SpotRiskProfile로 반영 |
| MGMT | policy-manager | 사용자 TrainingPolicy에 따라 용량과 이동/복원 흐름 조정 |
| MGMT | checkpoint-coordinator | 체크포인트 간격 계산 및 요청 |
| MGMT | spot-recovery-controller | 검증된 복구에 연결된 이전 Spot 자원 정리 |
| MGMT | placement-webhook | ResourceBinding 이동 보류 및 검증된 release 경로 |
| 각 학습 Member | training-runtime-collector | 학습 payload의 runtime 상태 수집 |
| AWS Member 노드 | spot-watcher | IMDSv2 중단 신호를 NodeProvision 상태에 반영 |

기본 통합 대상:
- [NodeProvisioner](https://github.com/GProjectdev/PublicCloud-VM-Provisioner_test/tree/In-aws-create-WorkerNode): VM 생성/삭제, bootstrap, 노드 등록과 runtime qualification.
- [Stateful Migration](https://github.com/GProjectdev/Stateful-Migration-Operator-with-PV): checkpoint export, archive 준비, source fencing, restore 검증.
- [PV Migration](https://github.com/GProjectdev/Karmada_with_PVMigration): PVMetadata, PVMigration, PV/PVC Work 및 binding receipt.
- FluidCR: 학습 payload와 admission webhook, DDP checkpoint/resume. 프로젝트의 My_FluidCR checkout과 Stateful Migration overlay를 호환되는 revision으로 함께 준비합니다.

PV Migration 저장소 이름에 Karmada가 포함되어 있어도 **Karmada 자체의 코드나 이미지를 변경하지 않습니다.**
CRD/RBAC/RIC/Work/admission 확장만 사용합니다. 동일 리소스·operation의 기존 RIC와 중복 등록하지 마세요.
이미 있는 RIC는 백업하고 기능을 비교하여 담당 구성 하나로 관리합니다.

### 다른 시스템 또는 사전 구성 자원 사용

임의 backend를 고르는 범용 plugin 설정은 아직 없습니다. 이름이나 이미지만 바꾸면 호환되는 구조가 아닙니다.
외부 시스템을 바꿀 때는 기존 API 계약을 구현하는 어댑터 또는 이 저장소의 controller 변경이 필요합니다.

| 교체 대상 | 유지해야 하는 계약 / 검증 |
|---|---|
| VM 공급 | NodeProvision spec/status, 인스턴스·노드 identity, ownership/finalizer, readiness, 시장 유형 및 중단 신호. 중복 VM 생성과 잘못된 노드 삭제 방지. |
| FluidCR | FluidCRMigration 및 runtime HTTP 계약, rank/world/Pod UID, 일관된 checkpoint round, model·optimizer·RNG·step 복원, DDP 통신 재구성. 애플리케이션 checkpoint 재시작을 프로세스 restore와 동일하게 보고하면 안 됩니다. |
| Stateful Migration | RestoreRequest/RestorePlan, archive hash/durableRef, 원본 실행 차단, 대상 검증 receipt와 올바른 status 반영. Pod Ready를 Verified로 대신하지 않습니다. |
| PV Migration | 원본 메타데이터 identity, source fencing, PV/PVC reciprocal UID binding, 동일 NFS backing, 별도 PV/PVC Work와 durable receipts. 다른 CSI/backend는 별도 데이터 이동·무결성 계약이 필요합니다. |
| 위험률 공급 | 시간당 hazard 의미, 유효기간·관측 시각·generation. Spot 가격이나 placement score를 실제 중단 hazard로 취급하지 않습니다. |

이미 VM/GPU/NFS를 구성한 경우 설치 도구는 그것을 다시 만들지 않습니다.
하지만 사전 생성 노드가 있다는 이유로 정책의 자동 VM 공급이 비활성화되는 것은 아닙니다.
자동 용량을 쓰려면 NodeProvision identity/관리권을 맞추고, 외부 공급만 쓰려면 해당 동작을
어댑터 또는 controller 수준에서 분리·검증해야 합니다. 서로 다른 컨트롤러가 같은 VM을 소유하지 않게 하세요.
어댑터 검증은 정상 복원뿐 아니라 stale UID, 누락 archive, 잘못된 backing, 중복 요청,
부분 실패, 재시작 및 삭제 재시도를 포함해야 합니다.

## 1. 소스와 이미지 준비

다음 예제는 새 설치용입니다. 기존 운영 클러스터에서는 먼저 CRD·Deployment·RBAC·RIC·webhook을
백업하고 활성 checkpoint/restore/replacement가 끝난 뒤 변경하세요. 이전 단일
`hybridspot-management`와 split controller를 동시에 실행하지 않습니다.

```bash
git clone --branch main https://github.com/GProjectdev/HybridSpotVM_ManagementSystem.git
cd HybridSpotVM_ManagementSystem
git rev-parse HEAD
python3 -m venv .venv
. .venv/bin/activate
python3 -m pip install PyYAML==6.0.2

export MGMT_KUBECONFIG=/secure/mgmt.kubeconfig
export KARMADA_KUBECONFIG=/secure/karmada.kubeconfig
export ONPREM_KUBECONFIG=/secure/source.kubeconfig
export AWS_KUBECONFIG=/secure/target.kubeconfig
export IMAGE_REPOSITORY=registry.example.org/team/hybridspot-system
export IMAGE_TAG="$(git rev-parse --short=12 HEAD)"
```

`registry.example.org/team`은 자신의 쓰기·읽기 가능한 registry 경로로 바꾸세요.
예제 태그의 이미지가 미리 공개되어 있다고 가정하지 않습니다.

```bash
buildah login registry.example.org
(
  set -euo pipefail
  for COMPONENT in vm-spot-risk-collector policy-manager checkpoint-coordinator spot-recovery-controller training-runtime-collector spot-watcher placement-webhook; do
    IMAGE="$IMAGE_REPOSITORY:$COMPONENT-$IMAGE_TAG"
    buildah bud --build-arg COMPONENT="$COMPONENT" -t "$IMAGE" .
    buildah push "$IMAGE" "docker://$IMAGE"
  done
)
```

Docker daemon은 사용하지 않습니다. Dockerfile은 Buildah의 build recipe입니다.
private registry는 각 실행 클러스터의 `hybridspot-system`에 pull Secret을 준비하고,
아래 렌더링에 `--pull-secret <secret-name>`을 추가하세요.

## 2. 설치 도구와 CRD

`scripts/install-system.py`는 저장소 manifest를 재사용합니다.
기본은 오프라인 JSON 렌더링이며 클러스터에 접근하지 않습니다.
`--apply --kubeconfig ... --confirm-context ...`를 지정해야 변경합니다.
context가 다르면 중단하고 CRD Established / controller rollout을 기다립니다.
일부 apply 후 실패할 수 있으며 자동 rollback이나 삭제를 하지 않습니다.

| stage | 적용 위치 | 포함 범위 |
|---|---|---|
| crds | Karmada와 모든 학습 Member | 이 저장소의 training.dcnlab.com CRD |
| karmada | Karmada API만 | System 서비스 계정/RBAC와 TrainingRuntime RIC |
| management | MGMT | namespace와 4개 management Deployment |
| member | 비-AWS 학습 Member | runtime RBAC/collector (AWS watcher 없음) |
| aws-member | AWS Member | runtime collector + SpotWatcher |
| placement | MGMT | placement webhook Deployment와 NodePort Service |

각 kubeconfig가 의도한 클러스터인지 먼저 확인합니다. 경로 변수 이름만 믿지 마세요.
```bash
kubectl --kubeconfig="$MGMT_KUBECONFIG" config current-context
kubectl --kubeconfig="$KARMADA_KUBECONFIG" config current-context
kubectl --kubeconfig="$ONPREM_KUBECONFIG" config current-context
kubectl --kubeconfig="$AWS_KUBECONFIG" config current-context
kubectl --kubeconfig="$KARMADA_KUBECONFIG" get clusters

python3 scripts/install-system.py crds --out rendered/crds.json
python3 scripts/install-system.py karmada --out rendered/karmada.json
```

파일을 검토한 뒤 각 stage를 적용합니다. 아래 context 조회는 앞에서 확인한 kubeconfig에 한해서 사용합니다.
```bash
(
  set -euo pipefail
  for CONFIG in "$KARMADA_KUBECONFIG" "$ONPREM_KUBECONFIG" "$AWS_KUBECONFIG"; do
    CONTEXT=$(kubectl --kubeconfig="$CONFIG" config current-context)
    python3 scripts/install-system.py crds --out rendered/crds.json \
      --apply --kubeconfig "$CONFIG" --confirm-context "$CONTEXT"
  done
  CONTEXT=$(kubectl --kubeconfig="$KARMADA_KUBECONFIG" config current-context)
  python3 scripts/install-system.py karmada --out rendered/karmada.json \
    --apply --kubeconfig "$KARMADA_KUBECONFIG" --confirm-context "$CONTEXT"
)
```

이 단계에는 외부 시스템 CRD가 포함되지 않습니다. 앞의 연동 저장소 설치 절차에 따라
NodeProvision, FluidCRMigration, RestoreRequest/Plan, PVMetadata/PVMigration CRD와
필요한 RIC/admission을 먼저 구성하세요. NodeProvision status RIC 예제는
`config/runbook/nodeprovision-status.yaml`이며 기존 동일 operation RIC 유무부터 확인합니다.

## 3. 컨트롤러 인증과 배포

Karmada RBAC 설치 후 아래 helper는 컨트롤러 서비스 계정의 token을 요청합니다.
24시간을 요청하지만 실제 만료는 서버 설정을 따릅니다. 운영에서는 자동 갱신 체계를 마련하고,
갱신 후 Secret과 해당 Deployment를 갱신하세요. 관리자 kubeconfig를 그대로 Pod에 넣지 마세요.

```bash
export OUTPUT_KUBECONFIG=/secure/hybridspot-controller.kubeconfig
bash scripts/create-karmada-kubeconfig.sh
kubectl --kubeconfig="$MGMT_KUBECONFIG" apply -f config/management/namespace.yaml
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system create secret generic hybridspot-karmada-kubeconfig \
  --from-file=kubeconfig="$OUTPUT_KUBECONFIG" --dry-run=client -o yaml |
  kubectl --kubeconfig="$MGMT_KUBECONFIG" apply -f -

python3 scripts/install-system.py management --out rendered/management.json
python3 scripts/install-system.py member --out rendered/source-member.json
python3 scripts/install-system.py aws-member --out rendered/aws-member.json
```

렌더링된 이미지, 리소스 한도, 배치 조건과 Secret 이름을 검토하세요.
기본 manifest에는 전용 control-plane toleration을 자동 추가하지 않습니다.
해당 노드에만 배치할 환경이라면 검토한 overlay를 사용하세요.
렌더링 결과를 직접 편집했다면 아래 도구 재실행은 편집을 덮어쓰므로 해당 파일을 kubectl로 적용하세요.

```bash
python3 scripts/install-system.py management --out rendered/management.json --apply \
  --kubeconfig "$MGMT_KUBECONFIG" --confirm-context "$(kubectl --kubeconfig="$MGMT_KUBECONFIG" config current-context)"
python3 scripts/install-system.py member --out rendered/source-member.json --apply \
  --kubeconfig "$ONPREM_KUBECONFIG" --confirm-context "$(kubectl --kubeconfig="$ONPREM_KUBECONFIG" config current-context)"
python3 scripts/install-system.py aws-member --out rendered/aws-member.json --apply \
  --kubeconfig "$AWS_KUBECONFIG" --confirm-context "$(kubectl --kubeconfig="$AWS_KUBECONFIG" config current-context)"
```

AWS 아닌 Member에는 `member`를 사용합니다. AWS 위에 구성한 모의 on-prem이라면
실제 공급자와 NodeProvision ownership에 따라 watcher 필요 여부를 결정하세요.
Watcher는 `ml.dcn.ssu.ac.kr/provider=AWS` 노드와 IMDSv2를 사용합니다.
노드가 0개인 DaemonSet의 rollout 성공은 중단 신호 수집 성공을 뜻하지 않습니다.
runtime collector의 Pod IP:8298 접근은 내부 NetworkPolicy로 필요한 주체만 허용합니다.

## 4. Placement Webhook TLS와 등록

이 단계 없이 cross-cluster 이동이 보호된다고 판단하지 마세요.
`config/management/kustomization.yaml`은 placement webhook을 포함하지 않습니다.
Karmada API server가 실제로 접근 가능한 MGMT endpoint를 먼저 마련합니다.
기본 Service는 NodePort 30443입니다. 운영 LB를 사용하면 Service/URL을 함께 조정하세요.

1. 조직의 CA/cert-manager 등으로 endpoint DNS/IP SAN이 일치하는 서버 인증서를 발급합니다.
2. MGMT `hybridspot-system/placement-webhook-tls`에 `tls.crt`, `tls.key`를 준비합니다.
3. webhook을 배포하고 실제 Karmada API server 네트워크에서 TLS와 endpoint 도달성을 검증합니다.
4. URL과 신뢰 CA를 채운 webhook 설정을 Karmada API에 등록합니다.

```bash
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system create secret tls placement-webhook-tls \
  --cert=/secure/placement/tls.crt --key=/secure/placement/tls.key --dry-run=client -o yaml |
  kubectl --kubeconfig="$MGMT_KUBECONFIG" apply -f -
python3 scripts/install-system.py placement --out rendered/placement.json
python3 scripts/install-system.py placement --out rendered/placement.json --apply \
  --kubeconfig "$MGMT_KUBECONFIG" --confirm-context "$(kubectl --kubeconfig="$MGMT_KUBECONFIG" config current-context)"
export PLACEMENT_URL=https://mgmt-api-reachable.example.org:30443/hold-ddp-placement
export PLACEMENT_CA=/secure/placement/ca.crt
python3 - <<'PY'
import base64, json, os, pathlib, yaml
obj = yaml.safe_load(pathlib.Path("config/karmada/placement-webhook.yaml").read_text())
client = obj["webhooks"][0]["clientConfig"]
client["url"] = os.environ["PLACEMENT_URL"]
client["caBundle"] = base64.b64encode(pathlib.Path(os.environ["PLACEMENT_CA"]).read_bytes()).decode()
pathlib.Path("rendered/placement-registration.json").write_text(json.dumps(obj, indent=2))
PY
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f rendered/placement-registration.json
```

URL 예제는 실제 endpoint로 바꿔야 합니다. `failurePolicy: Fail`이므로 잘못 등록하면
ResourceBinding UPDATE가 막힐 수 있습니다. 운영 중 우회하거나 `Ignore`로 바꾸어 복원을
강행하지 마세요. release identity는 기본 서비스 계정과 일치해야 하며,
별도 identity를 쓰면 RBAC와 `PLACEMENT_RELEASE_USERNAME`도 함께 검토합니다.

## 5. 학습 작업 등록과 사용

자동 TrainingPolicy 생성은 제거되었습니다. 사용자가 workload와 TrainingPolicy를 소유합니다.
과거 automatic-policy-defaults 예제를 신규 설치에 적용하지 마세요.

1. Karmada와 참여 Member에 같은 application namespace를 준비합니다.
2. Member namespace에 공급자 credential Secret/NetConfig, 유효한 join token,
   runtime bootstrap 설정과 NFS 접근 권한을 준비합니다. Secret을 광범위하게 전파하지 않습니다.
3. 데이터·모델·checkpoint PVC를 준비하고 실제 server/path와 UID binding을 확인합니다.
   현재 full-world 경로의 공유 checkpoint 조건은 [full-group 가이드](docs/full-group-rollout.md)를 따릅니다.
   StatefulSet volumeClaimTemplates를 쓰는 임의 작업이 자동 호환된다고 가정하지 마세요.
4. payload가 주입된 단일 학습 컨테이너 DDP StatefulSet, headless Service와
   의존 ConfigMap, 소스 PropagationPolicy를 작성합니다. 모든 rank의 runtime 보고를 검증합니다.
5. [위험률 계약](docs/risk-feed.md)에 따라 SpotRiskProfile을 등록합니다.
   `config/samples/11-spot-risk-profile-static.yaml`은 실험 전용입니다.
6. Karmada StatefulSet의 실제 UID를 조회하여 `config/samples/12-training-policy.yaml`의
   workloadRef에 입력합니다. sourceCluster, world size, runtime/risk 참조,
   AWS capacity와 credentialsRef를 실제 환경 값으로 채웁니다.
7. 검토한 정책을 Karmada에 적용합니다. 이 시점부터 유료 VM이 생성될 수 있습니다.
   replica 수와 capacity quota를 먼저 확인하세요.
8. 사용자 소유 PropagationPolicy를 변경하여 이동을 요청하고, NodeProvision readiness,
   checkpoint export, source fencing, PV binding, RestoreRequest Verified,
   양쪽 rank의 복원 후 step 진행을 확인합니다.

```bash
export NS=training-demo
export APP=trainer
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get statefulset "$APP" \
  -o jsonpath='{.metadata.uid}{"\n"}'
# 편집/렌더링한 파일만 적용합니다. config/samples의 placeholder를 그대로 적용하지 않습니다.
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f rendered/risk-profile.yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f rendered/training-policy.yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingpolicies,trainingruntimes,restorerequests,pvmigrations
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get nodeprovisions,pods,pvc
```

위 `rendered/risk-profile.yaml`, `training-policy.yaml`은 운영자가 작성한 입력 파일입니다.
설치 도구는 workload, cloud credential, 위험률 feed 또는 application policy를 생성하지 않습니다.
표본 YAML은 [config/samples](config/samples), 단계별 검증은
[기존 환경 검증 가이드](docs/from-existing-mgmt-validation-guide.md)에서 확인합니다.
과거 가이드의 사이트 IP, namespace, 이미지 태그를 자신의 환경에 그대로 복사하지 마세요.

## 6. 검증, 업그레이드와 정리

- 설치 확인: 각 Deployment Ready, CRD Established, Karmada Member Ready, RIC 상태 반영,
  TLS/토큰 만료, controller 로그의 Forbidden/NotFound 및 reconcile error를 확인합니다.
- 실제 검증: GPU allocatable, runtime 도구의 동적 라이브러리 경로, NFS 읽기/쓰기,
  DDP 진행, archive SHA256, 동일 checkpoint에서 두 rank 복원 및 optimizer update 검증을 수행합니다.
- E1: [GPT-2 실행/증거 수집](docs/e1-gpt2-experiment.md),
  [Spot 구성](docs/e1-spot-v5.0.4.md), [PV/PVC split Work](docs/e1-split-work-v5.0.5.md).
  E1 성공만으로 E2/E3의 비용·안정성 또는 통계적 주장을 입증하지 않습니다.
- 업그레이드: commit/image digest와 기존 manifest를 보존하고, 의존 CRD/계약부터 검토한 뒤
  새 태그로 배포합니다. 기존 운영 이미지를 동일 태그로 덮어쓰지 마세요.
  CRD·runtime 변경은 단순 Deployment rollback만으로 되돌아가지 않을 수 있습니다.
- 중단: 실험 실행기 종료와 Pod 종료는 다릅니다. 정책 suspend, source/target workload 정지,
  관련 요청 종료를 확인한 뒤 원본 Karmada 리소스부터 정리합니다.
- 삭제: NodeProvision 삭제는 실제 VM 종료를 유발할 수 있습니다.
  VM 종료/잔여 Spot 요청을 공급자 API에서도 확인합니다. 공통 GPU Worker, NFS, VPN은 별도 자산입니다.
- 볼륨: Work ownership과 preserveResourcesOnDeletion, PV reclaimPolicy, 사용 중인 PVC를
  확인한 뒤 특정 실행만 정리합니다. PV/PVC 삭제와 NFS 실제 파일 삭제는 별개입니다.
  force finalizer 제거 또는 전체 namespace/CRD 삭제를 기본 정리 절차로 사용하지 않습니다.
- 증거: commit/image, 설정, event 시각, restore/update audit 및 PV/PVC UID를 보존합니다.
  토큰·Secret·개인키는 제외하고 재현 가능한 결과와 실패도 함께 기록합니다.

## 개발 검증

```bash
python3 -m unittest discover -s scripts -p 'test_install_system.py'
go test -mod=readonly ./...
go vet ./...
go build -mod=readonly ./cmd/...
```

로컬 테스트는 실제 Kubernetes admission, EC2 생성·삭제, GPU/NCCL 복원 검증을 대체하지 않습니다.
설치 도구는 운영 클러스터나 외부 dependency를 자동 업그레이드하지 않습니다.

## 추가 문서

- [Architecture and workflows](docs/architecture-workflows-ppt.md)
- [User-owned policy enrollment](docs/architecture-realignment.md)
- [Full-group acceptance](docs/full-group-rollout.md)
- [Restore safety contract](docs/spot-replacement-contract.md)
- [Integrated runtime upgrade](docs/integrated-upgrade-validation.md)
- [Risk feed contract](docs/risk-feed.md)
- [Evaluation scripts](evaluation/README.md)
- [E2 GPT-2 interruption, recovery and presentation evidence](docs/e2-gpt2-experiment.md)

과거 릴리스 문서는 특정 revision/실험 환경을 설명합니다. 신규 설치는 이 README를 우선하고,
과거 자동 enrollment, Docker 명령 또는 Karmada fork 안내를 현재 설치에 혼용하지 마세요.
