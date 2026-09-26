# 기존 MGMT/Karmada + AWS Member에서 시작하는 설치·검증 가이드

이 문서는 **MGMT와 Karmada는 설치됨, aws Member에는 이전 NodeProvisioner만 설치됨, 생성된 worker는 0개**인 상태에서 시작합니다. 명령은 **Linux 관리 터미널의 Bash**에서 순서대로 실행합니다. Windows PowerShell용 명령이 아닙니다.

**검증 범위:** 저장소 코드·manifest 계약을 기준으로 작성했습니다. 실제 EC2 생성, 이미지 pull, GPU/DDP checkpoint/restore 및 Spot interruption E2E가 완료되었다는 뜻은 아닙니다. 각 단계의 통과 기준을 실제 환경에서 확인해야 합니다. 빈 값이나 교체 표시가 있으면 다음 단계로 넘어가지 마세요.

**읽는 순서:** 1~5 설치 → 6 VM 생성 → 7 학습 → 8 주기 checkpoint → 9 계획 이전 준비 → 10 복원 → 11 실제 Spot 시험 → 12 종료. 전체 코드 블록을 한 번에 실행하지 말고 각 통과 기준에서 멈춰 확인합니다. 9~10단계는 학습 중단, 11단계는 실제 VM 삭제를 포함합니다.

## 0. 최종 목표와 중단 지점

| 단계 | 결과 | 비용/위험 |
|---|---|---|
| 1~5 | 코드·이미지·CRD·컨트롤러·NetConfig 준비 | EC2 생성하지 않음 |
| 6 | Policy가 AWS worker 2개 생성 | **여기부터 EC2/GPU/EBS 비용 발생** |
| 7 | GPU/NFS/runtime 및 2-rank 학습 확인 | 실제 학습 시작 |
| 8 | 시간 기반 checkpoint와 durable export 확인 | 저장 공간 사용 |
| 9~10 | source 차단 → PV 준비 → 사용자 placement 변경 → restore 검증 | 학습 중단을 수반하는 계획 이전 |
| 11 | 실제 Spot 신호와 검증된 restore를 연결한 cleanup | 지정한 이전 Spot VM 삭제 |
| 12 | 증거 보관 및 종료 | 잘못 삭제하면 복구 불가 |

AWS만 준비되어 있다면 8단계까지만 검증할 수 있습니다. 9단계 이후에는 별도 Member **onprem**과 호환되는 GPU worker 2개가 필요합니다. MGMT 호스트는 자동으로 onprem workload cluster가 되지 않습니다. 대상 이름이 다르면 TARGET_CLUSTER를 바꾸되 이후 상태 비교도 같은 이름을 사용합니다.

- 사용자가 workload placement와 실제 source fencing을 수행합니다.
- CheckpointCoordinator가 초 단위 주기로 FluidCRMigration을 생성합니다. FluidCR/member checkpoint controller가 실제 rank checkpoint를 수행합니다.
- member artifact controller가 archive를 durable store로 내보내고 대상에 내려받습니다. 수동 scp를 정상 경로로 사용하지 않습니다.
- Restore 컴포넌트가 Prepared와 Verified를 판단합니다. PV Completed나 Pod Ready는 복원 성공이 아닙니다.
- SpotWatcher가 Member NodeProvision.status.spot을 기록하고 RIC가 Karmada로 반영합니다.
- Policy Manager는 검증된 SpotRecovery로 특정 old NodeProvision을 삭제합니다. placement를 자동 변경하지 않습니다.

## 1. 관리 터미널과 입력값 준비

필요 도구: git, kubectl, Docker/build registry 권한, jq, envsubst, Python 3 + PyYAML, Helm(GPU addon용). Ubuntu 관리 터미널에서 Python 가상환경을 사용합니다.

~~~bash
sudo apt-get update
sudo apt-get install -y git jq gettext-base python3-venv
mkdir -p "$HOME/hybridspot-validation"
cd "$HOME/hybridspot-validation"
export ROOT="$PWD"
python3 -m venv .venv
source .venv/bin/activate
pip install PyYAML==6.0.2

export MGMT_KUBECONFIG=/secure/mgmt-host.kubeconfig
export KARMADA_KUBECONFIG=/secure/karmada-admin.kubeconfig
export AWS_KUBECONFIG=/secure/aws-member.kubeconfig
export ONPREM_KUBECONFIG=/secure/onprem-member.kubeconfig
export SOURCE_CLUSTER=aws TARGET_CLUSTER=onprem AWS_CLUSTER=aws
export NS=fluidcr-demo APP=trainer
export POLICY_NAME=trainer-policy RUNTIME_NAME=trainer-runtime
export SOURCE_KUBECONFIG="$AWS_KUBECONFIG"
export TARGET_KUBECONFIG="$ONPREM_KUBECONFIG"
umask 077
set -eo pipefail

git clone --depth 1 --single-branch --branch main https://github.com/GProjectdev/HybridSpotVM_ManagementSystem.git System
git clone --depth 1 --single-branch --branch In-aws-create-WorkerNode https://github.com/GProjectdev/PublicCloud-VM-Provisioner_test.git provisioner
git clone --depth 1 --single-branch --branch main https://github.com/GProjectdev/Stateful-Migration-Operator-with-PV.git stateful
git clone --depth 1 --single-branch --branch main https://github.com/GProjectdev/Karmada_with_PVMigration.git pv
export SYSTEM="$ROOT/System" PROVISIONER="$ROOT/provisioner" ST="$ROOT/stateful" PV="$ROOT/pv"
mkdir -p "$ROOT/evidence" "$ROOT/rendered"
for repo in System provisioner stateful pv; do
  git -C "$ROOT/$repo" rev-parse HEAD > "$ROOT/evidence/$repo.commit"
done
~~~

이미 clone한 폴더가 있다면 실제 경로를 변수에 지정합니다. 기존 변경을 reset하지 않습니다. pv는 main만 shallow clone하므로 보관용 Old_and_have_karmada 브랜치를 받지 않습니다.

~~~bash
kubectl --kubeconfig="$MGMT_KUBECONFIG" get nodes -o wide
kubectl --kubeconfig="$KARMADA_KUBECONFIG" get clusters
kubectl --kubeconfig="$AWS_KUBECONFIG" get nodes -o wide
kubectl --kubeconfig="$AWS_KUBECONFIG" get nodeprovisions -A
kubectl --kubeconfig="$AWS_KUBECONFIG" get nodeprovisionnetconfigs -A
kubectl --kubeconfig="$AWS_KUBECONFIG" get deployments -A
kubectl --kubeconfig="$KARMADA_KUBECONFIG" get propagationpolicies -A
kubectl --kubeconfig="$KARMADA_KUBECONFIG" get clusterpropagationpolicies
~~~

**통과:** aws Member Ready, AWS control-plane Ready, 기존 worker NodeProvision이 없음. 광범위한 placement 정책이 새 trainer/NodeProvision을 자동 선택하지 않는지 확인합니다. 기존 NodeProvision이 있다면 신규 2개 생성 경로를 중단하고 소유권부터 확인합니다.

| 입력 | 조건 |
|---|---|
| SPOT_RISK_COLLECTOR_IMAGE, POLICY_MANAGER_IMAGE, CHECKPOINT_COORDINATOR_IMAGE, SPOT_RECOVERY_IMAGE, RUNTIME_COLLECTOR_IMAGE, SPOT_WATCHER_IMAGE, PROVISIONER_IMAGE, PV_IMAGE, STATEFUL_IMAGE | 직접 빌드하여 pull 가능한 고유 태그/가능하면 digest |
| FLUIDCR_ROOT | 현재 FluidCR 소스 폴더를 Linux 관리 터미널에 복사한 절대 경로 |
| INJECTOR_IMAGE, PAYLOAD_BASE_IMAGE, PAYLOAD_IMAGE | webhook, 원본 payload, Stateful overlay payload의 서로 다른 태그 |
| TRAINER_IMAGE | /workspace/train.py가 있는 실제 2-rank DDP + FluidCR hook 이미지 |
| GPU_RUNTIME_CLASS | 두 Member에 실제 존재하는 RuntimeClass |
| NFS_SERVER, NFS_EXPORT_0, NFS_EXPORT_1, ARTIFACT_EXPORT | 두 Member가 읽고 쓸 수 있는 NFS server/export |
| KUBELET_CA_FILE | AWS kubelet serving 인증서를 검증하는 CA 파일 |
| AWS 설정 | region/AZ/type/AMI/VPC/subnet/SG/instance profile, GPU quota, egress |
| runtime package | 검토한 CRI-O/CRIU/adapter/helper/CUDA plugin .deb URL와 digest |

아래 빈 값을 실제 환경값으로 채웁니다. secret은 Git이나 evidence에 저장하지 않습니다.

~~~bash
export SPOT_RISK_COLLECTOR_IMAGE='' POLICY_MANAGER_IMAGE='' CHECKPOINT_COORDINATOR_IMAGE=''
export SPOT_RECOVERY_IMAGE='' RUNTIME_COLLECTOR_IMAGE='' SPOT_WATCHER_IMAGE=''
export PROVISIONER_IMAGE='' PV_IMAGE='' STATEFUL_IMAGE=''
export FLUIDCR_ROOT='' INJECTOR_IMAGE='' PAYLOAD_BASE_IMAGE='' PAYLOAD_IMAGE=''
export TRAINER_IMAGE='' GPU_RUNTIME_CLASS=''
export NFS_SERVER='' NFS_EXPORT_0='' NFS_EXPORT_1='' ARTIFACT_EXPORT=''
export KUBELET_CA_FILE=''
~~~

## 2. 런타임 패키지와 이미지 준비

