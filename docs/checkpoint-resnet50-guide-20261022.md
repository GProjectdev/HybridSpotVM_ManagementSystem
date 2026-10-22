# ResNet-50 Checkpoint 주기 평가 (2026-10-22)

> 최신 실행 절차는 [빠른/느린/동적 비교 가이드](checkpoint-resnet50-comparison-20261022.md)를 따른다.
> 아래 F300/F600 구성은 초기 제안이며 최신 F300/F1200/D 및 결합 위험률/FIS 절차로 대체한다.

## 실험 조건

- OD 1대 + Spot 1대 고정, full-group 복구. 비용 A/B와 분리한다.
- 우선 F300(300초), F600(600초), D(분석식 60~3600초)를 비교한다. F60은 선택적인 스트레스 조건이다.
- 모델 크기만으로 최적 주기를 정할 수 없다. 사전 실행에서 Checkpoint 완료/정지 시간과 실제 시작 간격을 확인한 뒤 후보를 확정한다.
- 현재 CLI는 F60/F300/F600/D만 지원한다. 임의 F900 또는 config interval 추가는 지원하지 않는다.
- 설정 주기와 실제 발생 간격을 별도로 보고한다. 300초 내 완료가 어렵다면 F300 비교 전에 스케줄 동작부터 점검한다.
- 이미지 digest, 코드, seed, batch, goal_steps, 위험률, 선점 offset은 arm 간 동일하게 유지한다.
- fixedOnDemand CRD/컨트롤러, full-group runtime, coordinator group-control-image 및 Spot 통지 수신 경로가 배포되어야 한다.
- 관련 설치는 full-group-rollout.md, emergency-group-fallback-20261019.md를 참조한다. push만으로 원격 배포를 가정하지 않는다.

## 1. MGMT 설정 생성

기존 ResNet-50 이미지와 데이터를 재사용한다. 이 문서 때문에 새 이미지를 만들 필요는 없다.
기존 실행 정리와 EC2 종료를 확인한다. 아래 파일은 이미 존재하면 덮어쓰지 않는다.

```bash
cd /root/hybridspot-validation/System
export NS=fluidcr-realign-121040
git rev-parse HEAD
jq '{model,training_image,batch_size,goal_steps}' evaluation/config.resnet50.full.next.local.json
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get pods,nodeprovisions
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get trainingpolicies
python3 - <<'PY'
import json
from pathlib import Path
c = json.loads(Path('evaluation/config.resnet50.full.next.local.json').read_text())
c.update(model='resnet50', batch_size=32, restore_audit=False, initial_risk=0.8)
c.pop('risk_schedule', None)
for name, scenario, steps, limit in [('preflight','constant',12000,7200), ('full','interruption',22000,10800)]:
    v = dict(c, scenario=scenario, goal_steps=steps, max_seconds=limit)
    with Path(f'evaluation/config.r50.checkpoint.{name}.local.json').open('x') as f:
        json.dump(v, f, indent=2)
        f.write('\n')
PY
```

22000 step은 기존 측정에 근거한 후보이지 완료 시간 보장이 아니다. 0.14초/step이면 계산 약 51분,
0.30초이면 약 110분이며 저장/복구/준비 시간이 추가된다. max_seconds에는 초기 준비도 포함된다.
본 비교 전 사전 실행 결과로 길이를 확정하고 모든 arm에 동일하게 적용한다.

## 2. MGMT: D 사전 실행, FIS 없음

```bash
export RUN=r50ckpre01
python3 evaluation/checkpoint.py render --config evaluation/config.r50.checkpoint.preflight.local.json --run-id "$RUN" --arm D --out "evidence/checkpoint/$RUN"
jq '{annotations:.metadata.annotations,policy:.spec.policy,checkpoint:.spec.checkpoint}' "evidence/checkpoint/$RUN/policy.yaml"
python3 evaluation/checkpoint.py prepare --evidence "evidence/checkpoint/$RUN"
python3 evaluation/checkpoint.py start --evidence "evidence/checkpoint/$RUN"
```

