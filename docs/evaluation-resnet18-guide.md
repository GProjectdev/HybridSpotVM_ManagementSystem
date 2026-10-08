# ResNet18 평가 실행 가이드

이 문서는 기존 MGMT/Karmada/AWS 환경의 동작 검증을 마친 뒤 새 ResNet18 학습과 평가 toolkit을 연결하는 절차다. 이미 검증한 provisioning, checkpoint/restore, replacement 전체 시나리오를 처음부터 반복하지 않는다. 먼저 새 이미지·데이터·DDP·runtime 수집의 짧은 연동 smoke를 확인하고 같은 조건의 본 실험으로 진행한다. 이 문서 작성이나 로컬 테스트 통과는 실제 GPU 실행·복원·비용 절감 검증을 의미하지 않는다.

기존 namespace는 `fluidcr-realign-121040`이다. 기존 `trainer-realign`은 안전한 spec 템플릿을 가져오는 원본이며 평가 도구가 덮어쓸 대상이 아니다. 매 실행은 새로운 run-id로 구분하고 해당 run의 리소스·증거만 관리한다. 데이터셋, kubeconfig, Secret 값, 개인 설정과 수집 증거를 Git에 commit하지 않는다.

## 실행 경계

run.json.phase는 rendered → prepared → running → completed 또는 failed다. stop/cleanup은 별도 명령이며 stopped/cleaned라는 phase 전환으로 추정하지 않는다. phase만으로 두 rank와 scenario 증거를 대체하지 않는다.

- `capture-policy`: 기존 정책에서 재사용할 안전한 spec 기본값을 로컬 파일로 추출한다. 실제 Secret 내용을 복사하는 단계가 아니다.
- `render`: 설정과 run-id로 평가 manifest와 증거 디렉터리를 준비한다.
- `prepare`: 같은 namespace에 실행 전용 리소스를 새로 만들며 policy suspend, workload replicas 0 상태를 유지한다.
- `start`: 수집과 provisioning/학습을 시작하고 목표 또는 제한 시간까지 관찰한다. 종료 시 suspend 후 최대 180초간 정지 조건을 확인하고, 확인된 경우에만 replicas 0으로 멈춘다. VM 자동 삭제를 의미하지 않는다.
- `cleanup`: 정확한 run-id와 정지 조건을 확인한 뒤 해당 실행 소유 리소스만 정리한다. 정지 확인 실패 시 Pod·VM을 보존하며 PVC 삭제와 finalizer 강제 제거는 수행하지 않는다.

종료·실패 후에도 남은 VM과 EBS/NFS 등의 비용은 별도로 확인한다. 수집 프로세스 중단이나 Pod 수 0만으로 과금 종료라 판단하지 않는다. 진행 중인 복구를 임의로 삭제하거나 기존 실험의 status/finalizer를 고쳐 새 실행에 재사용하지 않는다.

## 1. 준비 환경과 controller 버전

Linux MGMT Bash의 새 작업 상위 디렉터리에서 아래 clone을 실행한 뒤 저장소 루트 `System`을 기준으로 진행한다. 기존 dirty checkout은 덮어쓰지 않는다. Python 3.10 이상, kubectl, jq, Buildah와 registry push 권한이 필요하다. AWS 상태/FIS 조회에는 AWS CLI의 기존 인증과 해당 권한이 필요하다. kubeconfig는 파일 경로만 지정한다. ZIP 다운로드에는 .git이 없어 git archive와 commit 기록 명령을 사용할 수 있으리라 가정하면 안 된다. 이 가이드는 아래 Git clone 경로를 사용한다.

```bash
set -euo pipefail
umask 077
git clone --branch restore-automation-20260928 https://github.com/GProjectdev/HybridSpotVM_ManagementSystem.git System
cd System
export MGMT_KUBECONFIG=/absolute/path/to/mgmt.kubeconfig
export KARMADA_KUBECONFIG=/absolute/path/to/karmada.kubeconfig
export AWS_KUBECONFIG=/absolute/path/to/aws.kubeconfig
test -s "$MGMT_KUBECONFIG"
test -s "$KARMADA_KUBECONFIG"
test -s "$AWS_KUBECONFIG"
export NS=fluidcr-realign-121040
python3 --version
python3 evaluation/run.py --help
buildah version
kubectl --kubeconfig="$KARMADA_KUBECONFIG" get namespace "$NS"
kubectl --kubeconfig="$AWS_KUBECONFIG" get namespace "$NS"
git rev-parse HEAD
git status --short
```

