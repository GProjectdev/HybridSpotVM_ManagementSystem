# GPT-2 Checkpoint 추가 실험

작성일: 2026-10-28. 로컬 설정/가이드만 작성했으며 원격 배포 및 FIS 실행은 하지 않았다.

## 계획과 목표 Step

F300/F1200/D를 GPT-2 small에서 비교한다. 위험률은 기존 0.05 → 0.8회/시간(학습 진행 감지 +60분), FIS 요청은 +80분, PT2M 그대로 유지한다. 정상 구성은 OD 1대 + Spot 1대, 모델 내 batch=4/rank, sequence length=512, seed=42로 고정한다. 모델 변경에 필요한 데이터/학습률/batch도 GPT-2용으로 바뀌므로 모델 외 모든 텐서 설정이 ResNet과 동일하다는 의미는 아니다. 세 정책 사이에서는 동일하다.

목표 10,000 Step. 과거 GPT-2 빠른 구간 0.672초/Step이면 순수 계산 약 112분, 느린 구간 1.654초/Step이면 약 276분이다. 준비·Checkpoint·복구를 더하면 길어진다. max_seconds=28,800(8시간)은 안전 상한이지 목표 실행시간이 아니다. timeout/학습 종료 후에도 실제 EC2 종료 확인이 필요하다.

사전 smoke run으로 메모리/데이터/학습속도/저장소 여유를 확인한다. 측정상 전체 계산이 100분 미만(약 0.60초/Step 미만)이면 본 실험 전 모든 정책의 목표를 함께 재설정한다. 본 실험 도중 특정 arm의 목표나 선점시각만 변경하지 않는다. 사전 실행은 본 결과에서 분리한다.

1차 F300→F1200→D, 2차 F1200→D→F300, 3차 D→F300→F1200. 최소 각 3회, 순차 실행. 동일 source policy, 코드 commit, 모든 controller/payload/training 이미지 digest를 고정한다. v5.0 배포와 실험을 섞지 말고 세 정책을 같은 검증된 시스템으로 실행한다.

## 0. 파일 배치와 사전 확인

로컬 생성 설정은 `../evaluation/config.gpt2.ck10k-risk60-fis80.json`이다. MGMT의 `/root/hybridspot-validation/System/evaluation/`에 같은 이름으로 전달한다. 서버에서는 restore-automation-20260928 브랜치에서 git pull --ff-only 후 설정 파일의 존재를 확인한다. 기존 서버 `evaluation/policy-source.json`을 그대로 사용하며 예제 AMI/네트워크 설정으로 덮어쓰지 않는다.

```bash
cd /root/hybridspot-validation/System
: "${AWS_KUBECONFIG:?}" "${KARMADA_KUBECONFIG:?}"
test -s evaluation/config.gpt2.ck10k-risk60-fis80.json
test -s evaluation/policy-source.json
git rev-parse HEAD
git status --short
kubectl --kubeconfig="$AWS_KUBECONFIG" -n fluidcr-realign-121040 get nodeprovisions,statefulsets,pods
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n fluidcr-realign-121040 get trainingpolicies
```

다른 실행이 남아 있으면 중복 실험하지 않는다. 전체 namespace 삭제 금지. NFS 서버에서 위 설정의 모델/토큰 데이터 경로 존재, 가용 공간, 실제 training image 접근 가능 여부를 확인한다. PVC Bound는 파일 존재/데이터 정합성 증거가 아니다. 10Gi가 실제 파일 보존량에 충분한지 확인하며 NFS quota 적용 여부도 별도 확인한다.

`checkpoint_suite.py configure`는 현재 model=resnet50, batch=32를 강제하므로 사용하지 않는다. interrupt 하위 명령만 사용한다.

## 1. 실행별 render와 prepare

