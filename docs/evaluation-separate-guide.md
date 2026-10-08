# 두 평가의 독립 실행 가이드

두 평가는 동시에 실행하지 않는다. namespace는 fluidcr-realign-121040으로 유지하고
설정 파일, run-id, 증거 디렉터리와 결과 표를 분리한다.
이번 변경은 스크립트와 문서 변경이므로 추가 이미지 재빌드는 필요 없다.
주기 평가에는 이전 평가용 fixedOnDemand 지원 CRD와 컨트롤러가 필요하다.

## 1. 공통 준비

[설치 가이드](evaluation-resnet18-guide.md)의 1~3단계로 이미지, kubeconfig,
policy-source.json, 실제 NFS 경로를 준비한다. 값이 채워진 config.local.json에서 분리한다.

~~~bash
cp -n evaluation/config.local.json evaluation/config.cost.local.json
cp -n evaluation/config.local.json evaluation/config.checkpoint.local.json
~~~

policy_source는 같은 policy-source.json을 참조해도 된다.
제공한 baseline AWS 값은 evaluation/policy-source.baseline.yaml에 반영했다.
기존 policy-source.json이 있으면 먼저 백업하고, 아래 명령으로 복사한다.
이 파일은 renderer 입력용이며 직접 kubectl apply하지 않는다.

~~~bash
cp -n evaluation/policy-source.json evaluation/policy-source.before-schedule.json
cp evaluation/policy-source.baseline.yaml evaluation/policy-source.json
~~~

기존 파일이 없다면 첫 백업 명령은 생략한다.
AMI, subnet, VPC, 보안 그룹, aws-node-credentials 참조는 제공한 값을 사용하며
원본에 없는 iamInstanceProfile은 넣지 않았다. 실제 리소스의 유효성과 권한은 별도 확인한다.
모델 ResNet18, CIFAR-10, g5.xlarge 2개를 사용한다.
같은 평가 안에서 이미지, seed, batch_size, goal_steps를 동일하게 유지한다.
평가 간 목표 Step은 달라도 되지만 서로 다른 평가의 결과를 직접 비교하지 않는다.

## 2. 비용 평가

config.cost.local.json에 scenario=risk-schedule, initial_risk=0.05를 설정한다.
risk_schedule에 시간표를 지정한다. 시간은 두 Rank의 학습 진행 관측 이후 경과 초다.
예: [{"after_seconds":1200,"lambda_per_hour":0.8},
{"after_seconds":2400,"lambda_per_hour":0.05},
{"after_seconds":3600,"lambda_per_hour":0.8}]
이때 changed_risk와 risk_change_after_seconds는 사용하지 않는다.
20분 간격은 예시이며 VM 준비·복구 및 이후 학습 시간을 측정하여 구간을 정한다.
동일한 시간표를 A/B에 적용하고 실제 요청 시각과 정책 수신, 결정값을 기록한다.
마지막 변경 이후에도 전환 완료와 학습 관측 시간이 확보되도록 실행 길이를 정한다.
시간표가 끝나기 전에 학습이 끝나거나 이전 위험도 입력이 다음 시점까지 처리되지 않으면 실패 처리한다.
전환이 진행 중이어도 시간표를 임의로 늦추지는 않으므로 겹치는 전환이 없도록 구간을 충분히 잡는다.
goal_steps는 위험도 변경, VM 준비, 복구 완료까지 충분하도록 정한다.
max_seconds에는 초기 VM 생성 시간도 포함한다.

- A: minOnDemand=2, 목표 worker=2이므로 OD 2개를 유지한다.
- B: minOnDemand=0, fixedOnDemand 없음. OD 최소 개수를 강제하지 않으며 All-Spot도 허용한다.
- 위험도가 낮다고 반드시 All-Spot이 된다고 가정하지 않는다. 실제 정책 결정과 VM 구성을 기록한다.
- 위험도 변화는 실제 선점 발생과 다르다. All-Spot 동시 선점 복구를 검증했다는 뜻은 아니다.
- 양쪽 모두 Checkpoint 300초 고정이다. 여기서는 FIS나 주기 평가를 실행하지 않는다.

먼저 A만 실행한다. 각 명령의 성공을 확인한 뒤 다음 명령을 실행한다.

~~~bash
python3 evaluation/cost.py render --config evaluation/config.cost.local.json --run-id costa01 --arm A --out evidence/cost/costa01
python3 evaluation/cost.py prepare --evidence evidence/cost/costa01
python3 evaluation/cost.py start --evidence evidence/cost/costa01
~~~

완료 증거를 보존한 뒤 해당 실행의 VM 정리를 요청한다.

~~~bash
python3 evaluation/cost.py cleanup --evidence evidence/cost/costa01 --confirm costa01
~~~

Karmada와 AWS 양쪽의 해당 NodeProvision 제거 및 실제 EC2 종료를 확인한다.
정리 완료 전에는 다음 prepare를 실행하지 않는다. PVC와 데이터는 보존된다.
이후 같은 config로 B를 실행한다.