평가 2(checkpoint)는 `spec.policy.fixedOnDemand=1`로 두 worker의 구성을 OD 1 + Spot 1로 고정한다. `minOnDemand=1`은 하한일 뿐 고정 구성이 아니므로 대신 사용할 수 없다. 고정 구성은 위험률 기반 checkpoint 적응과 interruption recovery를 끄지 않는다.

checkpoint renderer는 `training.dcnlab.com/planned-partial=disabled`를 명시하며 FIS 이후 기존 **full-group restore**를 사용한다. F60/F300/F600/D 모두 같은 복구 방식으로 checkpoint 주기만 비교하며 partial 알고리즘 비교가 아니다. 기존 partial의 same-market Spot→Spot 금지 계약은 변경하지 않는다. 고정 구성의 emergency Spot 유지도 이 명시적 group 모드에서만 사용한다. cost B는 기존 partial market-flip 경로를 유지한다.

이 필드를 지원하는 CRD와 **checkpoint-coordinator, policy-manager 모두** 필요하다. 기존 v3.0 이미지에 이 변경이 없다면 같은 최종 코드에서 두 개를 다시 빌드하고 `checkpoint_coordinator_v3.0-eval.1`, `policy_manager_v3.0-eval.1`처럼 별도 tag로 배포한다. 기존 v3.0 tag를 덮어쓰지 않는다. CRD는 Karmada, Deployment는 MGMT의 `hybridspot-system`, container는 `manager`다. 기존 args를 유지하는 이미지 변경만 수행하며 전체 management manifest를 apply하지 않는다.

controller 변경은 진행 중 replacement/restore가 끝난 안정 구간에서 수행한다. 기존 이미지 참조를 먼저 별도 로컬 JSON에 보관하고 각 rollout 결과를 확인한다. CRD에 필드가 보이는 것만으로 실행 바이너리 지원이 확인된 것은 아니다. 최종 controller commit과 image digest도 실험 기록에 남긴다.

## 2. Buildah 이미지 빌드

controller를 갱신해야 하는 경우에만 다음 블록을 실행한다. 최신 toolkit·고정 구성 코드를 포함한 **확정 commit**에서 수행한다. Git archive로 별도 build context를 만들어 로컬 policy-source/config/evidence가 controller 이미지에 들어가지 않게 한다. commit하지 않은 코드는 이 context에 포함되지 않는다.

```bash
export BUILD_RECORD="$(mktemp -d "$HOME/resnet18-build.XXXXXXXX")"
export BUILD_SOURCE="$BUILD_RECORD/source"
mkdir "$BUILD_SOURCE"
git rev-parse HEAD > "$BUILD_RECORD/source.commit"
git archive HEAD | tar -x -C "$BUILD_SOURCE"
buildah login docker.io
export COORD_TAG=docker.io/jeongseungjun/hybrid-spot-vm-system:checkpoint_coordinator_v3.0-eval.1
export POLICY_TAG=docker.io/jeongseungjun/hybrid-spot-vm-system:policy_manager_v3.0-eval.1
buildah bud --arch amd64 --build-arg COMPONENT=checkpoint-coordinator -t "$COORD_TAG" "$BUILD_SOURCE"
buildah push --digestfile "$BUILD_RECORD/coordinator.digest" "$COORD_TAG" "docker://$COORD_TAG"
buildah bud --arch amd64 --build-arg COMPONENT=policy-manager -t "$POLICY_TAG" "$BUILD_SOURCE"
buildah push --digestfile "$BUILD_RECORD/policy-manager.digest" "$POLICY_TAG" "docker://$POLICY_TAG"
grep -Eq '^sha256:[0-9a-f]{64}$' "$BUILD_RECORD/coordinator.digest"
grep -Eq '^sha256:[0-9a-f]{64}$' "$BUILD_RECORD/policy-manager.digest"
export COORD_IMAGE="$COORD_TAG@$(cat "$BUILD_RECORD/coordinator.digest")"
export POLICY_IMAGE="$POLICY_TAG@$(cat "$BUILD_RECORD/policy-manager.digest")"
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system get deployment checkpoint-coordinator policy-manager -o json | jq '[.items[] | {name:.metadata.name,containers:[.spec.template.spec.containers[] | {name,image}]}]' > "$BUILD_RECORD/controller-images-before.json"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply --dry-run=server -f "$BUILD_SOURCE/config/crd/trainingpolicies.yaml"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f "$BUILD_SOURCE/config/crd/trainingpolicies.yaml"
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system set image deployment/checkpoint-coordinator "manager=$COORD_IMAGE"
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system rollout status deployment/checkpoint-coordinator --timeout=180s
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system set image deployment/policy-manager "manager=$POLICY_IMAGE"
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system rollout status deployment/policy-manager --timeout=180s
```

