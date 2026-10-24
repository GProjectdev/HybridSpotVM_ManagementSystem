# onpre1에 빠진 멤버 구성 설치

이 문서는 기존 AWS 설정은 유지하고, onpre1의 누락된 멤버 구성만 설치한다.
MGMT의 기존 `stateful-management`, `pv-migration-management`는 이미 실행 중인
v3.1.1/v1.0 설정을 유지한다. 설치 파일을 렌더링할 때 원본 저장소는 수정하지 않는다.

## 전제

```bash
export ONPRE1_KUBECONFIG=/root/on-premise/member/onpre1.kubeconfig
export BASE=/root/hybridspot-validation
export CONTROL_IMAGE='docker.io/jeongseungjun/myfluidcr-operator@sha256:f4c52baff779d469b269bb5e525fa033c86d3569f418e7e750a698e6865599b9'
test -d "$BASE/stateful/config/member"
test -d "$BASE/pv/config/member"
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" -n stateful-migration-system \
  get pvc stateful-migration-artifacts
```

PVC는 Bound여야 하며 양쪽 GPU에서 같은 NFS 경로를 읽고 쓸 수 있어야 한다.
기존에 수행한 양방향 파일 검사 PASS 결과를 보존한다.

## Stateful 멤버와 Restore webhook

이 구성은 단순 checkpoint 컨트롤러와 다르다. 원본 Pod 종료 확인, 재생성 차단,
대상 RestorePlan 준비 및 Pod 변형을 담당한다. source/target 양쪽에 필요하다.

```bash
(
set -euo pipefail
DIR=$(mktemp -d /root/onpre1-stateful-member.XXXXXX)
cp -R "$BASE/stateful/config/member" "$DIR/base"
cat > "$DIR/kustomization.yaml" <<YAML
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- base
images:
- name: ghcr.io/gprojectdev/stateful-migration-system
  newName: docker.io/jeongseungjun/stateful-migration-system
  newTag: v3.1.1
patches:
- target:
    kind: Deployment
    name: stateful-member
  patch: |-
    - op: replace
      path: /spec/template/spec/containers/0/args
      value:
      - --mode=member
      - --cluster-name=onpre1
      - --webhook-cert-dir=/etc/webhook
      - --group-control-image=$CONTROL_IMAGE
    - op: add
      path: /spec/template/spec/nodeSelector
      value:
        kubernetes.io/hostname: member1
    - op: add
      path: /spec/template/spec/tolerations
      value:
      - key: node-role.kubernetes.io/control-plane
        operator: Exists
        effect: NoSchedule
- target:
    kind: DaemonSet
    name: stateful-artifact
  patch: |-
    - op: replace
      path: /spec/template/spec/containers/0/args/1
      value: --cluster-name=onpre1
    - op: add
      path: /spec/template/spec/nodeSelector/migration.dcnlab.com~1artifact-node
      value: "true"
YAML
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" apply -k "$BASE/stateful/config/crd"
kubectl kustomize "$DIR" > "$DIR/rendered.yaml"
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" apply -f "$DIR/rendered.yaml"
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" -n stateful-migration-system \
  wait certificate/stateful-restore-cert --for=condition=Ready --timeout=180s
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" -n stateful-migration-system \
  rollout status deployment/stateful-member --timeout=180s
)
```

실패 시 아래 단계로 넘어가지 않는다. 특히 인증서 CA 주입 완료와 webhook Service
Endpoint가 있어야 한다. GPU 워크로드 배포 전 설치하므로 webhook 기동 중 일시적인
Pod CREATE 차단이 발생할 수 있다.

## PV 멤버

```bash
(
set -euo pipefail
DIR=$(mktemp -d /root/onpre1-pv-member.XXXXXX)
cp -R "$BASE/pv/config/member" "$DIR/base"
cat > "$DIR/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- base
images:
- name: ghcr.io/gprojectdev/pv-migration-system
  newName: docker.io/jeongseungjun/multicluster-pvmigration-system
  newTag: v1.0
patches:
- target:
    kind: Deployment
    name: pv-migration-member
  patch: |-
    - op: replace
      path: /spec/template/spec/containers/0/args/1
      value: --cluster-name=onpre1
    - op: add
      path: /spec/template/spec/nodeSelector
      value:
        kubernetes.io/hostname: member1
    - op: add
      path: /spec/template/spec/tolerations
      value:
      - key: node-role.kubernetes.io/control-plane
        operator: Exists
        effect: NoSchedule
YAML
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" apply -k "$BASE/pv/config/crd"
kubectl kustomize "$DIR" > "$DIR/rendered.yaml"
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" apply -f "$DIR/rendered.yaml"
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" -n pv-migration-system \
  rollout status deployment/pv-migration-member --timeout=180s
)
```

