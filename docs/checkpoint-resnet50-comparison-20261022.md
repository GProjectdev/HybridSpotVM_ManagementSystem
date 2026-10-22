# ResNet-50: 동적 / 빠른 / 느린 Checkpoint 비교

작성일: 2026-10-22. 이 문서를 기본 실행 가이드로 사용한다.
이전 checkpoint-resnet50-guide-20261022.md의 F600 제안 대신 F300/F1200/D를 사용한다.
빠른/느린은 학습 속도가 아니라 Checkpoint 빈도를 뜻한다.

## 0. 무엇을 비교하는가

| arm | 의미 | 설정 주기 |
|---|---|---|
| F300 | 빠른 고정 저장 | 300초 |
| F1200 | 느린 고정 저장 | 1200초 |
| D | 제안 방식 | 분석식 기반 60~3600초 |

모든 arm은 ResNet-50, batch 32, OD 1 + Spot 1, 동일 이미지/코드/seed,
22000 step, 동일 위험률 시간표와 동일 FIS offset을 사용한다.
기존 ResNet-50 관측 간격은 약 10분30초였으므로 이를 사이에 둔 비교 후보다.
최적 주기라는 뜻은 아니다. 설정값과 실제 발생 간격이 일치하는지 먼저 확인한다.
네트워크 제한에 의한 처리 속도 변화와 Checkpoint 정지는 분리해서 보고한다.

시간 기준은 VM 생성이나 Pod Running이 아니라 all_ranks_progress_observed이다.

| 경과 시간 | 동작 |
|---|---|
| 시작 | 위험률 0.05/hour |
| 600초 (10분) | 위험률 0.8/hour 자동 요청 |
| 1500초 (25분) | 별도 승인한 FIS 스케줄러가 통지 요청 |
| 약 1620초 (27분) | FIS PT2M 설정에 따른 실제 Spot 중단 |
| 1800초 (30분) | 위험률 0.05/hour 자동 요청 |

위험률은 확률 퍼센트나 선점 명령이 아니다. FIS와 독립적인 입력이다.
fixedOnDemand=1을 유지하므로 위험률 변경 자체로 OD 수를 늘려 비교를 깨뜨리지 않는다.
복구 중에는 임시 VM 중복이 있을 수 있다.

22000 step은 시작 후보다. 0.14초/step이면 계산만 약51분, 0.30초면 약110분이다.
Checkpoint/준비/복구는 별도 추가된다. max_seconds=14400은 준비 포함 상한이며 자동 과금 종료가 아니다.
사전 실행에서 길이가 부족하면 본 비교 전에 모든 arm을 동일하게 늘린다.

## 1. MGMT: 업데이트와 배포 확인

실행 위치: root@mgmt, /root/hybridspot-validation/System.

```bash
export ROOT=/root/hybridspot-validation
cd "$ROOT/System"
git status --short
git pull --ff-only origin restore-automation-20260928
python3 evaluation/checkpoint.py render --help
python3 evaluation/checkpoint_suite.py --help
python3 -m unittest discover -s evaluation -p 'test_*.py' -q
export NS=fluidcr-realign-121040
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get pods,nodeprovisions
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get trainingpolicies
```

pull이 로컬 변경 때문에 실패하면 덮어쓰거나 reset하지 않는다.
기존 실행 정리와 EC2 종료를 먼저 확인한다.
이번 변경은 MGMT Python 도구이며 새 모델/컨트롤러 이미지 빌드를 요구하지 않는다.
단, fixedOnDemand와 full-group runtime, coordinator group-control-image,
실제 Spot 통지 수신과 긴급 fallback은 이미 배포되어 있어야 한다.
소스 push만으로 원격 배포를 확인했다고 간주하지 않는다.
[Full-group 배포](full-group-rollout.md), [긴급 fallback](emergency-group-fallback-20261019.md)를 참조한다.

## 2. MGMT: 설정 생성

기존에 실행한 ResNet-50 환경을 재사용한다. 다른 모델 이미지를 대신 넣지 않는다.