위 명령은 관리 controller 교체이며 실험 시작 명령이 아니다. 오류가 나면 다음 단계로 진행하지 않는다. rollback은 보관한 두 image 참조로 manager 이미지만 되돌리는 방식이며 CRD 삭제·전체 manifest 덮어쓰기를 하지 않는다. 로그인 token/password는 명령행이나 문서에 넣지 않는다.

학습 이미지는 별도로 빌드한다. dataset·checkpoint·kubeconfig가 없는 `evaluation/resnet18` 디렉터리를 context로 사용한다.

```bash
export TRAIN_BUILD="$(mktemp -d "$HOME/resnet18-training-build.XXXXXXXX")"
export TRAIN_TAG=docker.io/jeongseungjun/hybrid-spot-vm-system:resnet18_eval_v1
buildah login docker.io
buildah bud --arch amd64 -f evaluation/resnet18/Dockerfile -t "$TRAIN_TAG" evaluation/resnet18
buildah push --digestfile "$TRAIN_BUILD/training.digest" "$TRAIN_TAG" "docker://$TRAIN_TAG"
grep -Eq '^sha256:[0-9a-f]{64}$' "$TRAIN_BUILD/training.digest"
printf '%s@%s\n' "$TRAIN_TAG" "$(cat "$TRAIN_BUILD/training.digest")"
```

출력한 tag@digest를 다음 config의 training_image에 넣는다. 이미지의 PyTorch/torchvision 버전은 Dockerfile을 기준으로 고정한다. FluidCR은 기존 webhook payload로 주입하며 PyPI에서 임의 설치하거나 launcher를 이중으로 감싸지 않는다. renderer와 이미지의 command는 `/workspace/train_resnet18.py`다. runner가 `/workspace`에 script ConfigMap을 mount하므로 render한 소스와 이미지의 최종 commit도 함께 기록한다.

## 3. 원본 정책과 데이터 설정

먼저 기존 정책의 spec을 추출한 뒤 example을 개인 설정 파일로 복사한다. `capture-policy` 결과는 metadata/status를 복제해 원본 객체를 재생성하려는 파일이 아니라 render의 spec 기본값이다. networking과 기존 credentialsRef 등 필요한 참조를 보존하며 실제 Secret data는 가져오지 않는다.

```bash
python3 evaluation/run.py capture-policy --name trainer-realign --output evaluation/policy-source.json
cp -n evaluation/config.example.json evaluation/config.local.json
python3 -m json.tool evaluation/config.local.json
```

기존 trainer-realign 정책을 이미 삭제했다면 capture-policy 대신 아래 템플릿 경로를 사용한다. JSON 문법의 유효한 YAML을 복사한 뒤 AWS placeholder(AMI, subnet/VPC, security groups, instance profile, 기존 Secret 이름)를 실제 값으로 채운다. 원본 trainer나 namespace를 재생성할 필요는 없다. credentialsRef에는 기존 Secret 참조만 쓰고 자격증명 값을 넣지 않는다.

```bash
cp -n evaluation/policy-source.example.yaml evaluation/policy-source.json
cp -n evaluation/config.example.json evaluation/config.local.json
python3 -m json.tool evaluation/policy-source.json
```

`config.local.json`에서 아래 값을 실제 환경과 실험 계획에 맞게 편집한다. `policy_source` 상대 경로는 config 파일이 있는 디렉터리를 기준으로 해석된다. 루트 evidence/는 gitignored지만 개인 설정·policy source의 비밀정보 보호까지 보장하는 것은 아니다. commit 전에 git status로 경로를 확인하고 개인 파일·dataset·Secret은 추가하지 않는다.