~~~bash
python3 evaluation/cost.py render --config evaluation/config.cost.local.json --run-id costb01 --arm B --out evidence/cost/costb01
python3 evaluation/cost.py prepare --evidence evidence/cost/costb01
python3 evaluation/cost.py start --evidence evidence/cost/costb01
python3 evaluation/cost.py cleanup --evidence evidence/cost/costb01 --confirm costb01
~~~

실패 시 강제 삭제하지 않는다. 기존 가이드 7단계의 billing.csv 형식으로 실제 VM 과금,
생성·Join·복구 시간과 중복 과금 및 부대비용을 기록한다.

~~~bash
python3 evaluation/analyze.py evidence/cost/costa01 evidence/cost/costb01 --billing-csv evidence/cost/billing.csv --baseline-run-id costa01 --output evidence/cost/comparison.csv
~~~

총비용, 비용 절감률, 학습 완료 시간을 비교한다.
짧은 파일럿을 본 실험 baseline으로 사용하거나 실패 실행을 성공으로 포함하지 않는다.

## 3. 주기 평가

비용 평가의 정리가 끝난 뒤 진행한다.
config.checkpoint.local.json은 scenario=interruption으로 설정한다.
initial_risk는 모든 비교 arm에서 동일하게 하고 실제 선점은 FIS로 주입한다.
위험도 입력과 실제 선점 사건은 다르다. 여러 위험도를 평가하려면 위험도별로
고정 주기와 동적 주기를 같은 조건으로 다시 실행한다.

- F60/F300/F600: 각각 60/300/600초 고정 주기.
- D: 측정값 기반 분석식으로 60~600초 범위에서 주기를 계산한다.
- 이 평가에서만 fixedOnDemand=1로 OD 1개·Spot 1개를 유지한다.
- 모든 arm은 같은 full-group 복구 경로를 사용한다. Partial Restore 방식 비교가 아니다.

기존 설치 가이드의 full-group runtime과 group-control-image 준비가 필요하다.
먼저 F300을 실행한다.

~~~bash
python3 evaluation/checkpoint.py render --config evaluation/config.checkpoint.local.json --run-id ckf30001 --arm F300 --out evidence/checkpoint/ckf30001
python3 evaluation/checkpoint.py prepare --evidence evidence/checkpoint/ckf30001
python3 evaluation/checkpoint.py start --evidence evidence/checkpoint/ckf30001
~~~

start 실행 중 별도 터미널에서 기존 가이드 6단계의 FIS plan/start를 수행한다.
FIS_EVIDENCE=evidence/checkpoint/ckf30001, confirm=ckf30001을 사용한다.
FIS는 실제 Spot VM을 중단하며 자동으로 시작되지 않는다.
모든 arm에서 두 Rank 학습 시작 관측 기준 동일한 시간 offset에 주입하고 시각을 기록한다.
목표 Step에 도달하기 전에 선점과 복구를 완료할 충분한 실행 길이가 필요하다.

완료 후 정리한다.

~~~bash
python3 evaluation/checkpoint.py cleanup --evidence evidence/checkpoint/ckf30001 --confirm ckf30001
~~~

실제 EC2 종료까지 확인한 뒤 아래 arm을 하나씩 같은 절차로 실행한다.
각 실행마다 새 FIS 계획을 만들고 render/prepare/start/cleanup의 경로를 모두 변경한다.

| arm | run-id | evidence |
| --- | --- | --- |
| F60 | ckf60r01 | evidence/checkpoint/ckf60r01 |
| F600 | ckf600r01 | evidence/checkpoint/ckf600r01 |
| D | ckd01 | evidence/checkpoint/ckd01 |

~~~bash
python3 evaluation/analyze.py evidence/checkpoint/ckf60r01 evidence/checkpoint/ckf30001 evidence/checkpoint/ckf600r01 evidence/checkpoint/ckd01 --output evidence/checkpoint/comparison.csv
~~~

학습 완료 시간, Checkpoint 부담, 재수행 Step을 확인한다.
관측되지 않은 값은 0으로 간주하지 않는다. D가 bootstrap만 사용했다면 분석식 평가로 인정하지 않는다.
반복 실험은 새 run-id로 진행하고 실행 순서를 교차하여 기록한다.

## 4. 안전 경계와 검증

cost.py와 checkpoint.py는 다른 평가의 evidence를 받으면 클러스터 작업 전에 거부한다.
기존 run.py는 호환성을 위해 유지한다. 일반 실행은 위 전용 명령을 사용한다.
prepare의 기존 활성 workload, 정책, NodeProvision 검사도 유지한다.
이 검사는 분산 잠금이 아니므로 두 명령을 병렬로 실행하지 않는다.
collect.py, analyze.py, fis.py는 공통 도구이며 현재 평가의 evidence를 명시한다.
실제 GPU 학습, 복구, FIS 및 과금 결과는 로컬 테스트와 별도로 검증해야 한다.
