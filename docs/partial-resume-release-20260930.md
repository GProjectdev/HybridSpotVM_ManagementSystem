# Partial 복원 수정본 빌드·재배포 가이드 (2026-09-30)

> `fluidcr-realign-121040`을 계속 사용하는 이번 배포는 [2026-10-01 통합 가이드](checkpoint-schedule-release-20261001.md)를 따른다. 아래의 미적용 변경도 새 가이드에 포함되어 있으므로 두 문서를 중복 실행하지 않는다.

## 범위와 전제

이번 문서는 기존 검증 리소스 정리가 끝난 뒤 적용하는 **업데이트 절차**다. 클러스터 신규 설치 전체를 대체하지 않는다.
MGMT의 Bash에서 코드 블록 안 명령을 한 줄씩 실행한다. 함수·반복문은 사용하지 않는다.
명령이 실패하면 다음 단계로 넘어가지 않는다. Karmada/AWS/MGMT kubeconfig를 혼동하지 않는다.
진행 중인 복원 작업과 학습 Pod가 없는 유지보수 시간에 공유 컨트롤러를 업데이트한다.
기존 checkpoint/PVC를 새 검증에 재사용하지 않고, 새 실험 namespace와 새 PVC를 사용한다.

### 수정 내용

- 체크포인트 신호 전 모든 rank의 Running 상태·GPU worker 등록 준비를 확인한다. 준비 전에는 재시도하며 제한 시간 초과 시 실패한다.
- survivor 재개 승인 기록과 DDP 재결합 완료 기록을 분리한다. 승인된 재결합이 완료된 뒤에만 SurvivorResumed 증거를 기록한다.
- survivor의 실제 로드 checkpointID는 바꾸지 않는다. 이번 Partial checkpointID·generation·Pod UID에 연결된 survivorResume으로 복원 작업 참여를 증명한다.
- 검증기는 대상 Pod의 checkpointID와 survivor 재개 증거, 재개 이후 두 관측의 step 증가를 확인한다.
- runtime HTTP 오류 이유를 노출하고 RestoreReady 이후 최종 검증 대기 사유를 표시한다.
- Policy Manager가 StatefulSet template의 workload UID 라벨을 보완한다. 이때 OnDelete 전략을 설정해 기존 survivor의 자동 롤링을 막는다.
- Provisioner의 NVIDIA linker 설정 및 신규 노드 인증 코드는 이미 구현되어 있다. 최신 Provisioner 배포와 certifyRestore=true 설정이 필요하다. 기존 Ready 노드를 자동 재인증하지는 않는다.

Running만으로 검증 완료라고 표시하거나 finalizer/status/lock을 강제로 변경하지 않는다.
새 증거 계약이므로 과거 실패 operation에 이번 성공 증거를 수동으로 넣지 않는다.

## 1. 저장소 동기화

이 수정본의 연동 기준: FluidCR `df3c73c`, Stateful `de65ab3`,
Provisioner의 기존 linker/인증 수정 `e893e14` 이상이다.
System은 이 문서가 포함된 같은 브랜치의 최신 커밋을 사용한다.

네 저장소는 restore-automation-20260928 브랜치를 사용한다.
status 출력에 로컬 변경이 있으면 덮어쓰지 말고 먼저 내용을 확인한다.

```bash
git -C /root/hybridspot-validation/System status --short --branch
git -C /root/hybridspot-validation/stateful status --short --branch
git -C /root/hybridspot-validation/fluidcr status --short --branch
git -C /root/hybridspot-validation/provisioner status --short --branch
git -C /root/hybridspot-validation/System fetch origin restore-automation-20260928
git -C /root/hybridspot-validation/System merge --ff-only origin/restore-automation-20260928
git -C /root/hybridspot-validation/stateful fetch origin restore-automation-20260928
git -C /root/hybridspot-validation/stateful merge --ff-only origin/restore-automation-20260928
git -C /root/hybridspot-validation/fluidcr fetch origin restore-automation-20260928
git -C /root/hybridspot-validation/fluidcr merge --ff-only origin/restore-automation-20260928
git -C /root/hybridspot-validation/provisioner fetch origin restore-automation-20260928
git -C /root/hybridspot-validation/provisioner merge --ff-only origin/restore-automation-20260928
git -C /root/hybridspot-validation/fluidcr merge-base --is-ancestor df3c73c HEAD
git -C /root/hybridspot-validation/stateful merge-base --is-ancestor de65ab3 HEAD
git -C /root/hybridspot-validation/provisioner merge-base --is-ancestor e893e14 HEAD
```