| 설정 | 확인할 값 |
| --- | --- |
| namespace | `fluidcr-realign-121040` 유지 |
| cluster / region / availability_zone | `aws` / `ap-northeast-2` / `ap-northeast-2c` |
| instance_type | 비교 실험 전체에서 동일한 GPU instance type; example은 g5.xlarge |
| policy_source | `policy-source.json`; 원본 spec의 AMI, subnetId, vpcId, securityGroupIds, credentialsRef가 실제 환경과 일치해야 함 |
| nfs_server | worker가 접근 가능한 실제 NFS host/IP |
| nfs_dataset_path | 이미 다운로드·압축 해제한 `cifar-10-batches-py` 디렉터리의 **부모**인 절대 export 경로 |
| training_image | 빌드·push한 resnet18_eval_v1 이미지의 tag@digest |
| storage_class / checkpoint_size | 실제 checkpoint용 RWX StorageClass와 용량; example은 nfs-client / 10Gi |
| goal_steps / max_seconds | 목표 optimizer step과 provisioning을 포함한 전체 실행 제한 시간 |
| batch_size / seed | 모든 비교 arm에서 같은 값 사용; batch_size는 rank당 값이며 world-size 2에서 global batch는 그 두 배 |
| scenario | constant, risk-rise, interruption |
| initial_risk / changed_risk | 실험에 사용할 위험률; 실제 관측 interruption과 구분 |
| risk_change_after_seconds | risk-rise에서 두 rank의 학습 진행이 관측된 뒤 기다릴 시간 |

예를 들어 NFS 서버에 `/exports/datasets/cifar10/cifar-10-batches-py/data_batch_1`이 있다면 nfs_dataset_path는 `/exports/datasets/cifar10`이다. Pod에서는 `/datasets/cifar10/cifar-10-batches-py/data_batch_1`로 보여야 한다. 데이터 다운로드는 이미 완료된 전제이며 매 실행마다 다시 다운로드하지 않는다. dataset mount는 read-only이고 checkpoint 저장소와 분리한다.

이 toolkit은 run별 dataset PV/PVC와 checkpoint PVC를 새로 만든다. 기존 학습 PVC를 삭제·재활용하지 않으며 cleanup 후에도 이 PV/PVC는 남긴다. retained claim 때문에 같은 run-id를 재사용하지 않는다. 개인 설정·policy source·evidence에는 운영 식별자가 포함될 수 있으므로 Git에 올리지 않는다. 빌드 context에도 kubeconfig, Secret 값, 데이터셋을 넣지 않는다.

## 4. 평가 arm과 실행 식별자

| experiment | arm | 비교 조건 |
| --- | --- | --- |
| cost | A | OD 2, checkpoint 300초 고정 |
| cost | B | 최소 OD 1 + 정책이 결정한 나머지 구성, checkpoint 300초 고정 |
| checkpoint | F60 / F300 / F600 | OD 1 + Spot 1 고정, 각각 60 / 300 / 600초 |
| checkpoint | D | OD 1 + Spot 1 고정, checkpoint 60~600초 적응 |

B의 최종 구성은 status와 실제 VM 증거로 확인한다. minOnDemand만 보고 항상 OD 1 + Spot 1이었다고 기록하지 않는다. renderer는 원본의 measuredCosts와 paperProfile을 제거하며 이번 run의 새 실측 CRIU/checkpoint 비용을 기다린다. D도 비용 측정 전 bootstrap이면 아직 실측 최적화 결과가 아니다. 실제 async 계측 없이 paperProfile을 활성화하거나 측정 시각·비용을 조작하지 않는다.

run-id는 영문 소문자로 시작하고 소문자·숫자·하이픈만 사용하며 최대 17자다. 마지막 문자는 소문자 또는 숫자여야 한다. `r18a01`의 app/policy는 `eval-r18a01`이다. 반복 실험과 다른 arm에는 각각 새 ID와 새 evidence 디렉터리를 쓴다. 같은 seed·batch·goal·image·dataset·instance type·scenario를 맞추고 반복 횟수와 실행 순서를 기록한다.

## 5. 첫 실행: 새 학습 연동 smoke

실행 길이 주의: 기본 10,000 step은 ResNet18에서 1,200초 risk 이벤트 전에 끝날 수 있다. pilot에서 측정한 iteration 시간을 바탕으로 risk/FIS 시점 이후 VM 준비 약 7분과 restore 여유 구간까지 학습이 이어지도록 goal_steps를 정한다. 약 7분은 계획용 여유이지 보장 시간이 아니며, max_seconds에는 초기 provisioning도 포함한다. 고정 처리량을 가정하거나 성능·완료 시간을 보장하지 않는다.

