# 1번째 평가: 짧은 복원 상태 검증

## 범위

기존 90분 비용 실험을 반복하지 않는다. 기존 cost B의 위험률 기반 교체 경로를
사용하여 교체 직후 상태와 이후 업데이트를 점검한다. 실제 Spot 강제 종료는 하지 않는다.
기존 결과는 보존하고 이 실행의 시간/비용을 본 평가에 섞지 않는다.

검증 모드는 기본 OFF이며 config의 `restore_audit: true`일 때만 활성화된다.
학습 스크립트와 검증 모듈은 기존 ConfigMap으로 배포되므로 새 이미지 빌드나
controller 교체는 필요 없다. 기존 학습 이미지와 배포된 FluidCR를 그대로 사용한다.

검사 내용:
- optimizer 생성 후, DDP 초기화 전에 저장된 checkpoint와 모델/optimizer/RNG/Step 비교.
- 복원된 프로세스의 다음 3개 업데이트에서 유한하고 0이 아닌 gradient 및 가중치 변화 확인.
- 입력과 파라미터의 SHA256 기록. GPU tensor는 CPU로 옮겨 dtype/shape/value를 비교한다.
- 불일치하면 이벤트를 출력하고 학습 프로세스를 실패시킨다. 조용히 통과시키지 않는다.

한계: 이는 rank-local 복원 상태 검사다. survivor 전체의 상태, DDP gradient all-reduce의
정확성, 입력 순서 전체, 비복원 기준 실행과의 전체 수치 동등성을 증명하지 않는다.
통과하면 복원 및 후속 업데이트의 직접 증거가 보강되지만 "완전한 학습 동등성 입증"으로
표현하지 않는다. 실패하면 결과와 오류를 보존하고 다음 모델로 넘어가지 않는다.

## 1. MGMT: 코드 업데이트

```bash
cd /root/hybridspot-validation/System
git status --short
git pull --ff-only origin restore-automation-20260928
```

추적 중인 파일의 로컬 수정 때문에 pull이 거절되면 중단한다. reset으로 지우지 않는다.
기존 config.*.local.json은 덮어쓰지 않는다.

## 2. MGMT: GPU VM 생성 전 CPU smoke test

기존 Buildah와 기존 학습 이미지를 사용한다. GPU/새 EC2는 필요 없다.
아래 명령을 한 단계씩 실행하고 실패하면 다음 단계로 진행하지 않는다.

```bash
export AUDIT_IMAGE=docker.io/jeongseungjun/hybrid-spot-vm-system:resnet18_eval_v1
AUDIT_CTR=$(buildah from "$AUDIT_IMAGE")
buildah copy "$AUDIT_CTR" evaluation/resnet18/restore_audit.py /workspace/restore_audit.py
buildah copy "$AUDIT_CTR" evaluation/resnet18/test_restore_audit.py /workspace/test_restore_audit.py
buildah run "$AUDIT_CTR" -- python /workspace/test_restore_audit.py
buildah rm "$AUDIT_CTR"
```

기대 결과: 실제 PyTorch로 2개 테스트 OK. SGD와 AdamW 각각 상태 로드 후 3개
업데이트가 비복원 작은 모델과 일치하고, 상태 변경을 불일치로 검출한다.
이 테스트는 배포된 FluidCR 자체나 NCCL 테스트가 아니다. 테스트 실패 시에도
생성한 로컬 작업 컨테이너만 buildah rm으로 정리한다.

## 3. MGMT: ResNet-18 검증 설정

활성 실행과 이전 EC2가 정리된 상태에서 시작한다. 아래 30,000 Step은 조기 종료를
피하기 위한 여유 목표다. 끝까지 기다리는 목표가 아니며, 검증/교체 완료 후 수동 종료한다.
max_seconds=7200은 초기 준비를 포함한 안전 제한이지 예상 실행 시간은 아니다.

```bash
export RUN=r18audit01
jq '. + {model:"resnet18",restore_audit:true,goal_steps:30000,
 max_seconds:7200,scenario:"risk-schedule",initial_risk:0.05,
 risk_schedule:[{after_seconds:180,lambda_per_hour:0.8}]}' \
 evaluation/config.cost.full.local.json > evaluation/config.r18.audit.local.json
python3 evaluation/cost.py render \
 --config evaluation/config.r18.audit.local.json --run-id "$RUN" \
 --arm B --out "evidence/cost/$RUN"
jq '.items[] | select(.kind=="StatefulSet") |
 .spec.template.spec.containers[0].env[] | select(.name=="EVAL_RESTORE_AUDIT")' \
 "evidence/cost/$RUN/karmada.yaml"
python3 evaluation/cost.py prepare --evidence "evidence/cost/$RUN"
python3 evaluation/cost.py start --evidence "evidence/cost/$RUN"
```