## 2. 이미지 빌드와 push

registry 로그인, Buildah, jq가 필요하다. RELEASE/IMAGES는 이번 빌드 전용이다.
과거 release 디렉터리의 digest를 가져오지 않는다. 각 push가 만든 파일만 사용한다.

```bash
export RELEASE=partial-resume-$(date -u +%Y%m%dT%H%M%SZ)
export IMAGES=/root/hybridspot-validation/evidence/$RELEASE/images
mkdir -p "$IMAGES"
git -C /root/hybridspot-validation/System rev-parse HEAD > "$IMAGES/system.commit"
git -C /root/hybridspot-validation/stateful rev-parse HEAD > "$IMAGES/stateful.commit"
git -C /root/hybridspot-validation/fluidcr rev-parse HEAD > "$IMAGES/fluidcr.commit"
git -C /root/hybridspot-validation/provisioner rev-parse HEAD > "$IMAGES/provisioner.commit"
```

### Hybrid Spot 이미지

공통 정책 코드가 사용되는 컨트롤러도 함께 맞춘다.
```bash
buildah bud --arch amd64 --build-arg COMPONENT=policy-manager -t "docker.io/jeongseungjun/hybrid-spot-vm-system:policy-manager-$RELEASE" /root/hybridspot-validation/System
buildah push --digestfile "$IMAGES/policy-manager.digest" "docker.io/jeongseungjun/hybrid-spot-vm-system:policy-manager-$RELEASE" "docker://docker.io/jeongseungjun/hybrid-spot-vm-system:policy-manager-$RELEASE"
buildah bud --arch amd64 --build-arg COMPONENT=checkpoint-coordinator -t "docker.io/jeongseungjun/hybrid-spot-vm-system:checkpoint-coordinator-$RELEASE" /root/hybridspot-validation/System
buildah push --digestfile "$IMAGES/checkpoint-coordinator.digest" "docker.io/jeongseungjun/hybrid-spot-vm-system:checkpoint-coordinator-$RELEASE" "docker://docker.io/jeongseungjun/hybrid-spot-vm-system:checkpoint-coordinator-$RELEASE"
buildah bud --arch amd64 --build-arg COMPONENT=spot-recovery-controller -t "docker.io/jeongseungjun/hybrid-spot-vm-system:spot-recovery-controller-$RELEASE" /root/hybridspot-validation/System
buildah push --digestfile "$IMAGES/spot-recovery-controller.digest" "docker.io/jeongseungjun/hybrid-spot-vm-system:spot-recovery-controller-$RELEASE" "docker://docker.io/jeongseungjun/hybrid-spot-vm-system:spot-recovery-controller-$RELEASE"
buildah bud --arch amd64 --build-arg COMPONENT=placement-webhook -t "docker.io/jeongseungjun/hybrid-spot-vm-system:placement-webhook-$RELEASE" /root/hybridspot-validation/System
buildah push --digestfile "$IMAGES/placement-webhook.digest" "docker.io/jeongseungjun/hybrid-spot-vm-system:placement-webhook-$RELEASE" "docker://docker.io/jeongseungjun/hybrid-spot-vm-system:placement-webhook-$RELEASE"
buildah bud --arch amd64 --build-arg COMPONENT=training-runtime-collector -t "docker.io/jeongseungjun/hybrid-spot-vm-system:training-runtime-collector-$RELEASE" /root/hybridspot-validation/System
buildah push --digestfile "$IMAGES/training-runtime-collector.digest" "docker.io/jeongseungjun/hybrid-spot-vm-system:training-runtime-collector-$RELEASE" "docker://docker.io/jeongseungjun/hybrid-spot-vm-system:training-runtime-collector-$RELEASE"
```

### Stateful 컨트롤러와 Provisioner