첫 pilot은 `scenario=constant`, 짧은 goal_steps와 유한한 max_seconds로 설정한다. provisioning 시간을 포함한 제한이므로 GPU 준비 시간을 확보한다. 두 rank의 데이터 로드·step 증가·runtime·collector 이벤트를 확인한다. 새 학습의 checkpoint 연동까지 확인하려면 최초 주기와 checkpoint 완료까지 충분한 학습 길이가 필요하다. 짧은 pilot이 먼저 끝났다면 checkpoint/restore까지 검증했다고 쓰지 않는다. pilot에서는 FIS를 실행하지 않는다.

```bash
python3 -B -m unittest discover -s evaluation -p 'test_*.py' -v
python3 -B -m unittest discover -s evaluation/resnet18 -p 'test_*.py' -v
python3 evaluation/run.py render --config evaluation/config.local.json --run-id r18a01 --experiment cost --arm A --out evidence/r18a01
jq '{run_id,app,experiment,arm,goal_steps,max_seconds,scenario,training_image}' evidence/r18a01/run.json
jq '.items[] | {kind,name:.metadata.name,spec:.spec}' evidence/r18a01/member.yaml
jq '.items[] | select(.kind == "StatefulSet") | {name:.metadata.name,replicas:.spec.replicas,command:.spec.template.spec.containers[0].command}' evidence/r18a01/karmada.yaml
jq '{name:.metadata.name,annotations:.metadata.annotations,workloadRef:.spec.workloadRef,policy:.spec.policy,checkpoint:.spec.checkpoint}' evidence/r18a01/policy.yaml
```

render 결과는 JSON 문법으로 작성된 YAML이다. `member.yaml`은 member dataset PV/PVC와 checkpoint PVC, `karmada.yaml`은 workload와 관련 리소스, `policy.yaml`은 새 TrainingPolicy다. UID placeholder는 prepare가 실제 생성된 StatefulSet UID로 바인딩한다. 직접 파일을 일괄 apply하면 이 흐름을 우회하므로 아래 prepare를 사용한다.