```bash
cd /root/hybridspot-validation/System
export NS=fluidcr-realign-121040
export CONFIG="$PWD/evaluation/config.gpt2.ck10k-risk60-fis80.json"
export ARM=F300
case "$ARM" in
  F300) PREFIX=g3 ;;
  F1200) PREFIX=g12 ;;
  D) PREFIX=gd ;;
  *) echo '잘못된 ARM'; exit 1 ;;
esac
export RUN="${PREFIX}-$(date -u +%m%d%H%M%S)"
export EVIDENCE="$PWD/evidence/checkpoint/$RUN"
python3 evaluation/checkpoint.py render --config "$CONFIG" --run-id "$RUN" --arm "$ARM" --out "$EVIDENCE"
jq '{run_id,arm,model,goal_steps,batch_size,sequence_length,risk_schedule}' "$EVIDENCE/run.json"
jq '{capacity:.spec.capacity.mode,workers:.spec.expectedWorldSize,policy:.spec.policy,checkpoint:.spec.checkpoint}' "$EVIDENCE/policy.yaml"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply --dry-run=server -f "$EVIDENCE/policy.yaml"
python3 evaluation/checkpoint.py prepare --evidence "$EVIDENCE"
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get pvc "eval-$RUN-data" "eval-$RUN-checkpoint"
printf 'RUN=%s\nEVIDENCE=%s\n' "$RUN" "$EVIDENCE"
```

model=gpt2, goal_steps=10000, workers=2, fixedOnDemand=1을 확인한다. 주기 하한/상한은 F300=300/300, F1200=1200/1200, D=60/3600초다. D의 300초 riskBand는 초기 설정이며 실행 중 analytic 주기 선택을 별도로 확인한다.

## 2. 터미널 A: 학습 시작

```bash
python3 evaluation/checkpoint.py start --evidence "$EVIDENCE"
```

이 프로세스를 유지한다. 별도 터미널에는 RUN, EVIDENCE, kubeconfig 환경변수를 다시 설정한다. 아래 명령은 기존 실험의 FIS 실행 절차를 따른다.

## 3. 터미널 B: 이번 Spot에 대한 FIS 준비

RUN은 1단계에서 출력된 값을 사용한다. 다른 run의 값을 복사하지 않는다.

```bash
cd /root/hybridspot-validation/System
export RUN=여기에_이번_RUN
export EVIDENCE="$PWD/evidence/checkpoint/$RUN"
export ROLE=HybridSpotFIS-r50ckpreempt01
export FIS_ROLE_ARN="arn:aws:iam::767397782724:role/$ROLE"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n fluidcr-realign-121040 get nodeprovisions \
  -o custom-columns='NAME:.metadata.name,MARKET:.spec.marketType,PHASE:.status.phase,INSTANCE:.status.instanceId'
```

이번 worker-01의 Spot instance ID가 생성된 뒤 아래 블록을 실행한다. 공용 Role의 기존 정책을 바꾸므로 다른 FIS 실행과 동시에 사용하지 않는다.

```bash
(
  set -euo pipefail
  python3 evaluation/fis.py plan --evidence "$EVIDENCE" --role-arn "$FIS_ROLE_ARN"
  cat "$EVIDENCE/fis-target.json"
  jq '{targets,actions,stopConditions}' "$EVIDENCE/fis-template.json"
  aws iam get-role-policy --role-name "$ROLE" --policy-name InterruptOnlyThisRun \
    --query PolicyDocument --output json > "$EVIDENCE/fis-role-policy-before.json"
  TARGET_ARN=$(jq -er '.targets.oneSpot.resourceArns | select(length == 1) | .[0]' "$EVIDENCE/fis-template.json")
  jq -n --arg arn "$TARGET_ARN" '{Version:"2012-10-17",Statement:[{Effect:"Allow",Action:"ec2:SendSpotInstanceInterruptions",Resource:$arn}]}' \
    > "$EVIDENCE/fis-role-policy-current.json"
  aws iam put-role-policy --role-name "$ROLE" --policy-name InterruptOnlyThisRun \
    --policy-document "file://$EVIDENCE/fis-role-policy-current.json"
  aws iam get-role-policy --role-name "$ROLE" --policy-name InterruptOnlyThisRun --query PolicyDocument
)
```