```bash
jq '{model,training_image,seed,batch_size,nfs_dataset_path}' evaluation/config.resnet50.full.next.local.json
python3 evaluation/checkpoint_suite.py configure \
  --base evaluation/config.resnet50.full.next.local.json \
  --out evaluation/config.r50.ckcompare.local.json

python3 evaluation/checkpoint_suite.py configure \
  --base evaluation/config.resnet50.full.next.local.json \
  --out evaluation/config.r50.ckpilot.local.json \
  --scenario risk-schedule
```

파일이 존재하면 안전하게 실패한다. 다른 이름을 사용하거나 내용을 확인한다.
configure는 클라우드 작업을 하지 않는다.
본 비교는 scenario=interruption과 risk_schedule을 함께 사용한다.
pilot은 위험률 변경만 수행하며 FIS를 절대 실행하지 않는다.

## 3. 먼저 사전 검증

본 비교 전에 F300/F1200에서 실제 저장 간격, D에서 분석식 적용을 확인한다.
아직 검증하지 않은 arm은 아래 pilot 절차로 실행한다. pilot 자료는 본 비교에서 제외한다.
F1200은 완료된 정기 저장 최소 2회, 가능하면 3회로 간격을 확인한다.
Checkpoint가 저장 시간 때문에 밀린 경우와 정책/전파가 잘못된 경우를 구분한다.
목표 완료 이전에 자료가 부족하면 이를 검증 완료라고 쓰지 않는다.

```bash
export RUN=r50ckp300 ARM=F300 NS=fluidcr-realign-121040
python3 evaluation/checkpoint.py render \
  --config evaluation/config.r50.ckpilot.local.json \
  --run-id "$RUN" --arm "$ARM" --out "evidence/checkpoint/$RUN"
jq '{policy:.spec.policy,checkpoint:.spec.checkpoint,annotations:.metadata.annotations}' "evidence/checkpoint/$RUN/policy.yaml"
python3 evaluation/checkpoint.py prepare --evidence "evidence/checkpoint/$RUN"
python3 evaluation/checkpoint.py start --evidence "evidence/checkpoint/$RUN"
```

다음 pilot은 cleanup 후 RUN=r50ckp1200 ARM=F1200, RUN=r50ckpd ARM=D로 실행한다.
F300은 min=max=300, F1200은 min=max=1200, D는 60~3600을 확인한다.

별도 MGMT 터미널(같은 디렉터리, 같은 RUN/NS)에서:

```bash
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get trainingpolicy "eval-$RUN" -o json |
  jq '{policy:.status.policy,checkpoint:.status.checkpoint}'
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get fluidcrmigrations -o json |
  jq --arg p "eval-$RUN-periodic" '.items[] | select(.metadata.name | startswith($p)) |
    {name:.metadata.name,schedule:.spec.schedule,status:.status}'
jq '{phase,analytic_interval_observed,risk_schedule_observations,failure_reason}' "evidence/checkpoint/$RUN/run.json"
```

D는 checkpoint-cost-analytic 출처와 analytic_interval_observed=true가 필요하다.
Bootstrap만 유지되면 본 실험 중단: 측정 전달/배포/스케줄러 문제를 먼저 해결한다.
기존 약10분 관측만으로 F300/F1200 실제 적용을 보장할 수 없다.
수집 snapshots에서 startTime을 정렬해 실제 간격을 확인한다.
정기/긴급 저장을 혼합하지 않는다. 계획된 위험률 변경 두 건도 consumed여야 한다.
아래 7단계로 보존/정리한 뒤 본 실험에 진입한다.

## 4. 본 실험: MGMT 터미널 A

각 실행은 순차적으로 한다. 다음 실행 전에 cleanup과 EC2 종료를 확인한다.

| RUN | ARM |
|---|---|
| r50ckfast01 | F300 |
| r50ckprop01 | D |
| r50ckslow01 | F1200 |