별도 MGMT 터미널에서 같은 경로로 이동하고 RUN/NS를 다시 지정한다.

```bash
export RUN=r50ckpre01 NS=fluidcr-realign-121040
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get trainingpolicy "eval-$RUN" -o json | jq '{policy:.status.policy,checkpoint:.status.checkpoint}'
jq '{phase,analytic_interval_observed,failure_reason}' "evidence/checkpoint/$RUN/run.json"
```

통과 조건: 정기 Checkpoint 실제 완료/학습 재개, checkpoint-cost-analytic 출처,
analytic_interval_observed=true, 두 rank 목표 완료. Bootstrap만 관측되면 본 비교를 시작하지 않는다.
snapshots/monitor-events에서 실제 간격과 정지 시간을 확인한다. 아래 보존/cleanup 후 본 실행한다.

## 3. MGMT: 본 실행

r50ckf30001/F300, r50ckd01/D, r50ckf60001/F600 순으로 각각 독립 실행한다.
각 실행 종료/cleanup/EC2 종료 후 다음 실행으로 넘어간다.

```bash
cd /root/hybridspot-validation/System
export RUN=r50ckf30001 ARM=F300 NS=fluidcr-realign-121040
python3 evaluation/checkpoint.py render --config evaluation/config.r50.checkpoint.full.local.json --run-id "$RUN" --arm "$ARM" --out "evidence/checkpoint/$RUN"
jq '{run_id,experiment,arm,model,goal_steps,scenario,initial_risk}' "evidence/checkpoint/$RUN/run.json"
python3 evaluation/checkpoint.py prepare --evidence "evidence/checkpoint/$RUN"
python3 evaluation/checkpoint.py start --evidence "evidence/checkpoint/$RUN"
```

## 4. 별도 MGMT 터미널: Spot 선점

위험률 0.8은 시간당 입력이며 선점 명령이 아니다. AWS FIS로 실제 Spot 한 대를 중단한다.
현재 fis.py는 aws:ec2:send-spot-instance-interruptions, PT2M을 사용한다.
통지 후 약 2분 뒤 실제 중단되며 Pod 삭제로 대체하지 않는다.
FIS 실험 역할은 provisioning credentials와 별개다. AWS 공식 튜토리얼로 FIS trust/action 권한,
caller의 FIS/PassRole 권한과 승인된 중지 알람을 먼저 준비한다. 도구는 IAM을 만들지 않는다.

```bash
cd /root/hybridspot-validation/System
export RUN=r50ckf30001 NS=fluidcr-realign-121040
# 실제 승인된 ARN으로 바꾼다.
export FIS_ROLE_ARN='실제-FIS-역할-ARN'
export FIS_ALARM_ARN='실제-CloudWatch-중지-알람-ARN'
aws sts get-caller-identity
python3 evaluation/fis.py plan --evidence "evidence/checkpoint/$RUN" --role-arn "$FIS_ROLE_ARN" --alarm-arn "$FIS_ALARM_ARN"
cat "evidence/checkpoint/$RUN/fis-target.json"
cat "evidence/checkpoint/$RUN/fis-template.json"
```

plan은 통지를 보내지 않는다. 이번 policy UID 소유 Spot 한 대의 정확한 ARN인지 확인한다.
통지 요청 offset 후보는 all_ranks_progress_observed 이후 1200초(20분)이다.
사전 실행에서 이 시점 전에 복구 가능한 full-group Checkpoint가 완료되는지 확인한다.
그렇지 않으면 본 비교 전 공통 offset/step을 늘린다. arm마다 저장 완료에 맞춰 선점을 늦추지 않는다.