IAM 업데이트는 대상 1개로 제한한다. 실제 권한에는 다른 deny/role 설정도 영향을 줄 수 있으므로 성공을 보장하지 않는다. role policy 변경 직후 실행하지 않고 80분 예약 전 충분히 준비한다. Alarm 없는 기존 방식은 자동 stop condition이 없으므로 격리 실험에서만 명시적으로 허용한다. 새 실험에서 Alarm 정책을 바꿀 경우 모든 arm에 동일 적용한다.

```bash
python3 evaluation/checkpoint_suite.py interrupt \
  --evidence "$EVIDENCE" --confirm "$RUN" \
  --after-seconds 4800 --tolerance-seconds 30 \
  --wait-seconds 7200 --allow-no-alarm
```

이 프로세스를 유지한다. start만 실행하면 선점이 자동 예약되지 않는다. 시간 기준은 VM 생성 시작이 아니라 all_ranks_progress_observed다. 예약 창을 놓치거나 FIS 실패 시 그 실행을 실패로 보존하고 임의 시각 재주입 결과를 정상 비교에 섞지 않는다. 템플릿 대상이 현재 run의 Spot인지 확인한다.

## 4. 관측과 종료

```bash
tail -n 30 -F "$EVIDENCE/runner-events.jsonl"
```

별도 터미널에서 필요 시 `python3 evaluation/collect.py --evidence "$EVIDENCE" --interval 15 --aws-status`를 실행한다. 동일 evidence에 collector를 중복 실행하지 않는다. D는 실제 decision의 intervalSource/intervalCostEvaluated/checkpointIntervalSeconds를 확인한다. bootstrap-only면 동적 정책 검증으로 집계하지 않는다.

FIS 요청 뒤 fis-started.json의 experiment.id로 `aws fis get-experiment --region ap-northeast-2 --id ...`를 확인한다. requested만으로 선점 성공 판정하지 않는다. actual interruption, RestoreRequest/Plan, resumed step, rollback, 전체 rank 10000 Step 완료를 확인한다.

종료 후 collector도 중지하고 증거를 먼저 압축한다. 예시:

```bash
tar -czf "/root/${RUN}-completed.tar.gz" -C "$(dirname "$EVIDENCE")" "$RUN"
tar -tzf "/root/${RUN}-completed.tar.gz" >/dev/null
sha256sum "/root/${RUN}-completed.tar.gz"
```

압축 전 대상 run의 NodeProvision 및 instance ID 목록을 별도 보존한다. 정상 완료를 확인한 경우에만 아래 run 범위 cleanup을 수행한다. 실패 run은 상태를 확인한 뒤 abort 절차를 사용하며 증거를 먼저 보존한다.

```bash
python3 evaluation/checkpoint.py cleanup --evidence "$EVIDENCE" --confirm "$RUN"
```

cleanup 요청 후 보존한 모든 원본/대체 instance ID의 EC2 terminated, NodeProvision/Pod 정리를 확인한다. VMs still bill 경고를 무시하지 않는다. 보존한 PVC/PV/Checkpoint는 다음 run에 재사용하지 않고 고유 run ID로 진행한다. 전체 namespace 또는 이름 패턴으로 일괄 VM 삭제하지 않는다.

## 결과 판정

주 지표는 동일 Step 완료 실측 시간, 보조 지표는 초기 포함 시간, 정기/긴급 CP 횟수, 실제 정지 시간, 파일 크기, rollback, provisioning/restore 구간이다. 느린 run을 임의로 제외하거나 D 승리를 가정하지 않는다. 동일 trace 한 번의 시나리오이며 일반적인 선점 분포에서의 최적성을 증명하지 않는다. 각 3회 평균/중앙값과 개별 결과를 함께 보고하고 변동이 크면 반복을 늘린다.