```bash
cd /root/hybridspot-validation/System
export RUN=r50ckfast01 ARM=F300 NS=fluidcr-realign-121040
python3 evaluation/checkpoint.py render \
  --config evaluation/config.r50.ckcompare.local.json \
  --run-id "$RUN" --arm "$ARM" --out "evidence/checkpoint/$RUN"
jq '{run_id,arm,scenario,model,goal_steps,initial_risk,risk_schedule}' "evidence/checkpoint/$RUN/run.json"
python3 evaluation/checkpoint.py prepare --evidence "evidence/checkpoint/$RUN"
python3 evaluation/checkpoint.py start --evidence "evidence/checkpoint/$RUN"
```

start가 계속 실행되는 동안 다음 터미널 B를 사용한다.
위험률 변경은 start 내부에서 자동 실행된다. 수동 risk 명령을 중복 호출하지 않는다.
고정 arm도 같은 위험률 입력을 받지만 주기는 고정이며 D만 주기를 조정한다.
위험률 변경이 반드시 D의 숫자 변화를 보장하지는 않는다. clipping과 측정값을 함께 보고한다.

## 5. MGMT 터미널 B: FIS 계획과 명시적 실행 승인

FIS는 실제 VM을 중단한다. IAM 준비가 안 됐으면 본 실행 전에 준비한다.
AWS FIS experiment role(신뢰 principal fis.amazonaws.com 및 대상 작업 권한)과
호출자의 FIS/iam:PassRole, EC2 조회 권한은 provisioning credential과 별개다.
기존 승인된 역할과 CloudWatch stop alarm ARN을 사용한다. 도구는 IAM을 생성하지 않는다.
[공식 IAM 준비](https://docs.aws.amazon.com/fis/latest/userguide/getting-started-iam-service-role.html)
및 [Spot 튜토리얼](https://docs.aws.amazon.com/fis/latest/userguide/fis-tutorial-spot-interruptions.html)을 따른다.

터미널 A에서 phase=running이 되고 Spot VM 한 대가 running인 이후:
```bash
cd /root/hybridspot-validation/System
export RUN=r50ckfast01 NS=fluidcr-realign-121040
export FIS_ROLE_ARN='실제 승인된 FIS 역할 ARN'
export FIS_ALARM_ARN='실제 CloudWatch stop alarm ARN'
aws sts get-caller-identity
python3 evaluation/fis.py plan --evidence "evidence/checkpoint/$RUN" \
  --role-arn "$FIS_ROLE_ARN" --alarm-arn "$FIS_ALARM_ARN"
cat "evidence/checkpoint/$RUN/fis-target.json"
cat "evidence/checkpoint/$RUN/fis-template.json"
```

plan은 무중단이며 해당 policy UID의 Spot VM 정확히 한 대를 선택한다.
대상 ARN/계정/실행 ID를 확인한다. 같은 RUN에서 스케줄러를 두 개 실행하지 않는다.
새 실행마다 새 plan을 만든다. 이전 실행의 파일을 복사하지 않는다.

다음 명령은 25분 시점에 실제 중단 통지를 보내도록 승인한다.
학습 시작 전후부터 실행해 대기시켜도 되지만 phase=running이어야 한다.

```bash
python3 evaluation/checkpoint_suite.py interrupt \
  --evidence "evidence/checkpoint/$RUN" --confirm "$RUN" \
  --after-seconds 1500 --tolerance-seconds 30 --wait-seconds 7200
```

목표 시점을 30초 넘겨 놓치면 통지를 보내지 않고 실패한다. 시점을 임의로 늦추지 않는다.
이미 fis-started.json이 있으면 재시도하지 않는다. 불명확한 API 응답은 AWS에서 확인한다.
통지 시점에 대상이 바뀌었거나 실행이 끝났으면 기존 fis.py의 재검증으로 거부한다.
stop-experiment로 이미 보낸 선점을 취소할 수 없다.
알람 없는 실행은 기본 거부된다. 별도 위험 검토한 격리 환경에서만
plan의 --alarm-arn을 생략하고 interrupt에 --allow-no-alarm을 명시한다.

선점 직전 복구 가능한 전체 그룹 Checkpoint가 없거나 예상치 못한 자연 선점이 발생하면
비교 조건 위반으로 기록한다. 예정 시점을 유리하게 바꿔 성공 실행으로 포함하지 않는다.
자연 선점/경쟁 작업이 의심되면 스케줄러를 Ctrl+C로 중지하고 FIS 상태를 확인한다.

## 6. 선점과 복구 해석

FIS action은 PT2M이다. 위험률 변화와 실제 선점은 별도 이벤트다.
긴급 Checkpoint가 완료되면 해당 snapshot, 실패하면 배포된 fallback 경로가 선택한
이전 완료된 동일 그룹 snapshot을 사용했는지 실제 checkpointRef로 확인한다.
rank별 latest 파일을 섞어 동기화됐다고 가정하지 않는다.

긴급 저장이 늘 성공하면 정기 주기별 rollback 차이가 작을 수 있다.
이를 그대로 결과로 보고한다. 무통지 즉시 종료는 다른 실험이며 이 결과와 혼합하지 않는다.
Full-group 복구를 검증하므로 이전 비용 평가의 SpotReplacement Completed만으로 판단하지 않는다.
runner는 FIS completed, 실제 중단, group RestoreRequest Verified, 이전 노드 제거를 검증한다.

## 7. MGMT: 결과 저장과 정리

```bash
tail -n 20 "evidence/checkpoint/$RUN/runner-events.jsonl"
jq '{phase,failure_reason,analytic_interval_observed,risk_schedule_observations}' "evidence/checkpoint/$RUN/run.json"
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get trainingpolicy "eval-$RUN" -o json > "evidence/checkpoint/$RUN/policy-final.json"
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get restorerequests -o json > "evidence/checkpoint/$RUN/restores-final.json"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get fluidcrmigrations,restoreplans -o json > "evidence/checkpoint/$RUN/member-final.json"
tar -czf "$RUN-before-cleanup.tar.gz" -C evidence/checkpoint "$RUN"
python3 evaluation/checkpoint.py cleanup --evidence "evidence/checkpoint/$RUN" --confirm "$RUN"
tar -czf "$RUN-evidence.tar.gz" -C evidence/checkpoint "$RUN"
```

실패 실행은 먼저 증거를 저장한 뒤:
```bash
python3 evaluation/checkpoint.py abort --evidence "evidence/checkpoint/$RUN" --confirm "$RUN"
python3 evaluation/checkpoint.py cleanup --evidence "evidence/checkpoint/$RUN" --confirm "$RUN"
```
active recovery 오류가 있으면 강제 finalizer 제거 없이 상태를 확인한다.
AWS EC2의 실제 종료까지 확인한다. PVC/PV는 보존된다.

## 8. 분석

```bash
python3 evaluation/analyze.py \
  evidence/checkpoint/r50ckfast01 evidence/checkpoint/r50ckprop01 evidence/checkpoint/r50ckslow01 \
  --output evidence/checkpoint/r50-comparison.csv
```

설정 주기/실제 간격, 정기/긴급 저장 횟수, 저장/정지/복구 시간, rollback step,
목표 완료 시간, 위험률 요청/소비 시각, FIS 요청/실제 종료 시각을 보고한다.
없는 측정은 0으로 채우지 않는다. 단회는 탐색 결과이고 반복 시 순서를 교차한다.
서로 다른 step의 시간이나 임의 보정 시간을 동일 실측으로 비교하지 않는다.

AWS Master의 network_probe.py를 병행할 수 있다. 교체 VM은 새 IP/키와 새 파일에 수집한다.
GPU 호스트 도구가 없어도 네트워크 수집은 가능하다. 누락 구간을 표시한다.
네트워크 버스트 소진이 학습 속도를 바꾸므로 Checkpoint 자체 영향과 분리한다.
원격 FIS/IAM/배포/학습은 로컬 테스트로 검증됐다고 주장하지 않는다.