```bash
buildah bud --arch amd64 -t "docker.io/jeongseungjun/stateful-migration-operator:$RELEASE" /root/hybridspot-validation/stateful
buildah push --digestfile "$IMAGES/stateful.digest" "docker.io/jeongseungjun/stateful-migration-operator:$RELEASE" "docker://docker.io/jeongseungjun/stateful-migration-operator:$RELEASE"
buildah bud --arch amd64 -t "docker.io/jeongseungjun/my-publiccloudvm-provisioner:$RELEASE" /root/hybridspot-validation/provisioner
buildah push --digestfile "$IMAGES/provisioner.digest" "docker.io/jeongseungjun/my-publiccloudvm-provisioner:$RELEASE" "docker://docker.io/jeongseungjun/my-publiccloudvm-provisioner:$RELEASE"
```

### 학습 Pod에 주입할 payload

base를 먼저 push한 다음 그 digest로 overlay를 빌드한다. group-control은 이번 Partial 수정의 변경 대상이 아니다.
```bash
buildah bud --arch amd64 -f /root/hybridspot-validation/fluidcr/Dockerfile.payload -t "docker.io/jeongseungjun/myfluidcr-operator:payload-base-$RELEASE" /root/hybridspot-validation/fluidcr
buildah push --digestfile "$IMAGES/payload-base.digest" "docker.io/jeongseungjun/myfluidcr-operator:payload-base-$RELEASE" "docker://docker.io/jeongseungjun/myfluidcr-operator:payload-base-$RELEASE"
cat "$IMAGES/payload-base.digest"
buildah bud --arch amd64 -f /root/hybridspot-validation/stateful/Dockerfile.payload-overlay --build-arg "FLUIDCR_PAYLOAD_IMAGE=docker.io/jeongseungjun/myfluidcr-operator@$(cat "$IMAGES/payload-base.digest")" -t "docker.io/jeongseungjun/myfluidcr-operator:payload-stateful-$RELEASE" /root/hybridspot-validation/stateful
buildah push --digestfile "$IMAGES/payload-stateful.digest" "docker.io/jeongseungjun/myfluidcr-operator:payload-stateful-$RELEASE" "docker://docker.io/jeongseungjun/myfluidcr-operator:payload-stateful-$RELEASE"
cat "$IMAGES/payload-stateful.digest"
printf 'IMAGES=%s\n' "$IMAGES"
```

모든 digest는 sha256:으로 시작해야 한다. 파일 없음 오류가 나면 해당 build/push부터 완료한다.

## 3. 기존 배포 백업과 CRD 업데이트

아래 백업에는 배포 인자·이전 이미지가 들어간다. Secret 내용은 저장하지 않는다.
이번에는 TrainingRuntime CRD 변경이 있으므로 **양쪽 API에 먼저 적용**해야 한다.

```bash
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system get deployments -o yaml > "$IMAGES/mgmt-hybrid-before.yaml"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n stateful-migration-system get deployment stateful-management -o yaml > "$IMAGES/mgmt-stateful-before.yaml"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n stateful-migration-system get deployments,daemonsets -o yaml > "$IMAGES/aws-stateful-before.yaml"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n hybridspot-system get deployment training-runtime-collector -o yaml > "$IMAGES/aws-runtime-before.yaml"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n remote-cluster-provisioner-system get deployment remote-cluster-provisioner -o yaml > "$IMAGES/aws-provisioner-before.yaml"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n fluidcr-system get deployment fluidcr-webhook -o json > "$IMAGES/webhook-before.json"
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" apply -f /root/hybridspot-validation/System/config/crd/trainingruntimes.yaml
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" apply -f /root/hybridspot-validation/System/config/crd/trainingruntimes.yaml
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" apply -f /root/hybridspot-validation/System/config/karmada/runtime-interpreter.yaml
```

## 4. 컨트롤러 교체

각 rollout 성공 후 다음 명령을 실행한다. replicas=0이면 rollout 성공만으로 프로세스 기동을 확인할 수 없다.
이름/컨테이너가 아래와 다르면 get deployment로 실제 이름을 확인하고 맞춘다.