AWS의 PV 멤버도 Running인지 확인한다. AWS에 없으면 위 렌더링에서
`onpre1`은 `aws`, 노드 `member1`은 `ip-10-50-1-53`,
kubeconfig는 `AWS_KUBECONFIG`로 바꾸어 설치한다.

## TrainingRuntime 수집

두 클러스터에서 설치 상태를 먼저 확인한다. 미설치된 클러스터에만 다음을 적용한다.
컨테이너 이미지 태그는 기존 AWS의 실제 실행 이미지와 맞춘다. 해당 이미지가
없다면 System Dockerfile의 COMPONENT=training-runtime-collector로 빌드/푸시한다.

```bash
export RUNTIME_IMAGE=$(kubectl --kubeconfig="${AWS_KUBECONFIG:?}" \
  -n hybridspot-system get deployment training-runtime-collector \
  -o jsonpath='{.spec.template.spec.containers[0].image}')
test -n "$RUNTIME_IMAGE"
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" apply -f "$BASE/System/config/crd/trainingruntimes.yaml"
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" apply -f "$BASE/System/config/member/namespace.yaml"
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" apply -f "$BASE/System/config/member/rbac.yaml"
kubectl set image --local -f "$BASE/System/config/member/runtime.yaml" \
  manager="$RUNTIME_IMAGE" -o yaml |
  kubectl --kubeconfig="$ONPRE1_KUBECONFIG" apply -f -
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" -n hybridspot-system \
  rollout status deployment/training-runtime-collector --timeout=180s
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" apply \
  -f "$BASE/System/config/karmada/runtime-interpreter.yaml"
```

`kubectl set image --local`에는 단일 Deployment만 전달한다. Namespace/RBAC까지
섞인 전체 kustomize 출력에 적용하면 앞서 발생한 오류가 재발한다.

## 기존 onpre1 FluidCR namespace selector 확장

앞서 확인한 selector가 gpu-fluidcr-demo만 선택하는 경우에만 아래를 실행한다.
정확히 그 설정인지 test한 뒤 기존 namespace와 실제 학습 namespace를 함께 허용한다.
다른 selector이면 아래 guard가 실패하므로 임의로 덮어쓰지 않는다.
이미 fluidcr-realign-121040을 포함하면 수정할 필요가 없다.

```bash
(
set -euo pipefail
SELECTOR_BACKUP=$(mktemp /root/gpu-fluidcr-demo/setup/fluidcr-inject.before.XXXXXX.json)
chmod 600 "$SELECTOR_BACKUP"
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" get mutatingwebhookconfiguration fluidcr-inject \
  -o json > "$SELECTOR_BACKUP"
jq -e '.webhooks | length==1 and .[0].name=="inject.fluidcr.dcnlab.com" and
  .[0].namespaceSelector=={matchLabels:{"kubernetes.io/metadata.name":"gpu-fluidcr-demo"}}' \
  "$SELECTOR_BACKUP" >/dev/null
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" patch mutatingwebhookconfiguration fluidcr-inject \
  --type=json -p '[
    {"op":"test","path":"/webhooks/0/name","value":"inject.fluidcr.dcnlab.com"},
    {"op":"test","path":"/webhooks/0/namespaceSelector","value":{"matchLabels":{"kubernetes.io/metadata.name":"gpu-fluidcr-demo"}}},
    {"op":"replace","path":"/webhooks/0/namespaceSelector","value":{"matchExpressions":[
      {"key":"kubernetes.io/metadata.name","operator":"In","values":["gpu-fluidcr-demo","fluidcr-realign-121040"]}
    ]}}
  ]'
)
```

복귀 가이드의 touched-webhooks 목록에도 onpre1/mutating/fluidcr-inject를 기록한다.
AWS는 기존 실험 namespace가 이미 포함돼 있는지 조회하며 불필요하게 수정하지 않는다.


## 설치 결과 점검

```bash
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" -n stateful-migration-system \
  logs deployment/stateful-member --tail=40
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" -n pv-migration-system \
  logs deployment/pv-migration-member --tail=40
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" \
  get mutatingwebhookconfiguration stateful-restore-mutate -o json |
  jq '.webhooks[] | {name,service:.clientConfig.service,caPresent:((.clientConfig.caBundle // "")|length>0)}'
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" \
  get validatingwebhookconfiguration stateful-restore-validate -o json |
  jq '.webhooks[] | {name,caPresent:((.clientConfig.caBundle // "")|length>0)}'
```

컨트롤러 기동/리더 획득만으로 C/R 성공을 선언하지 않는다. 본 가이드의 소규모
checkpoint/restore 리허설까지 성공해야 실제 시연 준비 완료다.
