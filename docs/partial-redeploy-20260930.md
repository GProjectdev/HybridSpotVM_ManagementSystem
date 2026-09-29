# 정리 후 Partial 재배포 가이드

> 후속 수정본은 [Partial 재개 증거 수정본 가이드](partial-resume-release-20260930.md)를 사용한다.
> 아래는 이전 시점의 절차다. 특히 CRD/Stateful 컨트롤러 변경 불필요 안내는 후속 수정본에 해당하지 않는다.

## 현재 상태

Karmada/AWS trainer replicas=0, 학습 Pod 삭제, StatefulSet에 Group 복원 annotation 없음이 확인되었다.
실패한 RestoreRequest/RestorePlan/prepare Job은 정리했다. PVC와 NodeProvision은 보존한다.
순서는 GitHub 동기화 → 새 이미지 빌드/push → 배포 → 기존 파일 보관 → 새 학습 → Partial 검증이다.

모든 명령은 MGMT Bash에서 각각 실행한다. 함수/반복문은 사용하지 않는다.
오류가 나면 다음 명령을 실행하지 않는다. Policy suspend=true, replacement.enabled=false, 위험률=0을 유지한다.
다른 workload가 복원 중이면 공유 컨트롤러 배포를 미룬다. 아직 replicas를 올리지 않는다.

## 1. 코드 동기화

origin과 브랜치가 맞고 로컬 변경이 없는지 먼저 확인한다.
FluidCR 경로가 다르면 아래 fluidcr 경로를 실제 경로로 바꾼다.

```bash
git -C /root/hybridspot-validation/System status --short --branch
git -C /root/hybridspot-validation/System remote -v
git -C /root/hybridspot-validation/stateful status --short --branch
git -C /root/hybridspot-validation/stateful remote -v
git -C /root/hybridspot-validation/fluidcr status --short --branch
git -C /root/hybridspot-validation/fluidcr remote -v
```

세 저장소가 restore-automation-20260928 브랜치일 때 진행한다. 로컬 변경을 reset하지 않는다.

```bash
git -C /root/hybridspot-validation/System fetch origin restore-automation-20260928
git -C /root/hybridspot-validation/System merge --ff-only origin/restore-automation-20260928
git -C /root/hybridspot-validation/stateful fetch origin restore-automation-20260928
git -C /root/hybridspot-validation/stateful merge --ff-only origin/restore-automation-20260928
git -C /root/hybridspot-validation/fluidcr fetch origin restore-automation-20260928
git -C /root/hybridspot-validation/fluidcr merge --ff-only origin/restore-automation-20260928
git -C /root/hybridspot-validation/System merge-base --is-ancestor 1df3636 HEAD && echo SYSTEM_OK
git -C /root/hybridspot-validation/stateful merge-base --is-ancestor d0a7470 HEAD && echo STATEFUL_OK
git -C /root/hybridspot-validation/fluidcr merge-base --is-ancestor b47aeaebfd2c9a82b63758378224a36e86595c22 HEAD && echo FLUID_OK
```

세 OK가 모두 나와야 빌드한다. Not a valid object name 오류는 건너뛰지 않는다.

## 2. 이미지 빌드와 push

같은 셸에서 실행한다. 변수는 경로/태그 저장용이며 함수가 아니다.
Buildah registry 로그인과 디스크 여유가 필요하다. 각 build 성공 후 해당 push를 실행한다.

```bash
export RELEASE=partial-retry-$(date -u +%Y%m%dT%H%M%SZ)
export IMAGES=/root/hybridspot-validation/evidence/$RELEASE/images
mkdir -p "$IMAGES"
buildah bud --arch amd64 --build-arg COMPONENT=policy-manager -t "docker.io/jeongseungjun/hybrid-spot-vm-system:policy-manager-$RELEASE" /root/hybridspot-validation/System
buildah push --digestfile "$IMAGES/policy-manager.digest" "docker.io/jeongseungjun/hybrid-spot-vm-system:policy-manager-$RELEASE" "docker://docker.io/jeongseungjun/hybrid-spot-vm-system:policy-manager-$RELEASE"
buildah bud --arch amd64 --build-arg COMPONENT=checkpoint-coordinator -t "docker.io/jeongseungjun/hybrid-spot-vm-system:checkpoint-coordinator-$RELEASE" /root/hybridspot-validation/System
buildah push --digestfile "$IMAGES/checkpoint-coordinator.digest" "docker.io/jeongseungjun/hybrid-spot-vm-system:checkpoint-coordinator-$RELEASE" "docker://docker.io/jeongseungjun/hybrid-spot-vm-system:checkpoint-coordinator-$RELEASE"
buildah bud --arch amd64 -f /root/hybridspot-validation/fluidcr/Dockerfile.payload -t "docker.io/jeongseungjun/myfluidcr-operator:payload-base-$RELEASE" /root/hybridspot-validation/fluidcr
buildah push --digestfile "$IMAGES/payload-base.digest" "docker.io/jeongseungjun/myfluidcr-operator:payload-base-$RELEASE" "docker://docker.io/jeongseungjun/myfluidcr-operator:payload-base-$RELEASE"
cat "$IMAGES/payload-base.digest"
```

payload-base digest가 sha256:으로 시작하는 정상 값인지 확인한 뒤 overlay를 빌드한다.