```bash
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system set image deployment/policy-manager "manager=docker.io/jeongseungjun/hybrid-spot-vm-system@$(cat "$IMAGES/policy-manager.digest")"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system rollout status deployment/policy-manager --timeout=300s
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system set image deployment/checkpoint-coordinator "manager=docker.io/jeongseungjun/hybrid-spot-vm-system@$(cat "$IMAGES/checkpoint-coordinator.digest")"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system rollout status deployment/checkpoint-coordinator --timeout=300s
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system set image deployment/spot-recovery-controller "manager=docker.io/jeongseungjun/hybrid-spot-vm-system@$(cat "$IMAGES/spot-recovery-controller.digest")"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system rollout status deployment/spot-recovery-controller --timeout=300s
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system set image deployment/placement-webhook "manager=docker.io/jeongseungjun/hybrid-spot-vm-system@$(cat "$IMAGES/placement-webhook.digest")"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system rollout status deployment/placement-webhook --timeout=300s
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n hybridspot-system set image deployment/training-runtime-collector "manager=docker.io/jeongseungjun/hybrid-spot-vm-system@$(cat "$IMAGES/training-runtime-collector.digest")"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n hybridspot-system rollout status deployment/training-runtime-collector --timeout=300s
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n stateful-migration-system set image deployment/stateful-management "manager=docker.io/jeongseungjun/stateful-migration-operator@$(cat "$IMAGES/stateful.digest")"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n stateful-migration-system rollout status deployment/stateful-management --timeout=600s
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n stateful-migration-system set image deployment/stateful-checkpoint "manager=docker.io/jeongseungjun/stateful-migration-operator@$(cat "$IMAGES/stateful.digest")"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n stateful-migration-system rollout status deployment/stateful-checkpoint --timeout=600s
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n stateful-migration-system set image deployment/stateful-member "manager=docker.io/jeongseungjun/stateful-migration-operator@$(cat "$IMAGES/stateful.digest")"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n stateful-migration-system rollout status deployment/stateful-member --timeout=600s
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n stateful-migration-system set image daemonset/stateful-artifact "verifier=docker.io/jeongseungjun/stateful-migration-operator@$(cat "$IMAGES/stateful.digest")"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n stateful-migration-system rollout status daemonset/stateful-artifact --timeout=600s
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n remote-cluster-provisioner-system set image deployment/remote-cluster-provisioner "manager=docker.io/jeongseungjun/my-publiccloudvm-provisioner@$(cat "$IMAGES/provisioner.digest")"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n remote-cluster-provisioner-system rollout status deployment/remote-cluster-provisioner --timeout=600s
```

## 5. webhook의 payload 이미지 지정

webhook 자체 이미지와 다른 인자는 보존한다. 다음 명령은 기존 --payload-image의 두 형식 모두 지원한다.
대상 인자가 정확히 하나가 아니거나 resourceVersion이 변경되면 적용하지 않는다.
함수 호출이나 대화형 편집 없이 변경 내용부터 출력한 뒤 patch한다.

```bash
export PAYLOAD_IMAGE="docker.io/jeongseungjun/myfluidcr-operator@$(cat "$IMAGES/payload-stateful.digest")"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n fluidcr-system get deployment fluidcr-webhook -o json > "$IMAGES/webhook-current.json"
jq -e --arg image "$PAYLOAD_IMAGE" '
  . as $d |
  [.spec.template.spec.containers | to_entries[] | select(.value.name=="webhook")] as $cs |
  (if ($cs|length)!=1 then error("webhook container must be unique") else $cs[0] end) as $c |
  ($c.value.args // []) as $a |
  [$a | to_entries[] | select(.value=="--payload-image" or (.value|startswith("--payload-image=")))] as $flags |
  (if ($flags|length)!=1 then error("payload flag must be unique") else $flags[0] end) as $f |
  (if $f.value=="--payload-image" then
     if ($a|length)<=$f.key+1 or ($a[$f.key+1]|startswith("--")) then error("payload value missing")
     else {index:($f.key+1),value:$image} end
   else {index:$f.key,value:("--payload-image="+$image)} end) as $v |
  [
    {op:"test",path:"/metadata/resourceVersion",value:$d.metadata.resourceVersion},
    {op:"replace",path:("/spec/template/spec/containers/"+($c.key|tostring)+"/args/"+($v.index|tostring)),value:$v.value}
  ]
' "$IMAGES/webhook-current.json" > "$IMAGES/webhook-payload.patch.json"
cat "$IMAGES/webhook-payload.patch.json"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n fluidcr-system patch deployment fluidcr-webhook --type=json --patch-file "$IMAGES/webhook-payload.patch.json"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n fluidcr-system rollout status deployment/fluidcr-webhook --timeout=300s
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n fluidcr-system get deployment fluidcr-webhook -o json | jq '.spec.template.spec.containers[] | {name,args}'
```