EVAL_RESTORE_AUDIT=1을 확인하고 prepare한다. run-id는 매번 새 이름을 쓴다.
위험률 변경은 모든 rank의 학습 관측 후 3분이다. VM 준비/교체/복원 시간은 별도다.

## 4. MGMT 별도 터미널: 확인 및 증거 보존

```bash
cd /root/hybridspot-validation/System
export RUN=r18audit01 NS=fluidcr-realign-121040
python3 evaluation/audit_restore.py "evidence/cost/$RUN"
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" \
 get spotreplacements,restorerequests,spotrecoveries -o json |
 jq --arg prefix "eval-$RUN-" '{items:[.items[] |
 select(.metadata.name | startswith($prefix))]}' \
 > "evidence/cost/$RUN/recovery-final.json"
jq '.items[] | {kind,name:.metadata.name,phase:.status.phase}' \
 "evidence/cost/$RUN/recovery-final.json"
```

audit 명령의 exit code 2는 실패 또는 증거 부족이다. 초기 실행 중에는 정상적으로
증거가 부족할 수 있다. `failures`가 있으면 학습 로그를 확인한다.

종료 전 다음을 모두 확인한다:
1. worker-00/01 두 SpotReplacement가 Completed.
2. 두 RestoreRequest가 Verified, 두 SpotRecovery가 Completed.
3. rank_local_audit_passed=true.
4. summary의 양 rank에 해당 교체 후 생성된 attempt가 포함되어 있고, 각 attempt의
   three_updates_passed=true. 이전 periodic 복원만 통과한 결과로 종료하지 않는다.

현재 Pod의 attempt는 다음 로그의 restored/restore_audit와 summary를 대조한다.

```bash
for rank in 0 1; do
 kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" \
 logs "eval-$RUN-$rank" -c trainer --timestamps \
 > "evidence/cost/$RUN/rank-$rank-before-stop.log"
done
python3 evaluation/audit_restore.py "evidence/cost/$RUN" \
 > "evidence/cost/$RUN/restore-audit-summary.json"
```

## 5. 종료와 정리

검증 및 모든 교체가 끝났으면 start를 실행한 첫 터미널에서 Ctrl+C를 한 번 누른다.
runner의 finally가 stop과 collector 종료를 처리한다. 중간 종료이므로 run phase는
failed/interrupted로 남을 수 있다. 이것을 completed로 수정하지 않는다.
감사 통과와 목표 Step 완료는 다른 결과다. start 프로세스가 반환될 때까지 기다린다.

```bash
python3 evaluation/cost.py cleanup --evidence "evidence/cost/$RUN" --confirm "$RUN"
tar -czf "$RUN-evidence.tar.gz" -C evidence/cost "$RUN"
```

stop/cleanup이 active operation을 이유로 거부하면 강제 삭제/finalizer 제거를 하지 말고
오류와 recovery-final.json을 전달한다. EC2 종료까지 확인한 후 다음 실행으로 넘어간다.
실패한 검증은 진단 후 필요할 때만 기존 abort 절차를 사용한다.

## 6. GPT-2: ResNet 감사 결과 확인 후

동일 절차로 AdamW도 확인한다. 데이터/모델 경로와 batch/sequence_length는 기존 설정을
그대로 보존한다. 기존 이미지가 가진 PyTorch에서도 2절 smoke test를 먼저 실행한다.

```bash
export RUN=gptaudit01
jq '. + {model:"gpt2",restore_audit:true,goal_steps:4000,
 max_seconds:7200,scenario:"risk-schedule",initial_risk:0.05,
 risk_schedule:[{after_seconds:180,lambda_per_hour:0.8}]}' \
 evaluation/config.gpt2.full.local.json > evaluation/config.gpt2.audit.local.json
python3 evaluation/cost.py render \
 --config evaluation/config.gpt2.audit.local.json --run-id "$RUN" \
 --arm B --out "evidence/cost/$RUN"
python3 evaluation/cost.py prepare --evidence "evidence/cost/$RUN"
python3 evaluation/cost.py start --evidence "evidence/cost/$RUN"
```

4~5절에서 RUN=gptaudit01로 변경한다. 4,000 Step 완료까지 기다리지 않고 위 종료 기준을
충족하면 정리한다. 만약 목표 Step이 먼저 끝나면 복원 검증 성공으로 간주하지 않는다.
두 증거 압축파일을 기존 평가결과자료들 폴더로 전달한다.