먼저 [Stateful runtime 설치 가이드](https://github.com/GProjectdev/Stateful-Migration-Operator-with-PV/blob/main/docs/runtime-installation.md)의 source build·annotation adapter **patch와 helper 모두**·CUDA checkpoint smoke test를 수행합니다. fork를 설치하기만 하면 annotation restore가 된다고 가정하지 않습니다.

그 결과물로 [NodeProvisioner node-software 가이드](https://github.com/GProjectdev/PublicCloud-VM-Provisioner_test/blob/In-aws-create-WorkerNode/docs/node-software-guide.md)의 .deb builder를 실행하고 신뢰할 수 있는 HTTPS 위치에 호스팅합니다. digest와 source commit을 NetConfig에 고정합니다. 새 VM에서 source를 다시 빌드하지 않습니다. 패키지 Ubuntu/CPU architecture, CRI-O minor와 AWS control-plane Kubernetes minor가 맞아야 합니다. target onprem에도 호환 runtime이 별도 설치되어야 합니다.

~~~bash
# 하나라도 비어 있으면 빌드 전에 중단합니다.
for value in "$SPOT_RISK_COLLECTOR_IMAGE" "$POLICY_MANAGER_IMAGE" "$CHECKPOINT_COORDINATOR_IMAGE" \
  "$SPOT_RECOVERY_IMAGE" "$RUNTIME_COLLECTOR_IMAGE" "$SPOT_WATCHER_IMAGE" \
  "$PROVISIONER_IMAGE" "$PV_IMAGE" "$STATEFUL_IMAGE" \
  "$FLUIDCR_ROOT" "$INJECTOR_IMAGE" "$PAYLOAD_BASE_IMAGE" "$PAYLOAD_IMAGE"; do
  test -n "$value" || { echo 'Fill all image/source inputs first'; exit 1; }
done
docker build --build-arg COMPONENT=vm-spot-risk-collector -t "$SPOT_RISK_COLLECTOR_IMAGE" "$SYSTEM"
docker build --build-arg COMPONENT=policy-manager -t "$POLICY_MANAGER_IMAGE" "$SYSTEM"
docker build --build-arg COMPONENT=checkpoint-coordinator -t "$CHECKPOINT_COORDINATOR_IMAGE" "$SYSTEM"
docker build --build-arg COMPONENT=spot-recovery-controller -t "$SPOT_RECOVERY_IMAGE" "$SYSTEM"
docker build --build-arg COMPONENT=training-runtime-collector -t "$RUNTIME_COLLECTOR_IMAGE" "$SYSTEM"
docker build --build-arg COMPONENT=spot-watcher -t "$SPOT_WATCHER_IMAGE" "$SYSTEM"
docker push "$SPOT_RISK_COLLECTOR_IMAGE"
docker push "$POLICY_MANAGER_IMAGE"
docker push "$CHECKPOINT_COORDINATOR_IMAGE"
docker push "$SPOT_RECOVERY_IMAGE"
docker push "$RUNTIME_COLLECTOR_IMAGE"
docker push "$SPOT_WATCHER_IMAGE"
make -C "$PROVISIONER" docker-build docker-push IMG="$PROVISIONER_IMAGE"
make -C "$PV" docker-build docker-push IMG="$PV_IMAGE"
make -C "$ST" docker-build docker-push IMG="$STATEFUL_IMAGE"
docker build -f "$FLUIDCR_ROOT/Dockerfile.payload" -t "$PAYLOAD_BASE_IMAGE" "$FLUIDCR_ROOT"
docker push "$PAYLOAD_BASE_IMAGE"
docker build -f "$ST/Dockerfile.payload-overlay" \
  --build-arg FLUIDCR_PAYLOAD_IMAGE="$PAYLOAD_BASE_IMAGE" -t "$PAYLOAD_IMAGE" "$ST"
docker push "$PAYLOAD_IMAGE"
~~~

FluidCR injector는 FLUIDCR_ROOT의 webhook Dockerfile/빌드 지침으로 INJECTOR_IMAGE를 build/push합니다. 해당 버전의 Dockerfile 이름을 확인하고 사용하세요. trainer는 임의의 pytorch 이미지로 대체하지 않습니다. 기존 Stateful 2-Pod 예제는 FLUIDCR_DISTRIBUTED=0인 독립 작업 2개이며, 원본 FluidCR DDP 예제는 3개의 별도 StatefulSet입니다. **둘 다 검증된 2-replica DDP 이미지가 아닙니다.**

2-rank 이미지의 통과 조건: rank 0/1, WORLD_SIZE=2, safe optimizer-step hook, 실제 torch.distributed 통신, /runtime의 최신 globalStep/checkpointID, 동일 checkpoint round 저장 및 재결합. 이 입력이 없다면 인프라 검증과 DDP 검증을 구분하고 DDP 성공으로 기록하지 마세요.

Private registry이면 컨트롤러 namespace와 fluidcr-demo에 pull Secret을 준비하여 rendered Deployment/DaemonSet 및 trainer template에 imagePullSecrets를 넣습니다. payload init image도 pull 가능해야 합니다.

Member 보안 정책이 hostNetwork(SpotWatcher), hostPath/kubelet archive 접근(artifact), GPU/runtime 권한을 허용하는지도 확인합니다. 정책에 막히면 해당 namespace/ServiceAccount에 필요한 범위만 승인하고 cluster 전체 Pod 보안 정책을 해제하지 않습니다.

## 3. 기존 NodeProvisioner 업데이트와 NetConfig 준비

아직 worker CR을 생성하지 않습니다. 백업 후 새 CRD/controller를 설치합니다. 아래 이름은 기존 deploy/ 방식입니다. 1단계 조회 결과가 다르면 **실제로 실행 중인** Deployment/container 이름으로 바꾸고 중복 controller를 띄우지 않습니다.

~~~bash
kubectl --kubeconfig="$AWS_KUBECONFIG" get nodeprovisionnetconfigs -A -o json |
  jq '.items |= map(del(.status))' > "$ROOT/evidence/netconfig-before.json"
kubectl --kubeconfig="$AWS_KUBECONFIG" apply -k "$PROVISIONER/config/crd"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -k "$PROVISIONER/config/crd"
kubectl --kubeconfig="$AWS_KUBECONFIG" wait --for=condition=Established \
  crd/nodeprovisionnetconfigs.ml.dcn.ssu.ac.kr --timeout=120s
kubectl --kubeconfig="$AWS_KUBECONFIG" -n remote-cluster-provisioner-system \
  set image deployment/remote-cluster-provisioner manager="$PROVISIONER_IMAGE"
kubectl --kubeconfig="$AWS_KUBECONFIG" -n remote-cluster-provisioner-system \
  rollout status deployment/remote-cluster-provisioner --timeout=180s
for kc in "$KARMADA_KUBECONFIG" "$AWS_KUBECONFIG"; do
  kubectl --kubeconfig="$kc" create namespace "$NS" --dry-run=client -o yaml |
    kubectl --kubeconfig="$kc" apply -f -
done
cp "$PROVISIONER/config/samples/aws-vpc-netconfig-stateful.yaml" "$ROOT/rendered/netconfig.yaml"
~~~

netconfig.yaml의 metadata.namespace를 fluidcr-demo로, name을 그 namespace의 유일한 NetConfig 이름으로 수정합니다. 기존 객체가 있으면 같은 이름으로 갱신하고 **두 번째 NetConfig를 만들지 않습니다**.

- softwareConfig.kubernetesVersion: AWS control-plane의 실제 버전.
- nodeSoftware.runtimeProfile: StatefulMigration.
- nodeSoftware.nfsClient: true.
- nodeSoftware.gpuMode: **DevicePlugin**을 첫 기준 실험으로 사용. DRA는 7단계 별도 경로.
- migrationRuntime: 준비한 packageURL/packageSHA256/crioCommit/criuCommit/adapterSHA256.
- spec.clusterName: 실제 AWS cluster 설정값.

AWS credential Secret은 **AWS Member의 fluidcr-demo에만** 준비합니다. 키는 awsAccessKeyId, awsSecretAccessKey, 선택 awsSessionToken입니다. 기존 default namespace Secret을 자동 참조하지 않습니다.

~~~bash
# 보안 경로에 줄바꿈 없는 실제 값 파일을 먼저 준비합니다.
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" create secret generic aws-node-credentials \
  --from-file=awsAccessKeyId=/secure/aws-access-key-id \
  --from-file=awsSecretAccessKey=/secure/aws-secret-access-key \
  --dry-run=client -o yaml | kubectl --kubeconfig="$AWS_KUBECONFIG" apply -f -
kubectl --kubeconfig="$AWS_KUBECONFIG" apply --dry-run=server -f "$ROOT/rendered/netconfig.yaml"
kubectl --kubeconfig="$AWS_KUBECONFIG" apply -f "$ROOT/rendered/netconfig.yaml"
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get nodeprovisionnetconfigs -o json |
  jq '.items |= map(del(.status))'
~~~

STS 임시 credentials에는 awsSessionToken도 포함합니다. SDK credential chain 경로는 빈 Secret 참조와 Provisioner Pod IAM 권한을 별도로 검증합니다. IAM/SG/CNI/master 접근 조건은 [AWS VPC guide](https://github.com/GProjectdev/PublicCloud-VM-Provisioner_test/blob/In-aws-create-WorkerNode/docs/aws-vpc-worker-guide.md)를 따릅니다. 이 통합 경로에서는 그 문서의 worker sample을 apply하지 않습니다. Policy가 생성할 2개와 중복됩니다.

현재 TrainingPolicy는 associatePublicIP를 노출하지 않으며 Provisioner 생략 기본값은 public IP 연결입니다. private-subnet 정책상 이를 금지해야 하면 여기서 중단하고 capacity 계약을 확장해야 합니다. NAT가 있다는 이유로 false가 설정된 것으로 간주하지 않습니다.

## 4. CRD·RIC와 MGMT System 설치

아직 VM을 만들지 않습니다. NodeProvision status는 이 저장소의 **독립 RIC**를 사용합니다. 구 Provisioner 묶음의 end--- 문서 구분 문제를 피하고, namespace 전체 NP를 선택하는 PP도 추가하지 않습니다. Policy가 각 NP용 PP를 생성합니다.

~~~bash
for kc in "$KARMADA_KUBECONFIG" "$AWS_KUBECONFIG"; do
  kubectl --kubeconfig="$kc" apply -k "$SYSTEM/config/crd"
  kubectl --kubeconfig="$kc" apply -k "$PV/config/crd"
  kubectl --kubeconfig="$kc" apply -k "$ST/config/crd"
done
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -k "$SYSTEM/config/karmada"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f "$SYSTEM/config/runbook/nodeprovision-status.yaml"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -k "$PV/config/karmada/rbac"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -k "$PV/config/karmada/ric"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -k "$ST/config/karmada"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" get resourceinterpretercustomizations
~~~

이미지와 Member 이름을 **apply 전에** 렌더링합니다. renderer는 `SYSTEM_IMAGE`가 아니라 `SPOT_RISK_COLLECTOR_IMAGE`, `POLICY_MANAGER_IMAGE`, `CHECKPOINT_COORDINATOR_IMAGE`, `SPOT_RECOVERY_IMAGE`, `RUNTIME_COLLECTOR_IMAGE`, `SPOT_WATCHER_IMAGE`를 요구합니다. helper는 로컬 YAML→JSON 변환만 수행합니다. 선택적 control-plane toleration은 worker가 없는 환경에서 **controller Deployment만** control-plane에 배치 가능하게 합니다. taint를 제거하거나 trainer에 toleration을 넣지 않습니다. 여유 자원이 없으면 별도 management worker가 필요합니다.

~~~bash
render_bundle() {
  kubectl kustomize "$1" |
    python3 "$SYSTEM/scripts/render-validation-manifests.py" \
      --cluster "$2" --control-plane-tolerations > "$3"
}
export OUTPUT_KUBECONFIG="$ROOT/hybridspot-controller.kubeconfig"
bash "$SYSTEM/scripts/create-karmada-kubeconfig.sh"
kubectl --kubeconfig="$MGMT_KUBECONFIG" apply -f "$SYSTEM/config/management/namespace.yaml"
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system create secret generic hybridspot-karmada-kubeconfig \
  --from-file=kubeconfig="$OUTPUT_KUBECONFIG" --dry-run=client -o yaml |
  kubectl --kubeconfig="$MGMT_KUBECONFIG" apply -f -
LEGACY_DEPLOYMENT="$(kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system get deployment/hybridspot-management --ignore-not-found -o json)"
if test -n "$LEGACY_DEPLOYMENT"; then
  LEGACY_SELECTOR="$(printf '%s' "$LEGACY_DEPLOYMENT" | jq -r '.spec.selector.matchLabels | to_entries | map("\(.key)=\(.value)") | join(",")')"
  kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system scale deployment/hybridspot-management --replicas=0
  kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system wait \
    --for=delete pod -l "$LEGACY_SELECTOR" --timeout=180s
fi
render_bundle "$SYSTEM/config/management" aws "$ROOT/rendered/system-management.json"
kubectl --kubeconfig="$MGMT_KUBECONFIG" apply -f "$ROOT/rendered/system-management.json"
render_bundle "$SYSTEM/config/member" aws "$ROOT/rendered/system-aws.json"
RUNTIME_DEPLOYMENT="$(kubectl --kubeconfig="$AWS_KUBECONFIG" -n hybridspot-system get deployment/training-runtime-collector --ignore-not-found -o json)"
if test -n "$RUNTIME_DEPLOYMENT"; then
  RUNTIME_SELECTOR="$(printf '%s' "$RUNTIME_DEPLOYMENT" | jq -r '.spec.selector.matchLabels | to_entries | map("\(.key)=\(.value)") | join(",")')"
  kubectl --kubeconfig="$AWS_KUBECONFIG" -n hybridspot-system scale deployment/training-runtime-collector --replicas=0
  kubectl --kubeconfig="$AWS_KUBECONFIG" -n hybridspot-system wait \
    --for=delete pod -l "$RUNTIME_SELECTOR" --timeout=180s
fi
kubectl --kubeconfig="$AWS_KUBECONFIG" apply -f "$ROOT/rendered/system-aws.json"
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system rollout status deployment/vm-spot-risk-collector --timeout=180s
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system rollout status deployment/policy-manager --timeout=180s
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system rollout status deployment/checkpoint-coordinator --timeout=180s
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system rollout status deployment/spot-recovery-controller --timeout=180s
kubectl --kubeconfig="$AWS_KUBECONFIG" -n hybridspot-system rollout status deployment/training-runtime-collector --timeout=180s
kubectl --kubeconfig="$AWS_KUBECONFIG" -n hybridspot-system get daemonset spot-watcher
~~~

**통과:** 4개 MGMT Deployment와 runtime Deployment Ready. worker 0개일 때 SpotWatcher desired=0은 정상입니다. MGMT split Deployment는 기존 `hybridspot-karmada-kubeconfig` Secret과 기존 shared Karmada RBAC identity를 사용합니다. 이는 보안 격리가 아니며, 각 Deployment의 leader-election ID만 독립적이어야 합니다. runtime collector leader-election ID는 `hybridspot-runtime`에서 `hybridspot-training-runtime-collector`로 바뀌므로 upgrade에서는 AWS `training-runtime-collector`도 scale 0 후 Pod 종료를 기다려야 합니다. `spot-watcher` DaemonSet 이름은 그대로이므로 rolling update가 가능합니다. Karmada endpoint는 MGMT Pod에서 접근 가능해야 하며 localhost 주소이면 안 됩니다. token은 요청 24시간보다 짧게 발급될 수도 있습니다. 만료 전 kubeconfig Secret 갱신 후 Deployment를 restart합니다. member admin kubeconfig를 MGMT Pod에 넣지 않습니다.

## 5. PV·Stateful·FluidCR 컴포넌트 설치

NFS server/export는 별도로 준비합니다. 이 시스템은 NFS server를 생성하지 않습니다. application checkpoint export와 ARTIFACT_EXPORT를 분리하고 동일 archive export를 source/target에서 사용합니다. DDP는 rank 간 round 파일을 읽어야 하므로 이 예제의 NFS_EXPORT_0와 NFS_EXPORT_1은 **같은 공유 export root**입니다. Pod별 경로는 /checkpoint/trainer-0, /checkpoint/trainer-1입니다. NFS directory/UID/GID/ACL/root-squash 정책과 네트워크를 확인합니다. 접근 오류를 chmod 777로 우회하지 않습니다.

### 5.1 Member 준비

아래에서 MEMBER_KC/MEMBER_NAME을 aws로 설정하여 수행합니다. onprem이 준비되면 값을 TARGET_KUBECONFIG/TARGET_CLUSTER로 바꾸고 5.1~5.2를 반복한 후 9단계로 갑니다. 대상 Member 등록 자체는 기존 Karmada 운영 절차로 수행하고 Ready를 확인합니다.

~~~bash
export MEMBER_KC="$AWS_KUBECONFIG" MEMBER_NAME="$SOURCE_CLUSTER"
kubectl --kubeconfig="$MEMBER_KC" create namespace "$NS" --dry-run=client -o yaml |
  kubectl --kubeconfig="$MEMBER_KC" apply -f -
kubectl --kubeconfig="$MEMBER_KC" apply -k "$SYSTEM/config/crd"
kubectl --kubeconfig="$MEMBER_KC" apply -k "$PV/config/crd"
kubectl --kubeconfig="$MEMBER_KC" apply -k "$ST/config/crd"
kubectl --kubeconfig="$MEMBER_KC" apply -f "$ST/config/member/namespace.yaml"
envsubst '$NFS_SERVER $ARTIFACT_EXPORT' < "$SYSTEM/config/runbook/artifact-store.yaml" |
  kubectl --kubeconfig="$MEMBER_KC" apply -f -
kubectl --kubeconfig="$MEMBER_KC" -n stateful-migration-system get pvc stateful-migration-artifacts
~~~

**통과:** PVC Bound. 실제 NFS 쓰기 검증은 7단계에서도 수행합니다. NFS client는 artifact를 실행할 모든 노드에 필요합니다. renderer는 artifact DaemonSet을 migration.dcnlab.com/artifact-node=true 노드로 제한하여 준비되지 않은 control-plane까지 mount하지 않게 합니다.

FluidCR/Restore webhook용 cert-manager가 Member에 필요합니다. 이미 있으면 재설치/덮어쓰지 말고 Ready 상태만 확인합니다. 신규 설치는 [공식 설치·호환성 문서](https://cert-manager.io/docs/installation/kubectl/)에서 Kubernetes와 맞는 release를 선택합니다.

~~~bash
export CM_VERSION=''  # 호환성을 확인한 고정 vX.Y.Z
# cert-manager가 없는 Member에서만 실행
test -n "$CM_VERSION" || { echo 'Select cert-manager version first'; exit 1; }
kubectl --kubeconfig="$MEMBER_KC" apply \
  -f "https://github.com/cert-manager/cert-manager/releases/download/$CM_VERSION/cert-manager.yaml"
kubectl --kubeconfig="$MEMBER_KC" -n cert-manager get pods -o wide
~~~

worker 없는 AWS에서 taint 때문에 Pending이면 아래처럼 **각 controller Deployment만** 허용합니다. taint 자체를 제거하지 않습니다.

~~~bash
# Pending 원인이 해당 taint일 때에만 patch 실행
for dep in cert-manager cert-manager-webhook cert-manager-cainjector; do
  kubectl --kubeconfig="$MEMBER_KC" -n cert-manager patch deployment "$dep" --type=strategic \
    -p '{"spec":{"template":{"spec":{"tolerations":[{"key":"node-role.kubernetes.io/control-plane","operator":"Exists","effect":"NoSchedule"},{"key":"node-role.kubernetes.io/master","operator":"Exists","effect":"NoSchedule"}]}}}}'
  kubectl --kubeconfig="$MEMBER_KC" -n cert-manager rollout status deployment/"$dep" --timeout=180s
done
~~~

### 5.2 Member controllers와 injector

~~~bash
docker build -f "$FLUIDCR_ROOT/Dockerfile.webhook" -t "$INJECTOR_IMAGE" "$FLUIDCR_ROOT"
docker push "$INJECTOR_IMAGE"
render_bundle "$PV/config/member" "$MEMBER_NAME" "$ROOT/rendered/pv-$MEMBER_NAME.json"
render_bundle "$ST/config/member" "$MEMBER_NAME" "$ROOT/rendered/stateful-$MEMBER_NAME.json"
kubectl --kubeconfig="$MEMBER_KC" apply -f "$ROOT/rendered/pv-$MEMBER_NAME.json"
kubectl --kubeconfig="$MEMBER_KC" apply -f "$ROOT/rendered/stateful-$MEMBER_NAME.json"
kubectl --kubeconfig="$MEMBER_KC" -n stateful-migration-system wait \
  --for=condition=Ready certificate/stateful-restore-cert --timeout=180s
kubectl --kubeconfig="$MEMBER_KC" -n stateful-migration-system rollout status deployment/stateful-member --timeout=180s
kubectl --kubeconfig="$MEMBER_KC" -n pv-migration-system rollout status deployment/pv-migration-member --timeout=180s

# namespace/RBAC/runtime를 각각 적용: target에는 AWS SpotWatcher를 설치하지 않음
kubectl --kubeconfig="$MEMBER_KC" apply -f "$SYSTEM/config/member/namespace.yaml" -f "$SYSTEM/config/member/rbac.yaml"
RUNTIME_DEPLOYMENT="$(kubectl --kubeconfig="$MEMBER_KC" -n hybridspot-system get deployment/training-runtime-collector --ignore-not-found -o json)"
if test -n "$RUNTIME_DEPLOYMENT"; then
  RUNTIME_SELECTOR="$(printf '%s' "$RUNTIME_DEPLOYMENT" | jq -r '.spec.selector.matchLabels | to_entries | map("\(.key)=\(.value)") | join(",")')"
  kubectl --kubeconfig="$MEMBER_KC" -n hybridspot-system scale deployment/training-runtime-collector --replicas=0
  kubectl --kubeconfig="$MEMBER_KC" -n hybridspot-system wait \
    --for=delete pod -l "$RUNTIME_SELECTOR" --timeout=180s
fi
python3 "$SYSTEM/scripts/render-validation-manifests.py" --control-plane-tolerations \
  < "$SYSTEM/config/member/runtime.yaml" > "$ROOT/rendered/runtime-$MEMBER_NAME.json"
kubectl --kubeconfig="$MEMBER_KC" apply -f "$ROOT/rendered/runtime-$MEMBER_NAME.json"

for file in namespace rbac cert-manager service; do
  kubectl --kubeconfig="$MEMBER_KC" apply -f "$FLUIDCR_ROOT/deploy/webhook/$file.yaml"
done
python3 "$SYSTEM/scripts/render-validation-manifests.py" --control-plane-tolerations \
  < "$FLUIDCR_ROOT/deploy/webhook/deployment.yaml" > "$ROOT/rendered/injector-$MEMBER_NAME.json"
kubectl --kubeconfig="$MEMBER_KC" apply -f "$ROOT/rendered/injector-$MEMBER_NAME.json"
kubectl --kubeconfig="$MEMBER_KC" -n fluidcr-system wait \
  --for=condition=Ready certificate/fluidcr-webhook-cert --timeout=180s
kubectl --kubeconfig="$MEMBER_KC" -n fluidcr-system rollout status deployment/fluidcr-webhook --timeout=180s
kubectl --kubeconfig="$MEMBER_KC" apply -f "$FLUIDCR_ROOT/deploy/webhook/mutatingwebhookconfiguration.yaml"
~~~

artifact DaemonSet desired=0은 준비된 worker가 없는 단계에서 정상입니다. legacy FluidCR checkpoint operator/annotation-only suspension controller가 있으면 **해당 Deployment를 식별하여 중단**하고 중복 제어를 피합니다.

AWS checkpoint controller에는 kubelet serving CA가 필요합니다. API server CA와 동일하다고 가정하지 않습니다. kubelet 인증서 IP SAN, Checkpoint API, 접근/RBAC를 검증하고 insecure TLS 우회를 사용하지 않습니다. 다음은 **source AWS에만** 적용합니다.

~~~bash
test -s "$KUBELET_CA_FILE" || { echo 'Provide trusted kubelet serving CA'; exit 1; }
kubectl --kubeconfig="$AWS_KUBECONFIG" -n stateful-migration-system \
  create configmap kubelet-serving-ca --from-file=ca.crt="$KUBELET_CA_FILE" --dry-run=client -o yaml |
  kubectl --kubeconfig="$AWS_KUBECONFIG" apply -f -
render_bundle "$ST/config/checkpoint" aws "$ROOT/rendered/checkpoint-aws.json"
kubectl --kubeconfig="$AWS_KUBECONFIG" apply -f "$ROOT/rendered/checkpoint-aws.json"
kubectl --kubeconfig="$AWS_KUBECONFIG" -n stateful-migration-system \
  rollout status deployment/stateful-checkpoint --timeout=180s
~~~

### 5.3 MGMT PV/Stateful controllers

TrainingRuntime CRD를 Karmada에 설치한 다음 수행합니다. credential script는 **KUBECONFIG와 context**를 사용하고 기존 출력 파일을 덮어쓰지 않습니다.

~~~bash
KARMADA_CONTEXT="$(kubectl --kubeconfig="$KARMADA_KUBECONFIG" config current-context)"
KUBECONFIG="$KARMADA_KUBECONFIG" bash "$ST/scripts/create-karmada-kubeconfig.sh" \
  "$KARMADA_CONTEXT" "$ROOT/pv-controller.kubeconfig" pv-migration-system pv-migration-management
KUBECONFIG="$KARMADA_KUBECONFIG" bash "$ST/scripts/create-karmada-kubeconfig.sh" \
  "$KARMADA_CONTEXT" "$ROOT/stateful-controller.kubeconfig" stateful-migration-system stateful-management
kubectl --kubeconfig="$MGMT_KUBECONFIG" apply -f "$PV/config/management/namespace.yaml"
kubectl --kubeconfig="$MGMT_KUBECONFIG" apply -f "$ST/config/management/namespace.yaml"
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n pv-migration-system create secret generic karmada-kubeconfig \
  --from-file=kubeconfig="$ROOT/pv-controller.kubeconfig" --dry-run=client -o yaml |
  kubectl --kubeconfig="$MGMT_KUBECONFIG" apply -f -
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n stateful-migration-system create secret generic stateful-karmada-kubeconfig \
  --from-file=kubeconfig="$ROOT/stateful-controller.kubeconfig" --dry-run=client -o yaml |
  kubectl --kubeconfig="$MGMT_KUBECONFIG" apply -f -
render_bundle "$PV/config/management" aws "$ROOT/rendered/pv-management.json"
render_bundle "$ST/config/management" aws "$ROOT/rendered/stateful-management.json"
kubectl --kubeconfig="$MGMT_KUBECONFIG" apply -f "$ROOT/rendered/pv-management.json"
kubectl --kubeconfig="$MGMT_KUBECONFIG" apply -f "$ROOT/rendered/stateful-management.json"
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n pv-migration-system rollout status deployment/pv-migration-management --timeout=180s
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n stateful-migration-system rollout status deployment/stateful-management --timeout=180s
~~~

**주의:** 이 두 credential은 기본 1시간 token입니다. 긴 실험 전에 새 출력 파일로 발급, 동일 Secret update, 해당 Deployment restart를 수행합니다. expiry를 401 오류가 난 뒤 발견하지 않도록 관리합니다.

## 6. 최초 worker 2개 생성: 여기부터 비용 발생

### 6.1 아직 전파하지 않는 StatefulSet 원본

Policy는 실제 Karmada StatefulSet UID가 필요합니다. worker가 없어도 **Karmada에만** 객체를 먼저 만듭니다. 다음은 기존 예제 기반의 2-rank scaffold입니다. 학습 프로그램을 생성하거나 DDP 복원을 검증하는 코드가 아닙니다. 준비된 실제 TRAINER_IMAGE가 필수입니다.

Python은 PP를 제거하고 Service/StatefulSet만 저장합니다. rank는 [StatefulSet pod-index label](https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/#pod-index-label)을 Downward API로 전달합니다. launcher와 control process 모두 RANK를 볼 수 있어야 합니다.

~~~bash
test -n "$TRAINER_IMAGE" && test -n "$GPU_RUNTIME_CLASS" || { echo 'Fill workload inputs'; exit 1; }
python3 - <<'PY'
import json, os, pathlib, yaml
docs = list(yaml.safe_load_all((pathlib.Path(os.environ["ST"]) /
    "config/samples/two-replica/workload.yaml").read_text()))
items = [d for d in docs if d["kind"] in ("Service", "StatefulSet")]
sts = next(d for d in items if d["kind"] == "StatefulSet")
pod = sts["spec"]["template"]["spec"]
pod["runtimeClassName"] = os.environ["GPU_RUNTIME_CLASS"]
pod["affinity"] = {"podAntiAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": [{
    "labelSelector": {"matchLabels": {"app": "trainer"}},
    "topologyKey": "kubernetes.io/hostname"}]}}
c = pod["containers"][0]
c["image"] = os.environ["TRAINER_IMAGE"]
for e in c["env"]:
    if e["name"] == "FLUIDCR_DISTRIBUTED":
        e["value"] = "1"
c["env"] += [
    {"name": "RANK", "valueFrom": {"fieldRef": {
        "fieldPath": "metadata.labels['apps.kubernetes.io/pod-index']"}}},
    {"name": "WORLD_SIZE", "value": "2"},
    {"name": "LOCAL_RANK", "value": "0"},
    {"name": "MASTER_ADDR", "value": "trainer-0.trainer.fluidcr-demo.svc.cluster.local"},
    {"name": "MASTER_PORT", "value": "29500"}]
out = pathlib.Path(os.environ["ROOT"]) / "rendered/workload-unplaced.json"
out.write_text(json.dumps({"apiVersion": "v1", "kind": "List", "items": items}, indent=2))
PY
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f "$ROOT/rendered/workload-unplaced.json"
export WORKLOAD_UID="$(kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get sts "$APP" -o jsonpath='{.metadata.uid}')"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" label sts "$APP" \
  "training.dcnlab.com/workload-uid=$WORKLOAD_UID" --overwrite
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get sts
~~~

**통과:** AWS에 trainer가 아직 없어야 합니다. 보이면 기존 broad PP가 선택한 것이므로 충돌부터 해결합니다. UID label은 StatefulSet metadata에 넣습니다. Pod template label로 대체하지 않습니다.

### 6.2 Risk/Runtime 준비 후 Policy 적용

~~~bash
for file in 10-training-runtime 11-spot-risk-profile-static 21-training-runtime-propagationpolicy; do
  envsubst < "$SYSTEM/config/samples/$file.yaml" |
    kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f -
done
envsubst < "$SYSTEM/config/samples/12-training-policy.yaml" > "$ROOT/rendered/training-policy.yaml"
~~~

training-policy.yaml의 모든 __REPLACE... 값을 실제 AWS 값으로 바꿉니다. credential name=aws-node-credentials, namespace=fluidcr-demo, hardwareType/nodeLabel=gpu, source/karmadaCluster=aws입니다. targetWorkers=2, expectedWorldSize=2, minOnDemand=1을 유지합니다. staticLambdaPerHour=0.05는 **시험 입력**이며 AWS 실측 선점률이 아닙니다. 실제 feed는 [위험률 계약](risk-feed.md)과 `config/samples/11-spot-risk-profile-https.yaml`의 `aws-risk-feed`로 연결합니다. endpoint `https://risk-feed.example.invalid/aws/ap-northeast-2/g4dn.xlarge`를 실제 HTTPS feed로 바꾸고 TrainingPolicy risk reference가 `aws-risk-feed`를 가리키게 수정하세요. static sample과 HTTPS sample을 함께 apply해도 자동 전환되지 않습니다.

~~~bash
if grep -n '__REPLACE' "$ROOT/rendered/training-policy.yaml"; then
  echo 'Unfilled AWS inputs: stop'; exit 1
fi
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply --dry-run=server -f "$ROOT/rendered/training-policy.yaml"
# 모든 준비와 dry-run 성공 후 실행: 실제 VM 생성 시작
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f "$ROOT/rendered/training-policy.yaml"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingpolicy "$POLICY_NAME" -o yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get nodeprovisions -o wide
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get nodeprovisions -o wide
kubectl --kubeconfig="$AWS_KUBECONFIG" get nodes -o wide
~~~

**통과:** desiredWorkers=2, onDemandWorkers=1, spotWorkers=1. Karmada/AWS 각각 같은 이름의 NP 2개(UID는 다를 수 있음), EC2 2개, 두 worker Ready. status.observedCluster=aws와 instanceId가 Karmada에 반영되어야 합니다. 15~20분 동안 진전이 없으면 events/Provisioner logs/cloud-init을 확인하고 **추가 NP를 만들지 않습니다**.

측정값이 없을 때 costEvaluated=false/intervalCostEvaluated=false는 정상입니다. 위험률 변경 시 기존 VM 시장유형은 즉시 바뀌지 않고 replacement_required가 될 수 있습니다. 이를 자동 fallback 성공으로 기록하지 않습니다.

## 7. GPU·스토리지 준비 후 학습 전파

### 7.1 GPU와 artifact 노드

~~~bash
AWS_CONTEXT="$(kubectl --kubeconfig="$AWS_KUBECONFIG" config current-context)"
bash "$PROVISIONER/scripts/install-gpu-addons.sh" --kubeconfig "$AWS_KUBECONFIG" \
  --context "$AWS_CONTEXT" --mode device-plugin --driver-owner gpu-operator
# render 결과와 기존 driver/Operator 소유권을 확인한 뒤 실제 설치
bash "$PROVISIONER/scripts/install-gpu-addons.sh" --kubeconfig "$AWS_KUBECONFIG" \
  --context "$AWS_CONTEXT" --mode device-plugin --driver-owner gpu-operator --apply
kubectl --kubeconfig="$AWS_KUBECONFIG" get nodes \
  -o custom-columns='NAME:.metadata.name,GPU:.status.allocatable.nvidia\.com/gpu'
kubectl --kubeconfig="$AWS_KUBECONFIG" get runtimeclass
~~~

이미 driver를 이미지에 설치했다면 driver-owner=preinstalled 경로를 사용합니다. 기존 GPU Operator/ClusterPolicy를 installer가 거부하면 덮어쓰지 않습니다. 두 node에서 실제 CUDA smoke test와 runtime archive restore 검증을 수행합니다. Node Ready/GPU allocatable만으로 checkpoint 가능 판정을 내리지 않습니다.

**DRA 선택 경로:** VM 생성 전 NetConfig.gpuMode=DRA, Kubernetes>=1.34.2 및 [GPU addon 가이드](https://github.com/GProjectdev/PublicCloud-VM-Provisioner_test/blob/In-aws-create-WorkerNode/docs/gpu-addons.md)의 driver/toolkit/CDI 조건을 충족해야 합니다. install mode=dra와 대상 GPU node label이 필요합니다. 이 경우 6단계의 nvidia.com/gpu scaffold 대신 검증된 ResourceClaimTemplate/resourceClaims workload를 사용합니다. DRA와 DevicePlugin을 같은 할당 방식으로 혼용하지 않습니다. 본 문서는 DevicePlugin을 기준으로 하며 DRA E2E 성공을 주장하지 않습니다.

~~~bash
# get nodes에서 방금 생성한 서로 다른 worker 이름을 지정
export SOURCE_NODE_0='' SOURCE_NODE_1=''
test -n "$SOURCE_NODE_0" && test -n "$SOURCE_NODE_1" && \
  test "$SOURCE_NODE_0" != "$SOURCE_NODE_1" || { echo 'Select two workers'; exit 1; }
kubectl --kubeconfig="$AWS_KUBECONFIG" label node "$SOURCE_NODE_0" "$SOURCE_NODE_1" \
  migration.dcnlab.com/artifact-node=true --overwrite
kubectl --kubeconfig="$AWS_KUBECONFIG" -n stateful-migration-system \
  rollout status daemonset/stateful-artifact --timeout=180s
kubectl --kubeconfig="$AWS_KUBECONFIG" -n hybridspot-system \
  rollout status daemonset/spot-watcher --timeout=180s
~~~

각 worker에서 mount.nfs, CRI-O/CRIU 경로·버전, CUDA plugin 및 공유 export mount/read/write를 확인합니다. target도 같은 검증이 필요합니다. artifact DS Ready 외에 export된 digest 파일을 양쪽에서 읽을 수 있는지 8~9단계에서 확인합니다.

### 7.2 source PV와 placement

~~~bash
test -n "$NFS_SERVER" && test -n "$NFS_EXPORT_0" && \
  test "$NFS_EXPORT_0" = "$NFS_EXPORT_1" || { echo 'Use the shared DDP export root'; exit 1; }
envsubst '$NFS_SERVER $NFS_EXPORT_0 $NFS_EXPORT_1' \
  < "$ST/config/samples/two-replica/source-pvs.yaml" |
  kubectl --kubeconfig="$AWS_KUBECONFIG" apply -f -
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" apply -f - <<EOF
apiVersion: policy.karmada.io/v1alpha1
kind: PropagationPolicy
metadata:
  name: trainer-service
spec:
  resourceSelectors:
  - apiVersion: v1
    kind: Service
    name: trainer
  placement:
    clusterAffinity:
      clusterNames: ["$SOURCE_CLUSTER"]
EOF
envsubst < "$SYSTEM/config/samples/20-workload-propagationpolicy.yaml" |
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f -
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get pods,pvc -o wide
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get sts "$APP" -o yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingruntime "$RUNTIME_NAME" -o yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get pvmetadata -o yaml
~~~

**통과:** trainer-0/1이 서로 다른 GPU worker에서 실행, PVC 2개 Bound, FluidCR injection 완료, rank=0/1, worldSize=2, 같은 checkpointID, globalStep 증가. /runtime timestamp는 30초 이내, TrainingRuntime.status.clusters의 aws readyRanks=2. PVMetadata에는 두 ordinal PVC/PV가 있어야 합니다. MGMT StatefulSet UID와 Member StatefulSet UID는 다릅니다.

Pod Ready뿐 아니라 trainer 로그의 실제 DDP 초기화·통신과 학습 결과를 확인합니다. NetworkPolicy는 collector/checkpoint controller→PodIP:8298 및 rank 간 rendezvous/학습 통신을 허용하되 control port를 외부에 노출하지 않습니다. injector가 failurePolicy=Ignore일 수 있으므로 webhook 설치만으로 주입 성공을 간주하지 말고 Pod의 주입 annotation, init container, 변경된 entrypoint를 확인합니다.

## 8. 시간 기반 checkpoint와 archive 검증

~~~bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingpolicy "$POLICY_NAME" -o yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get fluidcrmigrations --sort-by=.metadata.creationTimestamp
kubectl --kubeconfig="$AWS_KUBECONFIG" -n stateful-migration-system \
  logs deployment/stateful-checkpoint --tail=100
~~~

기본 시험 입력 0.05에서는 300초 위험 구간 정책을 기대합니다. 주기는 초 단위이며 safe optimizer step/archive 작업 때문에 완료 간격은 더 길 수 있습니다. inflight_checkpoint 중 다음 round를 중복 생성하지 않아야 합니다.

완료된 실제 CR 이름을 선택합니다. status.checkpoint.lastMigrationName은 대기 중 비어 있을 수 있으므로 목록과 source status로 선택합니다.

~~~bash
export LAST_MIGRATION=''  # 실제 완료한 FluidCRMigration 이름
test -n "$LAST_MIGRATION" || { echo 'Select a completed checkpoint'; exit 1; }
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get fluidcrmigration "$LAST_MIGRATION" -o json \
  > "$ROOT/evidence/checkpoint.json"
jq '.metadata | {name,uid,generation,annotations}' "$ROOT/evidence/checkpoint.json"
jq '.status.clusters' "$ROOT/evidence/checkpoint.json"
~~~

**통과:** source report observedGeneration이 CR generation과 같고 phase=Completed. trainer-0/1의 archive마다 sha256(64 hex), file-store:로 시작하는 durableRef, exportedAt이 있어야 합니다. Pod/node/container/경로가 정확하고 두 rank가 같은 checkpointID/round/step이어야 합니다. checkpointID는 CR annotation training.dcnlab.com/checkpoint-id에서 읽습니다. stale generation이나 Completed만으로 진행하지 않습니다.

시간 주기 변경 검증은 계획 이전 전에 수행합니다. 다음은 candidate를 120초로 고정합니다.

~~~bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" patch trainingpolicy "$POLICY_NAME" --type=merge \
  -p '{"spec":{"checkpoint":{"minIntervalSeconds":120,"maxIntervalSeconds":120,"candidateIntervalSeconds":[120],"riskBands":[{"maxLambdaPerHour":1,"intervalSeconds":120}]}}}'
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingpolicy "$POLICY_NAME" -o yaml
~~~

새 generation의 interval=120, 중복 없는 새 CR 생성 시각, 두 rank archive 내보내기를 2회 이상 기록합니다. 이후 복원할 **최신 완료 round를 다시 선택**합니다. T_ckpt/copy duration을 측정하지 않았다면 임의로 0을 넣지 말고 초기 위험 구간 방식으로 둡니다.

## 9. 계획 이전 리허설: source 차단과 PV 준비

Spot 장애를 먼저 일으키지 않습니다. 이 단계는 **의도된 중단을 수반하는 계획 이전**이며 target onprem이 준비되지 않았다면 여기서 멈춥니다.

### 9.1 target 필수 준비

5.1~5.2를 MEMBER_KC="$TARGET_KUBECONFIG", MEMBER_NAME="$TARGET_CLUSTER"로 완료합니다. 호환 GPU/runtime 2개, 같은 NFS/ARTIFACT_EXPORT, injector/restore/artifact/runtime/PV controllers, cert-manager가 있어야 합니다. target 노드 RuntimeClass, 메모리/GPU/아키텍처/driver 호환성을 검증합니다. 검증 전 restore-from-file label을 붙이지 않습니다.

~~~bash
export TARGET_NODE_0='' TARGET_NODE_1=''
test -n "$TARGET_NODE_0" && test -n "$TARGET_NODE_1" && \
  test "$TARGET_NODE_0" != "$TARGET_NODE_1" || { echo 'Select two tested target workers'; exit 1; }
kubectl --kubeconfig="$KARMADA_KUBECONFIG" get cluster "$TARGET_CLUSTER" -o yaml
kubectl --kubeconfig="$TARGET_KUBECONFIG" get nodes "$TARGET_NODE_0" "$TARGET_NODE_1" -o wide
# runtime restore smoke test가 통과한 노드에만 허용
kubectl --kubeconfig="$TARGET_KUBECONFIG" label node "$TARGET_NODE_0" "$TARGET_NODE_1" \
  migration.dcnlab.com/artifact-node=true migration.dcnlab.com/restore-from-file=true --overwrite
kubectl --kubeconfig="$TARGET_KUBECONFIG" -n stateful-migration-system \
  rollout status daemonset/stateful-artifact --timeout=180s
# telemetry와 Service만 양쪽으로 전파. workload placement는 여전히 aws만.
for pp in "$RUNTIME_NAME-source-only" trainer-service; do
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" patch propagationpolicy "$pp" --type=merge \
    -p "$(jq -nc --arg s "$SOURCE_CLUSTER" --arg t "$TARGET_CLUSTER" \
      '{spec:{placement:{clusterAffinity:{clusterNames:[$s,$t]}}}}')"
done
~~~

### 9.2 새 round 생성을 멈추고 dispatch pause

초기 단일 실험 환경에서는 split System MGMT controller를 잠시 scale 0하여 새 checkpoint와 capacity/recovery 조정을 함께 멈춥니다. 이는 per-job pause가 아닙니다. 다른 운영 job이 있다면 이 방법을 사용하지 말고 별도 유지보수 창을 잡습니다. **PV/Stateful management와 Member controller는 계속 실행**합니다.

~~~bash
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system scale deployment/vm-spot-risk-collector --replicas=0
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system scale deployment/policy-manager --replicas=0
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system scale deployment/checkpoint-coordinator --replicas=0
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system scale deployment/spot-recovery-controller --replicas=0
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system get pods
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get fluidcrmigrations -o yaml
~~~

실행 중인 checkpoint가 있다면 완료/export될 때까지 기다립니다. 실패 round를 완료로 수정하지 않습니다. controller Pod가 완전히 종료되어 새 round가 만들어지지 않는지 확인합니다.

~~~bash
export RB_NAME="$(kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get resourcebindings -o json |
  jq -er --arg app "$APP" '[.items[] | select(.spec.resource.kind=="StatefulSet" and .spec.resource.name==$app)] |
    if length==1 then .[0].metadata.name else error("expected one workload RB") end')"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" patch resourcebinding "$RB_NAME" --type=merge \
  -p '{"spec":{"suspension":{"dispatching":true}}}'
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get resourcebinding "$RB_NAME" -o yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "karmada-es-$SOURCE_CLUSTER" get works -o yaml
~~~

**통과:** RB는 source만 선택하고 dispatching=true. 실제 source StatefulSet을 포함한 Work의 spec.suspendDispatching=true까지 기다립니다. 관련 Work를 이름 추정만으로 고르지 말고 manifests 안의 StatefulSet name/namespace로 식별합니다. 이 확인 전 Member replicas를 바꾸면 Karmada가 다시 생성할 수 있습니다.

**중요: 주기 checkpoint를 그대로 이전하지 않습니다.** resume=true인 round는 source를 재개하면서 공유 NFS lock을 제거합니다. 그 archive로 target을 복원하면 수동 resume 전에 실행을 시작할 수 있습니다. 계획 이전에서는 다음처럼 새 고유 이름으로 **최종 resume=false checkpoint**를 만들고 두 rank lock을 유지합니다.

~~~bash
export LAST_MIGRATION="trainer-final-$(date -u +%Y%m%d%H%M%S)"
python3 - <<'PY'
import json, os, pathlib, yaml
root = pathlib.Path(os.environ["ROOT"])
docs = list(yaml.safe_load_all((pathlib.Path(os.environ["ST"]) /
    "config/samples/two-replica/checkpoint.yaml").read_text()))
name = os.environ["LAST_MIGRATION"]
for o in docs:
    if o["kind"] == "PropagationPolicy":
        o["metadata"]["name"] = name + "-placement"
        o["spec"]["resourceSelectors"][0]["name"] = name
        o["spec"]["placement"]["clusterAffinity"]["clusterNames"] = [os.environ["SOURCE_CLUSTER"]]
    else:
        o["metadata"]["name"] = name
        o["metadata"]["annotations"]["training.dcnlab.com/checkpoint-id"] = name
        o["spec"]["workloadRef"]["uid"] = os.environ["WORKLOAD_UID"]
        o["spec"]["resume"] = False
(root / "rendered/final-checkpoint.json").write_text(
    json.dumps({"apiVersion": "v1", "kind": "List", "items": docs}, indent=2))
PY
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f "$ROOT/rendered/final-checkpoint.json"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get fluidcrmigration "$LAST_MIGRATION" -o yaml
~~~

8단계와 같은 current-generation Completed 및 두 archive의 durableRef/digest/exportedAt을 확인한 뒤 다음을 실행합니다. 실패/시간 초과면 fence와 target dispatch를 진행하지 않습니다.

~~~bash
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" exec trainer-0 -c trainer -- \
  test -f /checkpoint/trainer-0/lock
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" exec trainer-1 -c trainer -- \
  test -f /checkpoint/trainer-1/lock
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get fluidcrmigration "$LAST_MIGRATION" -o json \
  > "$ROOT/evidence/checkpoint.json"
~~~

공유 lock을 제거하는 다른 controller/resume 호출이 없어야 합니다. 이 마지막 round의 lock을 target 양쪽 native restore가 준비될 때까지 유지합니다.

### 9.3 source fencing과 PVMetadata 확인

source fencing은 **기존 writer가 더 이상 실행/쓰기할 수 없게 하는 것**입니다. RB pause는 실행 중 프로세스를 정지하지 않습니다. 본 계획 이전은 archive 완료 후 source StatefulSet을 Member에서 0으로 내려 실제 Pod 종료를 확인합니다. 원격 단절로 종료를 확인할 수 없다면 storage/network/VM fencing 절차 없이는 sourceFenced=true로 선언하지 않습니다.

~~~bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get pvmetadata -o yaml \
  > "$ROOT/evidence/pvmetadata-before-fence.yaml"
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" scale statefulset "$APP" --replicas=0
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" wait \
  --for=delete pod/trainer-0 pod/trainer-1 --timeout=180s
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get pods,pvc
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get sts "$APP"
~~~

**통과:** source 학습 Pod 없음, 다른 writer 없음, PVC 유지, Karmada 원본 replicas=2와 UID 유지. checkpoint 이후 fence 사이의 학습은 선택 checkpoint로 rollback될 수 있음을 기록합니다. 이 CLI의 Member 접근은 **운영자 fencing/검증**이며 MGMT controller가 Member kubeconfig를 사용하는 것은 아닙니다.

### 9.4 PVMigration

~~~bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get pvmetadata
export PV_METADATA=''  # 현재 workload UID/source와 맞는 PVMetadata 이름
export PV_MIGRATION=trainer-pv-001
test -n "$PV_METADATA" || { echo 'Select current PVMetadata'; exit 1; }
envsubst < "$ST/config/samples/two-replica/pvmigration.yaml" > "$ROOT/rendered/pvmigration.yaml"
# 실제 source fencing 확인 후 spec.sourceFenced만 true로 설정
python3 - <<'PY'
import os, pathlib, yaml
p = pathlib.Path(os.environ["ROOT"]) / "rendered/pvmigration.yaml"
o = yaml.safe_load(p.read_text())
o["metadata"]["name"] = os.environ["PV_MIGRATION"]
o["spec"]["sourceFenced"] = True
p.write_text(yaml.safe_dump(o, sort_keys=False))
PY
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f "$ROOT/rendered/pvmigration.yaml"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get pvmigration "$PV_MIGRATION" -o yaml
kubectl --kubeconfig="$TARGET_KUBECONFIG" get pv
~~~

**통과:** PVMigration observedGeneration=current, phase=Completed, planHash 비어 있지 않음, works 2개 모두 applied/detached=true, namespace=karmada-es-target. mapping은 checkpoint-trainer-0→동일 이름, checkpoint-trainer-1→동일 이름. target PV는 남고 PV용 Work는 삭제되어야 합니다. workload RB는 여전히 suspended여야 합니다. target이 workload RB에 이미 포함되었으면 PV 준비를 중단합니다.

PVMetadata.spec.workloadRef.uid는 Karmada UID입니다. status.clusters[].workloadUID는 Member-local UID이므로 두 값을 같은 것으로 검사하지 않습니다. Completed는 과거 준비 완료 증거이며 현재 NFS 가용성 증거가 아닙니다. target에서 NFS와 archive 접근을 다시 확인합니다.

## 10. RestoreRequest → 사용자 placement → Verified

### 10.1 실제 checkpoint 증거로 RestoreRequest 작성

9단계 source fence와 PV 준비가 **모두 완료된 뒤** 실행합니다. 아래 코드는 현재 CR에서 UID/generation/node/archive/digest를 읽으므로 예제의 고정 checkpoint 이름이나 generation=1을 사용하지 않습니다.

~~~bash
export RESTORE=trainer-restore-001
export TRAINING_RUNTIME_UID="$(kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" \
  get trainingruntime "$RUNTIME_NAME" -o jsonpath='{.metadata.uid}')"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get fluidcrmigration "$LAST_MIGRATION" -o json \
  > "$ROOT/evidence/checkpoint.json"
python3 - <<'PY'
import json, os, pathlib, re
root = pathlib.Path(os.environ["ROOT"])
c = json.loads((root / "evidence/checkpoint.json").read_text())
m = c["metadata"]
reports = [x for x in c["status"]["clusters"] if x["clusterName"] == os.environ["SOURCE_CLUSTER"]]
assert len(reports) == 1
r = reports[0]
assert r["phase"] == "Completed" and r["observedGeneration"] == m["generation"]
checkpoint_id = m["annotations"]["training.dcnlab.com/checkpoint-id"]
assert checkpoint_id and m["name"] == os.environ["LAST_MIGRATION"]
pods = []
for i in range(2):
    name = "trainer-" + str(i)
    observed = [p for p in r["pods"] if p["podName"] == name]
    assert len(observed) == 1
    p = observed[0]
    files = p["checkpointFiles"]
    assert len(files) == 1
    f = files[0]
    assert f["containerName"] == "trainer" and f["exportedAt"]
    assert re.fullmatch("[0-9a-f]{64}", f["sha256"])
    assert f["durableRef"].startswith("file-store:") and f["filePath"].startswith("/")
    pods.append({"sourcePod": name, "sourceNode": p["nodeName"], "targetPod": name,
        "targetNode": os.environ["TARGET_NODE_" + str(i)], "archives": [{
            "containerName": "trainer", "sourcePath": f["filePath"],
            "targetPath": "/var/lib/kubelet/checkpoints/" + f["sha256"] + ".tar",
            "sha256": f["sha256"]}]})
o = {"apiVersion": "migration.dcnlab.com/v1alpha1", "kind": "RestoreRequest",
    "metadata": {"name": os.environ["RESTORE"], "namespace": os.environ["NS"]},
    "spec": {
        "checkpointRef": {"name": m["name"], "uid": m["uid"],
            "generation": m["generation"], "checkpointID": checkpoint_id},
        "workloadRef": {"apiVersion": "apps/v1", "kind": "StatefulSet",
            "name": os.environ["APP"], "uid": os.environ["WORKLOAD_UID"]},
        "trainingRuntimeRef": {"name": os.environ["RUNTIME_NAME"],
            "uid": os.environ["TRAINING_RUNTIME_UID"]},
        "sourceCluster": os.environ["SOURCE_CLUSTER"], "targetCluster": os.environ["TARGET_CLUSTER"],
        "sourceFenced": True, "volumesReady": True, "pods": pods}}
(root / "rendered/restore-request.json").write_text(json.dumps(o, indent=2))
PY
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply --dry-run=server -f "$ROOT/rendered/restore-request.json"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f "$ROOT/rendered/restore-request.json"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get restorerequest "$RESTORE" -o yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get restoreplans -o yaml
~~~

RestoreRequest는 Karmada 관리 객체이며 Member로 직접 배포하지 않습니다. management가 RestorePlan 및 source/target 전파 정책을 생성합니다. member artifact controller가 durableRef를 따라 archive를 내려받고 digest를 검증합니다.

**통과:** 현재 generation RestoreRequest=Prepared, 연결된 RestorePlan target report=Prepared 및 두 target node의 artifact 확인. 아직 GPU 복원 성공이 아닙니다. Prepared가 안 되면 target DS, PVC, digest, node mapping을 확인하고 status를 수동 수정하지 않습니다.

### 10.2 operation UID 연결 후 사용자가 placement 변경

~~~bash
export PLAN="$(kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get restorerequest "$RESTORE" -o jsonpath='{.status.planName}')"
export RR_UID="$(kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get restorerequest "$RESTORE" -o jsonpath='{.metadata.uid}')"
export PV_UID="$(kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get pvmigration "$PV_MIGRATION" -o jsonpath='{.metadata.uid}')"
test -n "$PLAN" && test -n "$RR_UID" && test -n "$PV_UID" || { echo 'Missing operation identity'; exit 1; }
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" annotate resourcebinding "$RB_NAME" --overwrite \
  "migration.dcnlab.com/restore-request=$RESTORE" \
  "migration.dcnlab.com/restore-request-uid=$RR_UID" \
  "migration.dcnlab.com/pv-migration=$PV_MIGRATION" \
  "migration.dcnlab.com/pv-migration-uid=$PV_UID"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" patch sts "$APP" --type=merge \
  -p "$(jq -nc --arg p "$PLAN" '{spec:{template:{metadata:{labels:{"migration.dcnlab.com/restore-plan":$p}}}}}')"
# 사용자 주도 placement 변경. 직접 dispatch pause를 해제하지 않습니다.
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" patch propagationpolicy "$APP-source-only" --type=merge \
  -p "$(jq -nc --arg t "$TARGET_CLUSTER" '{spec:{placement:{clusterAffinity:{clusterNames:[$t]}}}}')"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get resourcebinding "$RB_NAME" -o yaml
kubectl --kubeconfig="$TARGET_KUBECONFIG" -n "$NS" get pods,pvc -o wide
~~~

Suspension controller가 현재 RR/PV UID, generation, checkpoint, workload UID, 정확한 2개 PVC mapping 및 Prepared를 검사한 후 **dispatching 필드를 제거**합니다. false로 바꾸는 것이 아닙니다. PV controller는 자동 resume하지 않습니다.

**통과:** RB target 단독 선택, dispatching 필드 없음, release annotation은 해당 RR UID, target trainer-0/1 생성. controller가 거부하면 MGMT stateful-management 로그를 확인하고 수동 unpause로 우회하지 않습니다.

### 10.3 DDP resume와 실제 복원 검증

두 target Pod의 native CRI-O/CRIU restore 로그, restore annotation, 실제 archive 사용, GPU 복구를 확인합니다. 새로운 프로세스가 step=0부터 시작한 것은 복원 성공이 아닙니다. 한 rank라도 실패하거나 source writer가 남아 있으면 resume하지 않습니다.

원본 FluidCR DDP 절차와 해당 trainer의 resume 계약을 확인한 뒤 **모든 rank 준비 이후 한 번만** coordinator resume를 보냅니다. 다음 명령은 사용 중인 FluidCR control registry가 두 rank를 올바르게 조정하는 구성이 확인된 경우의 호출입니다. 두 독립 작업처럼 각 Pod에 무조건 반복하지 않습니다.

~~~bash
kubectl --kubeconfig="$TARGET_KUBECONFIG" -n "$NS" exec trainer-0 -c trainer -- \
  test -f /checkpoint/trainer-0/lock
kubectl --kubeconfig="$TARGET_KUBECONFIG" -n "$NS" exec trainer-1 -c trainer -- \
  test -f /checkpoint/trainer-1/lock
kubectl --kubeconfig="$TARGET_KUBECONFIG" -n "$NS" exec trainer-0 -c trainer -- \
  env PYTHONPATH=/opt/fluidcr python -m fluidcr.ctrl resume --all
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingruntime "$RUNTIME_NAME" -o yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get restorerequest "$RESTORE" -o yaml
~~~

**최종 통과:** RestoreRequest.status.phase=Verified, observedGeneration=current, verification.requestUID/checkpointID/sourceCluster/targetCluster/trainingRuntimeRef 일치, verifiedAt 존재. target 두 rank가 선택한 동일 checkpointID로 Running이고 복원 이후 globalStep이 증가해야 합니다. RestoreRequest가 Running을 지나 이미 Verified일 수 있으므로 Running과 정확히 같기만을 기다리지 않습니다. 반대로 Verified는 학습 진행을 요구하므로 resume 전에 Verified를 기다리면 막힐 수 있습니다.

추가로 모델/optimizer/step 및 결과를 무중단 기준 실행과 비교합니다. /runtime 보고만으로 모델 수치적 정합성이나 NCCL 정상성을 모두 증명하지 않습니다. source 중지→target 첫 advancing step 시간을 downtime으로 기록합니다.

이 시점에 split System MGMT controller는 여전히 정지 상태입니다. 원래 Policy는 source=aws를 가리킵니다. target에서 계속 주기 checkpoint하려면 target checkpoint controller와 새로운 정책/용량 소유권 계약을 별도로 준비해야 하며, sourceCluster만 바꾸어 자동 전환되는 것으로 보지 않습니다. 복구 정리 작업을 수행하기 전 기존 AWS NP와 Policy의 상태를 검토합니다.

## 11. SpotWatcher·긴급 checkpoint·Policy cleanup 검증

### 11.1 먼저 비파괴 관측

~~~bash
kubectl --kubeconfig="$AWS_KUBECONFIG" -n hybridspot-system get pods -o wide
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get nodeprovisions -o yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get nodeprovisions -o yaml
~~~

AWS NP.status.spot과 Karmada NP.status.spot/observedCluster를 비교합니다. **이벤트가 없으면 atRisk=false 또는 아직 event 정보가 없는 것이 정상**입니다. Member NP UID와 node 소유권 label, instanceId를 확인합니다. RIC status를 제공한다고 별도 가짜 status를 주입하지 않습니다.

### 11.2 실제 interruption 시험은 새 실험에서 마지막에 수행

계획 이전 리허설을 성공한 뒤 별도의 source 실행 상태로 준비한 실험에서 진행합니다. 10단계에서 이미 onprem으로 옮긴 job을 그대로 두고 "긴급 checkpoint 시험"이라고 부르지 않습니다. 새 workload/operation 식별자를 사용하며 기존 이력을 덮어쓰지 않습니다.

AWS 관리자가 **테스트용 Spot 인스턴스만** 선택하여 조직에서 승인한 interruption 실험(예: AWS FIS)을 준비합니다. 범위/중단 조건/비용을 검토한 뒤 실행합니다. 이 문서는 자동 EC2 terminate 명령을 제공하지 않습니다. 단순 terminate API는 Spot 사전 알림 검증과 같지 않습니다.

검증 순서:

1. source의 두 rank Running, 최신 durable checkpoint, target 2개 GPU 및 storage 준비.
2. split System MGMT controller 실행 상태에서 실제 신호 수신.
3. SpotWatcher가 status.spot에 signalType/eventID/instanceID/atRisk를 기록.
4. RIC가 이를 Karmada에 반영. CheckpointCoordinator가 새 긴급 round를 만들거나 inflight round를 중복 없이 재사용.
5. deadline 안에 새 archive가 durable해졌는지 확인. 실패 시 마지막 유효 checkpoint를 사용하며 RPO 손실을 기록.
6. 실제 source fencing 확인 후 9~10단계의 **새 operation**으로 이전. 사용자가 placement를 변경.
7. Verified 이후에만 SpotRecovery를 제출.

실제 Spot 선점은 restore 완료를 기다려주지 않습니다. 사전 준비가 없으면 notice 시간 내 새 worker 준비/전체 migration을 보장할 수 없습니다. unreachable source를 fence 완료로 추정하지 않습니다.

Spot 종료 때문에 최종 resume=false checkpoint를 만들 수 없어 주기 archive로 되돌아가는 경우에는 공유 lock이 남아 있다고 가정하지 않습니다. 실제 source fencing 후 두 rank의 대기 lock/선택 round를 검증·복구하는 별도 승인된 절차가 필요합니다. 현재 이 문서는 해당 장애 상황의 자동 lock 복구를 제공하지 않으므로, 그 절차가 검증되지 않았다면 target dispatch 전에 중단합니다. 계획 이전 성공과 예고 없이 끊긴 Spot의 복구 성공을 분리하여 기록하세요.

### 11.3 SpotRecovery 작성

계획 이전에 실제 Spot event가 없었다면 이 절을 **건너뜁니다**. 새 시험의 실제 old Spot NP 이름과 검증된 RR을 선택합니다. 새 AWS replacement가 없는 onprem target이므로 optional replacementNodeProvisionRef는 넣지 않습니다.

~~~bash
export OLD_NODEPROVISION_NAME=''  # 실제 atRisk Spot NP, Karmada 객체 이름
export OPERATION=''              # 새 시험의 고유 operation 이름
test -n "$OLD_NODEPROVISION_NAME" && test -n "$OPERATION" || { echo 'Select actual event/operation'; exit 1; }
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get nodeprovision "$OLD_NODEPROVISION_NAME" -o json > "$ROOT/evidence/old-np.json"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingpolicy "$POLICY_NAME" -o json > "$ROOT/evidence/policy.json"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingruntime "$RUNTIME_NAME" -o json > "$ROOT/evidence/runtime.json"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get restorerequest "$RESTORE" -o json > "$ROOT/evidence/restore.json"
python3 - <<'PY'
import json, os, pathlib
root = pathlib.Path(os.environ["ROOT"])
def read(name):
    return json.loads((root / ("evidence/" + name + ".json")).read_text())
np, policy, runtime, rr = [read(n) for n in ("old-np", "policy", "runtime", "restore")]
assert np["spec"]["marketType"] == "Spot"
s = np["status"]
assert s["observedCluster"] == "aws" and s["spot"]["atRisk"]
assert s["spot"]["instanceID"] == s["instanceId"]
assert s["spot"]["signalType"] in ("InterruptionNotice", "RebalanceRecommendation")
assert rr["status"]["phase"] == "Verified"
assert rr["status"]["observedGeneration"] == rr["metadata"]["generation"]
assert rr["spec"]["sourceFenced"] and rr["spec"]["sourceCluster"] == "aws"
assert rr["spec"]["trainingRuntimeRef"]["uid"] == runtime["metadata"]["uid"]
assert rr["status"]["verification"]["requestUID"] == rr["metadata"]["uid"]
def ref(o, generation=False):
    result = {k: o["metadata"][k] for k in ("name", "uid")}
    if generation:
        result["generation"] = o["metadata"]["generation"]
    return result
o = {"apiVersion": "training.dcnlab.com/v1alpha1", "kind": "SpotRecovery",
    "metadata": {"name": os.environ["OPERATION"], "namespace": os.environ["NS"]},
    "spec": {"policyRef": ref(policy, True), "requestUID": rr["metadata"]["uid"],
        "operation": os.environ["OPERATION"], "sourceCluster": "aws",
        "targetCluster": rr["spec"]["targetCluster"], "workloadRef": rr["spec"]["workloadRef"],
        "trainingRuntimeRef": ref(runtime), "checkpointID": rr["spec"]["checkpointRef"]["checkpointID"],
        "eventID": s["spot"]["eventID"], "restoreRequestRef": ref(rr, True),
        "oldNodeProvisionRef": ref(np)}}
(root / "rendered/spot-recovery.json").write_text(json.dumps(o, indent=2))
PY
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply --dry-run=server -f "$ROOT/rendered/spot-recovery.json"
# 내용과 삭제 대상을 확인한 뒤: Policy Manager의 old Spot VM 삭제 허용
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f "$ROOT/rendered/spot-recovery.json"
~~~

Policy/NP UID, sourceNode, checkpoint/restore/runtime identity의 모든 최종 검사는 controller가 수행합니다. 위 Python은 전체 controller gate의 대체물이 아닙니다. 9단계에서 split System MGMT controller를 멈췄다면 기존 policy가 불필요한 AWS VM을 새로 만들지 않을지 확인한 뒤 복구합니다. source가 fenced 상태면 runtime_not_ready로 새 checkpoint가 막혀야 합니다.

~~~bash
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system scale deployment/vm-spot-risk-collector --replicas=1
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system scale deployment/policy-manager --replicas=1
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system scale deployment/checkpoint-coordinator --replicas=1
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system scale deployment/spot-recovery-controller --replicas=1
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get spotrecovery "$OPERATION" -o yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get nodeprovisions
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get nodeprovisions
~~~

**통과:** CleanupRequested→Completed, 지정 old NP의 Karmada/Member 삭제, 실제 EC2 종료까지 확인. Completed는 restore가 아니라 cleanup 결과입니다. Rejected이면 status를 고치지 말고 원인과 current identity를 확인합니다. finalizer를 강제로 지우지 않습니다.

SpotRecovery 이력은 **TrainingPolicy 수명 동안 유지**합니다. Rejected 이력도 old NP slot 재생성 방지에 관여합니다. 단순 재시도를 위해 이력을 삭제하지 않습니다. 고정 N은 목표 구성이지 선점 중 항상 N이 가용하다는 보장이 아니며, 이 cleanup 경로는 onprem capacity 생성이나 placement를 대신하지 않습니다.

## 12. 증거 보관·종료·문제 진단

~~~bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get \
  trainingpolicies,trainingruntimes,spotriskprofiles,fluidcrmigrations,pvmetadata,pvmigrations,restorerequests,restoreplans,spotrecoveries \
  -o yaml > "$ROOT/evidence/final-state.yaml"
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system logs deployment/vm-spot-risk-collector --tail=300 \
  > "$ROOT/evidence/system-vm-spot-risk-collector.log"
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system logs deployment/policy-manager --tail=300 \
  > "$ROOT/evidence/system-policy-manager.log"
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system logs deployment/checkpoint-coordinator --tail=300 \
  > "$ROOT/evidence/system-checkpoint-coordinator.log"
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system logs deployment/spot-recovery-controller --tail=300 \
  > "$ROOT/evidence/system-spot-recovery-controller.log"
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n stateful-migration-system logs deployment/stateful-management --tail=300 \
  > "$ROOT/evidence/stateful.log"
~~~

| 증거 | 합격 기준 |
|---|---|
| 설치 | 이미지 digest/commit, CRD 필드 보존, 각 controller Ready |
| capacity | NP/EC2 정확히 2개, 선택 Spot/OD 비율과 실제 일치 |
| telemetry | Karmada 원본 UID 연결, 두 rank 최신 timestamp, step 증가 |
| checkpoint | 시간 주기 변경 반영, 중복 없음, 동일 round, archive durable |
| PV | 현재 UID/generation, 2개 applied+detached, PV 유지/Work 제거 |
| suspension | 준비 전 target 미선택, source fence, 현재 operation만 해제 |
| restore | 두 rank native restore, Verified identity, advancing step, 모델 비교 |
| Spot cleanup | 실제 이벤트→RIC→검증된 RR→특정 old NP/EC2 삭제 |
| 평가 | checkpoint 시간/전송량, downtime, rollback step, 비용, 실패 원인 |

실험 종료 때는 **Policy 생성 루프를 먼저 중단**합니다. 시험용 TrainingPolicy를 Karmada에서 삭제한 뒤 해당 controller의 처리 중 작업이 끝났는지 확인하고, 정확히 해당 실험 소유인 NodeProvision만 Karmada에서 삭제합니다. 삭제는 EC2 종료를 유발합니다. Member NP만 먼저 지우면 Karmada가 다시 만들 수 있습니다. 생성된 per-NP PP는 NP 제거 확인 후 정리합니다. workload와 source/target PVC/PV, archive는 결과 보관 정책을 결정하기 전 삭제하지 않습니다. Retain은 데이터 자동 삭제가 아니라 보존 의도이며 NFS 자체의 백업을 대신하지 않습니다.

정상 종료 계획 없이 CRD/namespace/controller를 먼저 지우거나 finalizer를 제거하지 마세요. cleanup 수행 주체가 사라져 cloud 자원이 남을 수 있습니다. Restore label이 template에 남으면 다음 재시작도 오래된 archive를 사용할 수 있으므로, 검증 완료 후 OnDelete 상태를 유지한 채 정상 재시작 정책과 label 정리를 별도로 결정합니다.

| 증상 | 먼저 확인할 것 |
|---|---|
| controller Pending | control-plane taint/자원 부족, 해당 Deployment toleration |
| ImagePullBackOff | 직접 push 여부, registry 권한, payload 포함 pull Secret |
| NP 생성 안 됨 | risk freshness, workload UID, NetConfig 단일성, Policy status reason |
| Ready 후 GPU 없음 | driver/toolkit/Operator 상태; Node Ready와 GPU Ready 구분 |
| runtime_not_ready | injector, /runtime timestamp/port, rank/worldSize, 원본 UID label |
| checkpoint 실패 | kubelet CA/IP SAN/RBAC, hook/DDP barrier, CRI-O/CRIU 호환 |
| durableRef 없음 | source artifact DS label/PVC mount/권한, digest/export 로그 |
| Prepared 대기 | target artifact download/digest/node mapping, 동일 store 여부 |
| dispatch 미해제 | RR/PV UID·generation·workload/PVC mapping, target 단독 placement |
| 복원 후 step=0 | fresh start인지 확인; native restore/모델 상태 증거 필요 |
| 401 Unauthorized | 제한된 Karmada token 만료, Secret 갱신 후 rollout |
| cleanup 거부 | 실제 event·old NP UID·current Verified/Policy generation 불일치 |

**남은 실환경 작업:** 공개 trainer 이미지, runtime package hosting, target cluster, IAM/VPC/NFS 및 GPU 호환성은 사용자 환경 입력입니다. 이 가이드는 이를 임의의 성공 값으로 채우지 않습니다. 준비되지 않은 단계에서 중단한 결과도 검증 기록에 그대로 남기세요.