```bash
python3 - <<'PY'
import json, os
from pathlib import Path
from datetime import datetime, timezone
p = Path('evidence/checkpoint') / os.environ['RUN'] / 'runner-events.jsonl'
rows = [json.loads(x) for x in p.read_text().splitlines() if x.strip()]
e = next((x for x in rows if x['event']=='all_ranks_progress_observed'), None)
if e:
    t = datetime.fromisoformat(e['timestamp'].replace('Z','+00:00'))
    print('elapsed_seconds:', (datetime.now(timezone.utc)-t).total_seconds())
else:
    print('Training not observed; do not inject.')
PY
```

정해진 offset에 한 번만 실행한다. 실제 요청은 interruption_requested에 저장된다.
수동 지연을 기록하고 비교 허용 오차를 사전 정의한다.

```bash
python3 evaluation/fis.py start --evidence "evidence/checkpoint/$RUN" --confirm "$RUN"
```

실제 VM이 중단된다. FIS 중지로 이미 전송한 통지를 취소할 수 없다.
알람 없는 실행은 기본 거부된다. 격리 환경 위험 검토 후에만 plan의 --alarm-arn을 생략하고
start의 --allow-no-alarm을 사용한다. 시작 응답 불명확 시 재시도 전에 AWS 상태부터 확인한다.

긴급 Checkpoint 성공과 실패 후 이전 완료 그룹 snapshot 복구를 구분한다.
rank별 latest.pt를 임의로 섞는 것이 아니라 동일 그룹/세대의 검증된 snapshot을 사용해야 한다.
긴급 저장이 매번 성공하면 정기 주기별 rollback 차이가 작을 수 있다. 이를 그대로 보고한다.
무통지 강제 종료/긴급 저장 실패는 별도 실험 조건이며 임의로 혼합하지 않는다.
통지 수신, 긴급 저장 결과, 복구 checkpointID를 보존한다.

## 5. MGMT: 결과 보존과 cleanup

```bash
tail -n 20 "evidence/checkpoint/$RUN/runner-events.jsonl"
jq '{phase,failure_reason,analytic_interval_observed}' "evidence/checkpoint/$RUN/run.json"
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n "$NS" get trainingpolicy "eval-$RUN" -o json > "evidence/checkpoint/$RUN/policy-final.json"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n "$NS" get fluidcrmigrations,restoreplans -o json > "evidence/checkpoint/$RUN/member-final.json"
tar -czf "$RUN-before-cleanup.tar.gz" -C evidence/checkpoint "$RUN"
python3 evaluation/checkpoint.py cleanup --evidence "evidence/checkpoint/$RUN" --confirm "$RUN"
tar -czf "$RUN-evidence.tar.gz" -C evidence/checkpoint "$RUN"
```

runner는 FIS completed, 대상 stopped/terminated, 해당 groupRestoreRequest의 Verified 및
이전 NodeProvision UID 제거를 검사한다. D는 분석식 관측도 필요하다.
실패/active recovery가 있으면 증거 보존 후 abort 절차를 확인한다. finalizer 강제 제거 금지.
EC2 실제 종료까지 확인한다. PVC/PV는 보존된다.

## 분석과 한계

설정/계산 주기, 실제 간격, 정기/긴급 횟수, 저장/정지/복구 시간, rollback step,
목표 완료 시간과 실제 선점 offset을 비교한다. 단회는 탐색 결과이며 반복 시 순서를 교차한다.
기존 network_probe.py를 AWS Master에서 병행한다. 교체 VM은 새 IP/SSH 키로 새 파일에 수집하고
누락 구간을 표시한다. 호스트 nvidia-smi 부재가 네트워크 수집 실패는 아니다.
Checkpoint 정지와 네트워크 제한은 구분하고 실측 시간을 임의 보정하지 않는다.

## 공식 자료

- https://docs.aws.amazon.com/fis/latest/userguide/fis-tutorial-spot-interruptions.html
- https://docs.aws.amazon.com/fis/latest/userguide/fis-actions-reference.html
- https://docs.aws.amazon.com/fis/latest/userguide/getting-started-iam-service-role.html

현재 run.py와 fis.py CLI/검증 경로에 맞춰 작성했다. 원격 배포/IAM/FIS 실행은 수행하지 않았다.