학습 namespace가 webhook 허용 범위에 들어가는지 기존 namespace selector/인자 설정도 확인한다.
기존 학습 Pod에는 새 코드가 주입되지 않는다. **새 Pod의 initContainer image가 이번 payload digest인지 반드시 확인**한다.

## 6. 신규 노드 설정

실제 패키지 URL·SHA256·CRIO/CRIU commit·adapterSHA256이 들어간 NodeProvisionNetConfig를
새 실험 namespace에서 먼저 준비한다. 샘플의 __REPLACE_*를 그대로 apply하지 않는다.
TrainingPolicy의 region/AZ/VPC/subnet/SG/credentialsRef 및 member cluster와 맞아야 한다.
기존 검증된 rendered JSON을 복사해 namespace와 실제 환경값을 검토하되, 삭제된 Secret 이름을 재사용하지 않는다.

신규 설정에서 다음 항목을 사용한다. 이 조각만으로 새 CR 전체를 만들 수는 없다.

```yaml
spec:
  softwareConfig:
    nodeSoftware:
      runtimeProfile: StatefulMigration
      nfsClient: true
      migrationRuntime:
        certifyRestore: true
```

최신 Provisioner가 새 노드 준비 과정에서 driver library 로딩과 runtime qualification을 통과해야
restore-from-file 능력 라벨을 부여한다. certifyRestore=false이면 자동 인증을 기대하면 안 된다.
이미 Ready인 노드는 이 설정만 바꿔도 재인증되는 구조가 아니다. 이번 검증은 신규 노드로 진행한다.
인증 실패 시 NodeProvision message와 Provisioner 로그를 확인한다. 라벨만 강제 true로 바꾸지 않는다.

## 7. 새 실험 시작 순서

1. 새 namespace, AWS 자격증명 Secret, 실제 NodeProvisionNetConfig 및 필요한 전파 정책을 준비한다.
2. 새 공유 PVC·rank별 PVC, trainer Service·ConfigMap, FluidCR 주입 namespace 설정을 준비한다.
3. Karmada에 trainer StatefulSet 원본을 replicas=0으로 생성해 UID를 확보한다. 이전 UID를 복사하지 않는다.
4. 그 UID로 사용자가 TrainingPolicy를 작성한다. SpotRiskProfile도 먼저 생성한다.
5. 처음에는 replacement.enabled=false, 낮은 정적 위험률(검증용 0.05), suspend=true로 설정한다.
6. TrainingPolicy의 실제 네트워크·Secret·worldSize=2를 확인한다. replicas=0 상태에서 template UID 라벨 자동 보완을 확인한 뒤 replicas=2로 변경하고 suspend를 해제한다.
7. NodeProvision Ready, 두 trainer Running 및 step 증가, 새 payload digest·template UID·OnDelete를 확인한다.
8. 새 정기 체크포인트 Completed와 두 rank 재개를 확인한다. 이후 replacement=true 및 위험률 0.5로 Partial을 유발한다.

아래 변수는 **실제로 새로 만든** namespace와 Policy/Risk 이름으로 설정한다. 예시 이름을 과거 실험 이름으로 착각하지 않는다.
기존 설치용 manifest 작성은 [기본 검증 가이드](release-validation-20260929.md)를 참고하되,
이 문서의 이미지/CRD/순서를 우선한다. 자동 생성된 TrainingRuntime/SpotReplacement는 손으로 만들지 않는다.

```bash
export NS=fluidcr-partial-new
export POLICY=trainer-partial
export RISK=trainer-risk
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get statefulset trainer -o jsonpath='{.metadata.uid}{"\n"}'
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get statefulset trainer -o json | jq '{uid:.metadata.uid,templateUID:.spec.template.metadata.labels["training.dcnlab.com/workload-uid"],strategy:.spec.updateStrategy}'
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" scale statefulset trainer --replicas=2
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" annotate trainingpolicy "$POLICY" training.dcnlab.com/suspend-
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get nodeprovisions,pods,pvc
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get pod trainer-0 trainer-1 -o json | jq '[.items[] | {name:.metadata.name,uid:.metadata.uid,node:.spec.nodeName,payload:.spec.initContainers,restarts:.status.containerStatuses}]'
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get statefulset trainer -o json | jq '{labels:.spec.template.metadata.labels,strategy:.spec.updateStrategy}'
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get fluidcrmigrations
```