prepare 전 확인:

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingpolicies -o json | jq '[.items[] | {name:.metadata.name,suspend:.metadata.annotations["training.dcnlab.com/suspend"]}]'
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get statefulsets -o json | jq '[.items[] | {name:.metadata.name,replicas:.spec.replicas}]'
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get nodeprovisions
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get pods
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get nodeprovisionnetconfigs -o json | jq '[.items[] | {name:.metadata.name,runtimeProfile:.spec.softwareConfig.nodeSoftware.runtimeProfile,certifyRestore:.spec.softwareConfig.nodeSoftware.migrationRuntime.certifyRestore}]'
```

namespace의 기존 TrainingPolicy는 모두 suspended여야 하고 AWS NodeProvision은 없어야 한다. 기존 StatefulSet의 replicas도 0이어야 한다. 원본 trainer가 Pending이어도 replicas가 남으면 새 GPU를 두고 경쟁할 수 있다. 이 조건은 매 run을 새 provisioning부터 측정하기 위한 것이다. 충족하지 않으면 기존 작업의 소유권·복구 완료를 확인하고 그 작업의 정상 종료 절차를 따른다. 가이드에 namespace 전체 삭제, 모든 VM 삭제, 기존 trainer 강제 reset 명령은 없다.

기존 NodeProvisionNetConfig의 runtimeProfile은 StatefulMigration이고 migrationRuntime.certifyRestore는 true여야 한다. false이면 새 노드의 복원 인증이 막힐 수 있다. 기존 검증 릴리스의 runtime package URL/hash, GPU certification, FluidCR payload image를 확인한다. capability 라벨을 수동으로 붙여 인증을 우회하지 않는다. 새 GPU가 준비된 뒤 실제 인증 결과를 확인하는 짧은 preflight이며 전체 플랫폼 검증을 반복하는 절차가 아니다.

checkpoint 실험에는 유효한 기존 `--group-control-image` 설정과 full-group runtime 설치가 필요하다. [Full-group rollout 가이드](full-group-rollout.md)의 기존 설치·검증 기록과 현재 image digest를 확인한다. 새로 전체 시스템 검증을 자동 실행하는 단계가 아니다. render한 policy의 planned-partial=disabled와 fixedOnDemand=1도 확인하고, 설정이나 runtime이 누락되면 실험을 시작하지 않는다. partial 안전 계약을 우회해 대체하지 않는다.

```bash
python3 evaluation/run.py prepare --evidence evidence/r18a01
jq '{phase,workload_uid,policy_uid}' evidence/r18a01/run.json
jq '.' evidence/r18a01/created.json
python3 evaluation/run.py start --evidence evidence/r18a01
```

prepare는 create-only다. 부분 생성 실패 후 created.json을 지우거나 같은 명령을 무작정 재실행하지 않는다. 기록된 객체와 UID를 확인해 scoped cleanup 가능 여부를 판단한다. 이미 존재하는 evidence 디렉터리나 retained PVC와 충돌하면 새 run-id를 사용한다.

start는 전경에서 실행한다. collector를 함께 실행하고 새 worker provisioning과 학습을 시작하며, 목표 또는 max_seconds 제한까지 관찰한다. **rank 0과 rank 1 모두 목표 step의 completed 이벤트가 있어야 성공**이다. rank 0 단독 완료나 Pod Running만으로 완료 판정하지 않는다. 목표 도달한 trainer는 추가 학습 없이 살아 있을 수 있으므로 컨테이너 exit code만으로 완료를 판정하지 않는다.

interruption run은 두 rank 목표 달성 외에도 FIS completed와 실제 대상 stopped/terminated 증거가 필요하다. checkpoint 실험의 group 복구 성공은 해당 실행 소유 groupRestoreRequest 참조가 가리키는 RestoreRequest의 Verified와 교체 전 UID의 NodeProvision 제거로 확인하며, SpotReplacement Completed를 대신 요구하지 않는다. cost B는 기존 partial 경로의 소유 SpotReplacement/SpotRecovery 완료를 확인하고 risk-rise도 소유 recovery 완료가 필요하다. 목표 step이 전환·복구 완료보다 먼저 끝나면 failed로 분류해 성공 비교에서 제외한다. 충분한 goal_steps를 잡고 최종 scenario 증거를 보존한다.

정상 완료·제한 시간·예외 경로에서 runner는 stop을 요청한다. stop은 먼저 suspend하고 최대 180초 동안 해당 workload의 schedule이 disabled, Paused이며 observedGeneration이 현재 generation과 일치하는지, 소유 replacement/recovery/restore 작업이 안전하게 종료됐는지 확인한 뒤에만 replicas 0으로 줄인다. **Failed replacement/recovery는 안전한 종료로 취급하지 않는다.** 시간 초과 시 오류를 반환하고 Pod·VM을 보존하므로 수동 scale 0이나 finalizer patch로 우회하지 않는다. API 오류·프로세스 강제 종료·호스트 장애까지 정지를 보장하지는 않는다. 다른 터미널에서 같은 kubeconfig 환경을 설정하고 아래 stop으로 정지 요청과 결과를 확인할 수 있다.

```bash
python3 evaluation/run.py stop --evidence evidence/r18a01
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingpolicy eval-r18a01 -o json | jq '.metadata.annotations'
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get statefulset eval-r18a01 -o json | jq '.spec.replicas'
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get pods -l app=eval-r18a01
```

pilot 증거를 보존하고 cleanup과 실제 VM 종료를 확인한 뒤 본 실험 설정으로 새 run을 render한다. 예를 들어 cost B는 r18b01, checkpoint D는 r18d01을 쓴다. 아래 두 render는 별도 실험 예시이며 두 run을 동시에 start하지 않는다.

```bash
python3 evaluation/run.py render --config evaluation/config.local.json --run-id r18b01 --experiment cost --arm B --out evidence/r18b01
python3 evaluation/run.py render --config evaluation/config.local.json --run-id r18d01 --experiment checkpoint --arm D --out evidence/r18d01
```

## 6. 실제 interruption이 필요한 평가

첫 constant pilot에서는 FIS를 실행하지 않는다. checkpoint 비교가 실제 선점·복구 증거를 요구할 때 새 run을 scenario=interruption으로 render/prepare/start하고 **별도 터미널**에서 아래 단계를 진행한다. cost A+interruption은 거부되므로 OD baseline은 constant로 실행한다. risk-rise는 위험률 입력 변화이며 실제 interruption과 구분한다.

FIS용 IAM experiment role은 provisioning credentialsRef와 별개다. FIS trust와 action 권한, caller의 template 생성·실행 및 role 전달 권한을 준비한다. 이 도구는 IAM을 만들지 않는다. [AWS FIS IAM role 공식 문서](https://docs.aws.amazon.com/fis/latest/userguide/getting-started-iam-service-role.html)를 따른다.

plan은 policy UID에 연결된 NodeProvision의 running Spot VM **한 개의 정확한 ARN**을 생성한다. action은 `aws:ec2:send-spot-instance-interruptions`, durationBeforeInterruption은 PT2M이다. 실제 동작과 권한은 [AWS FIS action 공식 문서](https://docs.aws.amazon.com/fis/latest/userguide/fis-actions-reference.html#send-spot-instance-interruptions)를 참조한다. plan은 조회·로컬 파일 생성이며 start는 실제 interruption이다.

```bash
export FIS_EVIDENCE=evidence/r18d01
export FIS_ROLE_ARN=arn:aws:iam::REPLACE_ACCOUNT:role/REPLACE_FIS_ROLE
export FIS_ALARM_ARN=arn:aws:cloudwatch:ap-northeast-2:REPLACE_ACCOUNT:alarm:REPLACE_ALARM
python3 evaluation/fis.py plan --evidence "$FIS_EVIDENCE" --role-arn "$FIS_ROLE_ARN" --alarm-arn "$FIS_ALARM_ARN"
jq '.' "$FIS_EVIDENCE/fis-target.json"
jq '{targets,actions,stopConditions,roleArn}' "$FIS_EVIDENCE/fis-template.json"
```

같은 kubeconfig·AWS 계정에서 run-id, NodeProvision UID, instance ID, ARN과 alarm을 확인한다. 모든 비교 arm은 **두 rank의 첫 정상 학습 진행 관측으로부터 사전에 정한 동일 offset**에 시작한다. provisioning 시작 시각과 혼동하지 않는다. 목표 달성 전에 FIS와 복구가 끝날 충분한 goal_steps/max_seconds를 설정한다. offset을 놓쳤다면 동일 조건으로 기록하지 않는다.

```bash
python3 evaluation/fis.py start --evidence "$FIS_EVIDENCE" --confirm r18d01
jq '.' "$FIS_EVIDENCE/fis-created.json"
jq '.' "$FIS_EVIDENCE/fis-started.json"
```

위 블록은 필요할 때만 명시적으로 실행한다. alarm 없는 실험을 선택한 경우에만 plan의 --alarm-arn을 생략하고 start에 --allow-no-alarm을 추가한다. target 변경 시 재확인이 필요하며 fis-started.json이 요청만 기록한 상태에서 실패하면 AWS experiment 상태를 먼저 확인한다. 파일을 지워 재시도하지 않는다. FIS 중단은 이미 전송한 interruption을 되돌리지 않는다. 최종 fis-final.json, fis-instance-final.json, recovery-final.json을 보존하고, 남은 FIS template은 기록된 ID로 별도 관리한다.

## 7. 수집·분석과 실제 비용

start가 collector를 실행하므로 같은 evidence에 두 수집기를 동시에 띄우지 않는다. stopped 상태의 실행에 추가 snapshot이 필요하면 --once를 사용한다. AWS 상태 조회 권한이 있으면 --aws-status를 추가할 수 있다.

```bash
python3 evaluation/collect.py --evidence evidence/r18a01 --once
python3 evaluation/analyze.py evidence/r18a01 --output evidence/r18a01/summary.csv
```

logs의 Pod UID/container/restart별 EVAL_EVENT와 monitor-events.jsonl, snapshots, errors.jsonl, run.json을 함께 확인한다. 두 rank 완료와 scenario 완료가 성공 기준이며 rank 0만 완료하거나 timeout/failed인 결과를 성공 비교에 넣지 않는다. all_rank_completion_observed와 오류·누락도 확인한다. 관측하지 못한 step replay나 restore를 0으로 채우지 않는다. member interval은 이전 checkpoint 완료 후 대기 시간이므로 시작 간격 또는 정확한 iteration trigger와 다르다.

각 실행의 billing.csv 또는 공통 --billing-csv 파일에 실제 가격 원장을 작성한다. 컬럼은 다음과 같다.

```csv
run_id,cost_type,instance_id,start,end,price_per_hour,currency,amount,description
```

vm 행은 실제 instance ID, timezone 있는 과금 start/end와 시간당 가격을 사용한다. Spot 가격 변동은 겹치지 않는 구간으로 나누고 provisioning, 교체 전후 VM, 학습 종료 후 EC2 종료까지 포함한다. extra 행은 EBS/NFS·전송·FIS 등 정의한 범위의 amount를 적는다. 추가 비용 0은 확인한 경우에만 명시한다. 통화·공유비용 배분·가격 출처를 기록한다. 단순 Spot quote나 학습 시간은 실제 청구 원장이 아니다.

```bash
python3 evaluation/analyze.py evidence/r18a01 --billing-csv evidence/billing.csv --output evidence/r18a01/summary.csv
python3 evaluation/analyze.py evidence/r18a01 evidence/r18b01 --billing-csv evidence/billing.csv --baseline-run-id r18a01 --output evidence/cost-comparison.csv
```

두 번째 명령은 A/B를 각각 완료하고 원장을 채운 뒤 실행한다. checkpoint 비교는 F60/F300/F600/D의 해당 evidence 경로를 같은 방식으로 나열한다. billing_status가 missing/invalid이면 비용 비교가 미완성이다. VM/extra 증거 누락을 total_cost=0으로 대체하지 않는다. 실제 bill 대조 없이 최종 청구액 재현이라 주장하지 않는다.

`cost_saving_percent = 100 × (A.total_cost - B.total_cost) / A.total_cost`이며 음수는 비용 증가다. baseline은 입력에 포함된 유일한 cost A 실행이고 비용이 양수여야 한다. A/B 모두 completed, 두 rank 완료 관측, excluded=false, billing_status=validated, currency=USD여야 한다. model, goal_steps, instance_type, batch_size, seed, dataset, world_size, region, training_image가 일치해야 하므로 짧은 pilot A를 본 실험 B의 baseline으로 재사용하지 않는다. `cost_comparison_status=comparable`인지 확인한다. 설정 불일치는 configuration_mismatch, 부적격 실행·원장은 ineligible_run_or_billing으로 표시되고 절감률은 비어 있다. excluded와 exclusion_reasons, issues를 함께 확인하며 빈 값을 0%로 해석하지 않는다.

## 8. 정리와 검증 범위

증거를 보관하고 진행 중 replacement/restore가 끝난 뒤 정확한 run-id로 정리한다. cleanup도 suspend 후 같은 정지 조건을 최대 180초 확인하며, Failed replacement/recovery나 미확인 schedule이 남으면 삭제를 진행하지 않고 Pod·VM을 보존한다. **stop은 VM을 지우지 않으며 cleanup은 정지 확인 후 해당 run의 VM 종료를 요청할 수 있다.**

```bash
python3 evaluation/run.py cleanup --evidence evidence/r18a01 --confirm r18a01
export EVAL_POLICY_UID="$(jq -er '.policy_uid' evidence/r18a01/run.json)"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get nodeprovisions -l "training.dcnlab.com/policy-uid=$EVAL_POLICY_UID"
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get nodeprovisions -l "training.dcnlab.com/policy-uid=$EVAL_POLICY_UID"
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get pvc -l training.dcnlab.com/evaluation-run=r18a01
```

cleanup은 created.json의 객체 UID와 policy UID에 연결된 자식 범위로 제한된다. finalizer 강제 patch나 namespace 전체 삭제를 하지 않으며 dataset PV/PVC와 checkpoint PVC를 보존한다. 삭제 요청 성공과 실제 EC2 종료는 다르므로 기록된 instance ID로 AWS 상태·과금 종료를 확인한다. 다음 run은 잔여 NodeProvision이 없고 이전 정책·workload가 정지된 뒤 prepare한다.

로컬 테스트는 설정·manifest·UID 격리·수집/분석·sampler·이벤트 계약을 확인한다. 실제 이미지 build/push, NFS 접근, CUDA/NCCL 2-rank 학습, FluidCR checkpoint와 실험별 restore(checkpoint는 full-group, cost B는 partial), FIS와 비용은 별도 실환경 증거가 필요하다. source commit, controller/training digest, run-id/UID, 두 rank 완료, scenario 결과, checkpoint source, 누락·오류, 비용 원장과 남은 PV/PVC를 기록한다. 이 문서 작성 단계에서는 클라우드 명령을 실행하지 않는다.