```bash
buildah bud --arch amd64 -f /root/hybridspot-validation/stateful/Dockerfile.payload-overlay --build-arg "FLUIDCR_PAYLOAD_IMAGE=docker.io/jeongseungjun/myfluidcr-operator@$(cat "$IMAGES/payload-base.digest")" -t "docker.io/jeongseungjun/myfluidcr-operator:payload-stateful-$RELEASE" /root/hybridspot-validation/stateful
buildah push --digestfile "$IMAGES/payload-stateful.digest" "docker.io/jeongseungjun/myfluidcr-operator:payload-stateful-$RELEASE" "docker://docker.io/jeongseungjun/myfluidcr-operator:payload-stateful-$RELEASE"
cat "$IMAGES/policy-manager.digest" "$IMAGES/checkpoint-coordinator.digest" "$IMAGES/payload-stateful.digest"
echo "$IMAGES"
```

세 digest가 정상이어야 배포한다. 이번 수정만을 위해 CRD/Provisioner/group-control을 재배포할 필요는 없다.

## 3. 이미지 배포

각 rollout 성공 후 다음으로 진행한다.

```bash
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system set image deployment/policy-manager "manager=docker.io/jeongseungjun/hybrid-spot-vm-system@$(cat "$IMAGES/policy-manager.digest")"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system rollout status deployment/policy-manager --timeout=300s
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system set image deployment/checkpoint-coordinator "manager=docker.io/jeongseungjun/hybrid-spot-vm-system@$(cat "$IMAGES/checkpoint-coordinator.digest")"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system rollout status deployment/checkpoint-coordinator --timeout=300s
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n fluidcr-system get deployment fluidcr-webhook -o json | jq '.spec.template.spec.containers[] | {name,args}'
printf 'docker.io/jeongseungjun/myfluidcr-operator@%s\n' "$(cat "$IMAGES/payload-stateful.digest")"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n fluidcr-system edit deployment fluidcr-webhook
```

편집기에서 webhook 컨테이너의 기존 --payload-image 값만 위에 출력된 이미지로 바꾼다.
--payload-image=값이면 해당 항목, 두 항목 형태이면 다음 값만 변경한다.
다른 args와 webhook 자체 이미지는 유지한다. 인자가 없거나 컨테이너 이름이 다르면 임의 추가하지 않는다.
저장/종료한 뒤 다음을 확인한다. namespace 허용 설정에 fluidcr-realign-121040이 포함되어야 한다.

```bash
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n fluidcr-system rollout status deployment/fluidcr-webhook --timeout=300s
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n fluidcr-system get deployment fluidcr-webhook -o json | jq '.spec.template.spec.containers[] | {name,args}'
```

## 4. 재시작 전 확인 지점

아직 학습을 시작하지 않는다. 기존 PVC에는 latest.pt/lock/migration-generation 등 이전 작업 상태가 남아 있다.
학습 Pod가 없는 상태에서 파일을 보관하거나 새 PVC로 작업영역을 분리해야 한다.
이전 replacement NodeProvision도 보존했으므로 배치/용량 조건을 확인한다.

```bash
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" --request-timeout=15s -n fluidcr-realign-121040 get statefulset trainer -o custom-columns='NAME:.metadata.name,DESIRED:.spec.replicas,ACTUAL:.status.replicas'
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" --request-timeout=15s -n fluidcr-realign-121040 get pods,pvc
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" --request-timeout=15s -n fluidcr-realign-121040 get spotreplacements,restorerequests,restoreplans
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" --request-timeout=15s -n fluidcr-realign-121040 get nodeprovisions -o json | jq '[.items[] | {name:.metadata.name,uid:.metadata.uid,annotations:.metadata.annotations,market:.spec.marketType,status:.status}]'
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" --request-timeout=15s -n fluidcr-realign-121040 get trainingpolicy trainer-realign -o json | jq '{annotations:.metadata.annotations,spec:.spec,status:.status}'
```

**여기까지의 결과와 IMAGES 경로를 전달한다.** 여기까지가 현재 상태에서 실행 가능한 재배포 범위다.
파일 보관/노드 재사용 확인 전에는 suspend 해제나 replicas=2를 실행하지 않는다.
이전 replacement를 임의 삭제하거나 capacity_overshoot를 무시하지 않는다.

## 5. 이후 시험 순서

작업영역 분리와 배치 확인 후 다음 순서로 진행한다.
1. 같은 StatefulSet UID를 유지하고 replicas=2로 시작한다.
2. 새 Pod의 initContainer payload digest와 checkpoint PVC를 확인한다.
3. 두 rank step 증가 후 suspend를 해제한다. replacement=false와 위험률=0은 유지한다.
4. 새 체크포인트 Completed 및 재개를 확인한다. 과거 Completed를 성공 증거로 쓰지 않는다.
5. 전체 checkpoint의 ranks/0.json, 1.json을 확인한다. Partial에는 전체 그룹 metadata가 없어도 된다.
6. Spot 대상과 기존 replacement 재사용 조건을 확인한 뒤 replacement=true, 위험률=0.5로 시험한다.
7. 새 SpotReplacement의 partialCheckpoint/partialRestore와 대상 rank를 확인한다.
8. survivor Pod UID/restartCount 보존, 대상 새 UID/노드 이동, 두 rank step 증가와 복원 검증 증거를 확인한다.

Running/Ready만으로 CRIU 검증 완료라고 하지 않는다. 새 group-prepare가 생성되면 Partial 합격이 아니다.
이전 VM 삭제는 복원 증거 검증 후 컨트롤러가 수행한다. status/lock/finalizer를 조작하지 않는다.
로컬 테스트와 AWS GPU 검증은 별개이며 실제 배포/복원은 이 절차를 실행해 확인해야 한다.
