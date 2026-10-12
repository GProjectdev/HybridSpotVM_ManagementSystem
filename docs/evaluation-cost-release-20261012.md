# 비용 평가 A/B 재시작 가이드

## 1. 변경된 조건

- A: OD 2개, TrainingPolicy annotation training.dcnlab.com/periodic-checkpoint=disabled로 정기 Checkpoint를 명시적으로 비활성화한다.
- 앞서 안내한 2시간 주기 및 최초 지연 방식은 폐기했다. 실행 시간이 2시간을 넘어도 정기 Checkpoint를 만들지 않는다.
- A는 위험률 변경과 시간표 완료 검사를 하지 않는다. renderer가 scenario를 constant로 바꾼다.
- B: 최소 OD 강제 없음, 위험률 기반 VM 구성, 분석식 기반 Checkpoint 주기(60~3600초).
- 3600초는 위험률 입력의 1시간 변경 시나리오를 고려한 실험 상한이며 논문이 강제하는 값은 아니다. 위험률 갱신과 Checkpoint 실행은 동기화되지 않는다.
- 상한 변경은 renderer와 문서 변경뿐이다. eval.1.3 coordinator를 배포했다면 추가 이미지 빌드는 필요 없다. 새 run-id로 render하여 반영한다.
- B는 초기 측정값이 없을 때 bootstrap 주기를 사용한다. 이후 checkpoint-cost-analytic 관측이 있어야 성공이다.
- A에서 Checkpoint 리소스가 관측되거나 비활성화 적용 확인이 없으면 성공으로 인정하지 않는다.
- annotation은 해당 정책의 정기 작업만 비활성화한다. B와 긴급/교체용 Checkpoint 경로는 그대로 유지한다.
- 이미 정기 스케줄이 있다면 pause를 요청하고 멤버의 확인을 기다린다. 진행 중인 작업을 강제 중단하지 않는다.
- 기존 평가용 policy-manager/CRD는 유지한다. 이번에는 checkpoint-coordinator만 새로 빌드한다.

## 2. 코드 받기 및 테스트

이전 실행의 EC2 종료를 확인한다. 기존 evidence와 로컬 JSON 설정은 보존한다.
아래 명령은 MGMT의 git 저장소에서 실행한다. git pull이 충돌하면 파일을 강제로 덮어쓰지 않는다.

```bash
cd /root/hybridspot-validation/System
git pull --ff-only origin restore-automation-20260928
python3 -m unittest discover -s evaluation -p 'test_*.py'
go test -p 2 -mod=readonly ./internal/management ./internal/policy
```

## 3. Buildah 빌드 및 배포

정책 관리자, 학습 이미지, Stateful Migration 이미지는 이번 변경 때문에 재빌드할 필요 없다.
아래 명령은 한 단계가 성공한 뒤 다음 단계를 실행한다.

```bash
export COORD_IMAGE=docker.io/jeongseungjun/hybrid-spot-vm-system:checkpoint_coordinator_v3.0-eval.1.3
buildah login docker.io
buildah bud --arch amd64 --build-arg COMPONENT=checkpoint-coordinator -f Dockerfile -t "$COORD_IMAGE" .
buildah push "$COORD_IMAGE" "docker://$COORD_IMAGE"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system set image deployment/checkpoint-coordinator "manager=${COORD_IMAGE:?}"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system rollout status deployment/checkpoint-coordinator --timeout=180s
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system get deployment checkpoint-coordinator -o jsonpath='{.spec.template.spec.containers[*].image}'
```

## 4. 입력 및 실행

evaluation/config.cost.local.json의 NFS, 이미지와 AWS 설정은 유지한다.
goal_steps=30000, max_seconds=10800, scenario=risk-schedule,
initial_risk=0.05, risk_schedule=[{"after_seconds":3600,"lambda_per_hour":0.8}]로 설정한다.
90분 이내 완료는 예상일 뿐 보장하지 않는다. A와 B의 목표 Step은 동일하게 유지한다.
B가 60분 전에 끝나거나 교체 완료 전에 끝나면 시나리오 조건 미충족으로 실패할 수 있다.
180분 제한은 학습 목표 시간이 아니다. 기존 2시간 지연용 evidence는 재사용하지 말고 새 ID로 render한다.

아래 ID가 이미 사용됐다면 새 ID로 바꾼다. 각 명령의 성공을 확인하고 다음 명령을 실행한다.

```bash
python3 evaluation/cost.py render --config evaluation/config.cost.local.json --run-id costa04 --arm A --out evidence/cost/costa04
jq '{annotations:.metadata.annotations,checkpoint:.spec.checkpoint}' evidence/cost/costa04/policy.yaml
jq '{scenario,goal_steps}' evidence/cost/costa04/run.json
python3 evaluation/cost.py prepare --evidence evidence/cost/costa04
python3 evaluation/cost.py start --evidence evidence/cost/costa04
```

다른 터미널에서 아래를 확인한다. A의 policy-latest.json에 status.checkpoint.reason=periodic_disabled가 보여야 한다.
policy.yaml의 checkpoint 간격 필드는 비활성화 중에는 실행에 사용되지 않는다.

```bash
tail -n 20 -F evidence/cost/costa04/runner-events.jsonl
# 별도 조회
jq '.status.checkpoint' evidence/cost/costa04/policy-latest.json
```

완료 후 run.json의 phase와 실제 EC2 종료까지 확인한다. cleanup은 삭제 요청이며 종료 확인을 대신하지 않는다.

```bash
jq '{phase,failure_reason,goal_steps}' evidence/cost/costa04/run.json
python3 evaluation/cost.py cleanup --evidence evidence/cost/costa04 --confirm costa04
```

A 정리가 완료되면 동일 설정으로 B를 실행한다.

```bash
python3 evaluation/cost.py render --config evaluation/config.cost.local.json --run-id costb02 --arm B --out evidence/cost/costb02
python3 evaluation/cost.py prepare --evidence evidence/cost/costb02
python3 evaluation/cost.py start --evidence evidence/cost/costb02
```

B 종료 후 run.json의 analytic_interval_observed=true, phase=completed를 확인한다.
실행 중 주기 출처는 policy-latest.json의 status.checkpoint.intervalSource로 확인한다.

```bash
jq '{phase,failure_reason,analytic_interval_observed}' evidence/cost/costb02/run.json
python3 evaluation/cost.py cleanup --evidence evidence/cost/costb02 --confirm costb02
```

실제 비용은 VM 종료까지 집계한다. 이전 300초 고정 A/B 결과와 이번 결과를 같은 실험으로 혼합하지 않는다.
실제 AWS/GPU 실행과 이미지 push/배포는 이 가이드에 따라 별도 검증해야 한다.