새 정기 체크포인트 완료와 학습 재개까지 확인한 후에만 다음을 실행한다.
```bash
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" patch trainingpolicy "$POLICY" --type=merge -p '{"spec":{"replacement":{"enabled":true}}}'
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" patch spotriskprofile "$RISK" --type=merge -p '{"spec":{"staticLambdaPerHour":0.5}}'
```

## 8. 최종 검증과 캡처

조회는 반복 실행한다. kubectl의 다중 리소스 get에 --watch를 붙이지 않는다.
```bash
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get spotreplacements,restorerequests,spotrecoveries -o custom-columns='KIND:.kind,NAME:.metadata.name,PHASE:.status.phase,MESSAGE:.status.message'
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get restorerequests -o json | jq '[.items[] | {name:.metadata.name,status:.status}]'
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get trainingruntimes -o yaml
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get restoreplans -o yaml
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get pod trainer-0 trainer-1 -o wide
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" logs trainer-0 -c trainer --timestamps --since=2m --tail=15
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" logs trainer-1 -c trainer --timestamps --since=2m --tail=15
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" exec trainer-0 -c trainer -- python3 -c 'import urllib.request; print(urllib.request.urlopen("http://127.0.0.1:8298/runtime",timeout=5).read().decode())'
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get nodeprovisions -o wide
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" get nodes
```

합격 조건:
- Partial target rank만 이동하며 survivor Pod UID/restartCount가 유지된다. group-prepare로 우회하지 않는다.
- 양쪽 rank의 step이 증가하고 survivor /runtime이 HTTP 200이다.
- survivorResume의 checkpointID/generation/UID가 이번 Partial 작업과 맞는다. survivor의 로드 ID는 예전 정기 체크포인트여도 된다.
- RestoreRequest status.verification이 Verified이고, SpotReplacement가 그 증거를 받아 정리 단계로 진행한다.
- 기존 Spot NodeProvision의 정상 삭제 및 EC2 인스턴스 종료를 확인한다. Kubernetes Node 목록만으로 EC2 종료를 단정하지 않는다.

RestoreReady는 대상 복원 준비/재개 승인 단계이지 최종 Verified가 아니다.
VM이 남으면 먼저 RestoreRequest의 구체적인 검증 대기 사유를 확인한다.

## 9. 실패·되돌리기

실패 상태의 YAML과 컨트롤러 로그를 먼저 보관한다. 삭제/성공 status patch로 검증을 통과시키지 않는다.
진행 중인 Partial을 이미지 혼용 상태로 바꾸지 않는다. 작업을 안전하게 종료하고 학습 Pod가 없는 상태에서
3절 백업의 이전 image/args를 사용해 set image 또는 payload 인자를 되돌린다.
확장된 CRD를 급히 축소하지 않는다. 새 증거를 만든 checkpoint/Pod는 구버전 검증에 재사용하지 않는다.

로컬 Go 테스트·정적 검사 및 Python 계약 테스트는 GPU/CRIU 복원과 다르다.
실제 이미지 빌드, 신규 노드 인증, AWS GPU Partial 복원, 기존 VM 자동 종료는 위 절차로 별도 검증해야 한다.

### 로컬 검증 기록

- System/Stateful: 전체 Go 테스트, go vet, go build 통과.
- Stateful payload: Python 계약 테스트 40개 통과, base/overlay 파일 해시 일치 확인.
- FluidCR base: Python runtime 계약 테스트 54개 통과.
- Provisioner: pkg/nodesoftware 테스트 통과.
- 가이드 Bash 코드 블록: bash -n 검사 통과. 실제 kubectl/buildah 실행 검증은 하지 않았다.
- 로컬에는 PyTorch/GPU가 없어 PyTorch 의존 전체 테스트와 실제 CRIU/NCCL 복원은 검증하지 못했다.
