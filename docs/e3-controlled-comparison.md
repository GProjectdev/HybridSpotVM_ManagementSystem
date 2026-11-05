# E3: Spot 사용과 위험률 제어의 제한된 비교 실험

이 문서는 2026-11-05 10:00~18:00 KST의 실행 가이드다.
클라우드 자원을 자동으로 시작하지 않는다. Karmada는 변경하지 않는다.
현재 스크립트는 기존 평가 환경(aws 멤버, ap-northeast-2c,
fluidcr-realign-121040 네임스페이스)에 한정된다.

## 주장과 비교

| Arm | 구성 | 체크포인트 | 의미 |
| --- | --- | --- | --- |
| A | 2 On-Demand 고정 | 300초 고정 | 비용/진행량 기준 |
| B | 1 On-Demand + 1 Spot 고정 | A와 동일 | A 대비 Spot 사용의 비용/중단 절충 |
| C | 초기 B와 동일, 위험률 기반 변경 | 측정비용 기반 적응형 | B 대비 통합 제어의 추가 효과 |

동일 모델·데이터·GPU·배치·seed·이미지·스토리지, 매번 새 RUN/PVC를 사용한다.
C는 구성 변경과 체크포인트 제어의 **통합 효과**다. 개별 기능의 기여도는 분리하지 않는다.
C가 더 좋을 것이라고 전제하지 않는다. 더 비싸거나 진행량이 작아도 그대로 보고한다.

[Parcae NSDI24](https://www.usenix.org/conference/nsdi24/presentation/duan)는
선점 환경의 처리량과 견고성을 함께 비교하는 근거다. 본 실험이 Parcae의
trace/모델/규모를 재현한다거나 논문에서 FIS를 사용했다고 주장하지 않는다.
[AWS FIS 문서](https://docs.aws.amazon.com/fis/latest/userguide/fis-actions-reference.html)의
Spot interruption action을 사용하며 요청 성공과 실제 선점/복원 완료를 구분한다.

## 설정 근거와 제한

- 측정 창 90분: 30분 파일럿 + 3 x 90분 = 5시간이라는 예산 제약이다. 표준 벤치마크 시간이 아니다.
- 시작점: 양 rank의 양수 step을 실행기가 처음 관측한 시각. 프로비저닝은 별도 최대 30분이다.
- 장애/복원 중에도 90분 시계는 멈추지 않는다. goal_steps는 충분히 크게 두며 조기 도달은 실패다.
- 후보 시간표: 30분에 위험률 상승, 50분에 FIS 요청, 약 52분 이후 선점, 나머지 약 38분 관찰.
  E2에서 관찰한 약 15분 복구와 비교해 여유를 둔 운영 후보이지 최적값은 아니다.
- alpha=0.8, horizon=3600초. 구현식 floor(-ln(alpha)/(lambda*horizon_hours))에 따라
  한 Spot을 유지할 경계는 약 0.2231/hour다. 0.05 -> 0.8/hour는 이 경계를 가로지르는 합성 입력이다.
  alpha는 여기서 생존확률 기준이며 위험확률 80%라는 뜻이 아니다.
- 고정 300초는 E2에서 사용한 운영 기준이다. 최적 고정 주기라고 주장하지 않는다.
- 파일럿에서 실제 체크포인트 게시 완료·분석식 선택·구성 전환·복원 시간을 확인한 뒤 본 계획을 동결한다.
  30분으로 확인이 부족하면 본 실험을 억지로 승인하지 않는다.
- C가 FIS 이전에 Spot을 제거하면 동일한 위험 노출 기회에서 no_spot_exposure로 기록한다.
  이것은 B와 동일 횟수의 실제 장애를 받은 비교가 아니다. OD를 대신 종료하지 않는다.
- FIS는 체크포인트 완료를 기다리지 않는다. 지연 허용치 120초는 API/관측 지연 한계이며
  실제 요청/동작 시각을 함께 보고한다. 허용치를 넘기면 늦게 주입하지 않는다.
- 위험 입력과 선점이 정렬된 단일 사례이므로 예측 정확도나 일반 AWS 선점분포를 검증하지 않는다.
  n=1/arm, 단일 모델·두 rank: 통계적 일반화/보편적 우월성/논문 게재 보장은 불가하다.

## 오늘 준비

서버에서 기존 사용 브랜치를 갱신한다. 파일럿 후속 수정은 [E3 수정 및 배포 가이드](e3-pilot-fixes.md)에 따라 policy-manager와 checkpoint-coordinator 이미지를 새로 빌드하고 교체한다.
FluidCR payload v3.1.2와 Stateful/PV 컨트롤러 및 Karmada는 유지한다. 기존 local 설정 파일은 덮어쓰지 않는다.

```bash
cd /root/hybridspot-validation/System
git fetch origin
git merge --ff-only origin/restore-automation-20260928
python3 -m unittest discover -s evaluation -p 'test_e3*.py'
cp evaluation/config.gpt2.example.json evaluation/e3-config.local.json
cp evaluation/e3-plan.example.json evaluation/e3-plan.local.json
```

e3-config.local.json의 storage_class/NFS 경로/이미지/instance_type을 실제 E2 설정과 대조한다.
policy-source.json에는 현재 AWS AMI/subnet/security group/credentialsRef가 있어야 한다.
이 파일과 kubeconfig/credentials는 GitHub나 발표 압축본에 넣지 않는다.
기존 E2 run.json을 통째로 config로 복사하지 않는다(과거 UID가 섞일 수 있다).

사전 조건:
- Python3.10+, kubectl, AWS CLI, jq; 정확한 KARMADA_KUBECONFIG와 AWS_KUBECONFIG.
- NFS 데이터/모델 읽기와 새 체크포인트 경로 쓰기, GPU 드라이버/device plugin.
- 두 OD g5.xlarge에 필요한 8 vCPU **잔여** quota. C 전환 중 임시 중복 생성 수량도 파일럿 확인.
- 사용하지 않는 onpre1 GPU VM은 중지 상태 유지. 다른 평가가 같은 네임스페이스에서 실행되면 안 됨.
- FIS trust가 설정된 역할, scoped IAM policy 생성/삭제·simulate·PassRole·FIS·CloudWatch 권한.
- EC2 StatusCheckFailed 지표가 실제 도착해야 한다. ALARM/INSUFFICIENT_DATA를 강제로 OK로 바꾸지 않는다.
- 서버 시간 동기화, SSH 단절에도 유지되는 tmux/screen 세션, 종료 담당자.

## 파일럿: 30분, FIS 없음

```bash
export RUN="e3p-$(date -u +%m%d%H%M%S)"
export EVIDENCE="$PWD/evidence/e3/$RUN"
python3 evaluation/e3_experiment.py render \
  --config evaluation/e3-config.local.json \
  --policy-source evaluation/policy-source.json \
  --plan evaluation/e3-plan.local.json \
  --arm C --pilot --run-id "$RUN" --out "$EVIDENCE"
python3 evaluation/e3_experiment.py preflight --evidence "$EVIDENCE" --confirm "$RUN"
jq '.spec | {policy,checkpoint}' "$EVIDENCE/policy.yaml"
python3 evaluation/e3_experiment.py prepare --evidence "$EVIDENCE" --confirm "$RUN"
python3 -u evaluation/e3_experiment.py start --evidence "$EVIDENCE" --confirm "$RUN"
```

--pilot는 학습 관측 후 10분에 위험률을 올리고 30분간 관찰한다. FIS를 절대 실행하지 않는다.
본 실험과 별개로 저장한다. 비용과 종료 절차는 본 실험과 동일하다.

```bash
jq '{phase,measurement_started_at,measurement_ended_at,analytic_interval_observed,risk_schedule_observations}' "$EVIDENCE/run.json"
jq '.status | {policy,checkpoint}' "$EVIDENCE/policy-latest.json"
jq . "$EVIDENCE/e3-final.json"
python3 evaluation/e3_report.py --evidence "$EVIDENCE"
python3 evaluation/e3_experiment.py cleanup --evidence "$EVIDENCE" --confirm "$RUN"
```

실제 EC2 종료와 2OD 전환/복원/분석식 선택 증거를 확인한다.
단순 phase=measured는 성공 판정이 아니다. C가 구성 전환하지 못하면 통합 제어 효과를 주장할 수 없다.
확인한 파일럿 RUN과 시간/결과를 e3-plan.local.json의 pilot_evidence에 기록하고
approved_for_measurement를 true로 변경한다. 근거가 부족하면 false를 유지한다.
A/B/C는 **같은 동결 계획 파일**을 쓴다. run.json에 SHA-256이 저장된다.
승인 후 한 arm의 결과를 보고 주기/위험률/장애 시점을 바꾸지 않는다.

## 본 실험 A, B, C

아래를 A -> B -> C 순서로 실행하되 매번 새 RUN을 만든다.
순서 효과를 통제하지 못한 한계를 기록한다. 각 실행 사이 실제 EC2 종료를 확인한다.

```bash
export ARM=A
export RUN="e3a-$(date -u +%m%d%H%M%S)"
export EVIDENCE="$PWD/evidence/e3/$RUN"
python3 evaluation/e3_experiment.py render \
  --config evaluation/e3-config.local.json \
  --policy-source evaluation/policy-source.json \
  --plan evaluation/e3-plan.local.json \
  --arm "$ARM" --run-id "$RUN" --out "$EVIDENCE"
python3 evaluation/e3_experiment.py prepare --evidence "$EVIDENCE" --confirm "$RUN"
python3 -u evaluation/e3_experiment.py start --evidence "$EVIDENCE" --confirm "$RUN"
```

B는 ARM=B/RUN 접두사 e3b-, C는 ARM=C/e3c-로 변경한다.
실행 터미널을 유지한다. e2_schedule.py를 추가 실행하지 않는다.
E3 start 자체가 위험률/선점 시각을 관리한다.

### B/C에서만: 다른 터미널에서 FIS 설정

새 RUN/EVIDENCE를 정확히 넣는다. 초기 Spot VM이 running이 된 후,
위험률 상승 전(학습 시작 후 30분 이내)에 실행한다.
이 명령은 run별 IAM inline policy와 OD 건강 알람을 생성하고 대상을 고정한다.
권한 검증이 실패하면 원인을 해결하고 같은 명령으로 다시 설정할 수 있지만
이미 선점 요청된 RUN을 재사용하지 않는다.

```bash
cd /root/hybridspot-validation/System
export RUN="위에서_출력한_실제_RUN"
export EVIDENCE="$PWD/evidence/e3/$RUN"
python3 evaluation/e3_fis_setup.py \
  --evidence "$EVIDENCE" --confirm "$RUN" \
  --role-name HybridSpotFIS-r50ckpreempt01
jq . "$EVIDENCE/e3-authorization.json"
aws cloudwatch describe-alarms --region ap-northeast-2 \
  --alarm-names "$(jq -r .alarm_name "$EVIDENCE/e3-fis-setup.json")" \
  --query 'MetricAlarms[].{State:StateValue,Reason:StateReason}'
```

A와 파일럿에는 이 명령을 실행하지 않는다.
알람은 OD 감시용이고 선점 대상은 Spot이다. 과거 RUN의 instance ARN을 재사용하지 않는다.
FIS 설정/알람 미완료, 초기 구성 불일치, 목표 VM 교체, 예약시간 초과면 실행기는 실패로 중단한다.
실패를 삭제하거나 임의 늦은 선점으로 성공 결과처럼 바꾸지 않는다.

### 관측

```bash
tail -n 20 -F "$EVIDENCE/runner-events.jsonl"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n fluidcr-realign-121040 \
  get nodeprovisions,pods -o wide
```

runner-events의 all_ranks_progress_observed가 시작 기준이다.
e3-fault.json의 requested_not_yet_verified는 요청만 했다는 뜻이다.
실제 성공은 e3-final.json의 FIS 상태, 대상 EC2 상태, RestoreRequest 상태,
양 rank의 restore/update audit 및 학습 재진행을 함께 확인한다.

## 결과, 비용, 캡처

```bash
python3 evaluation/e3_experiment.py snapshot --evidence "$EVIDENCE" --confirm "$RUN"
python3 evaluation/e3_report.py --evidence "$EVIDENCE"
jq . "$EVIDENCE/e3-summary.json"
```

e3-summary.md / e3-summary.json / e3-evidence.tar.gz가 발표용 출력이다.
자동 보고서는 **관측 요약**이며 comparison_validity=not_evaluated다.
최종 양 rank counter의 최소값에서 시작 counter를 빼므로 rollback 재수행을 합산하지 않는다.
경계 로그 누락/수집 오류는 경고로 남긴다. 중단 중 최신 step이 오래됐다면 실제 유효 진행량을
과대 해석하지 말고 복원/종료 상태와 대조한다.

비용은 실제 사용한 모든 VM(초기/대체/중복)을 포함한 JSON ledger가 필요하다.
각 항목은 instance_id, market(spot 또는 on-demand), start/end(UTC ISO),
usd_per_hour(실제 리전/OS/시점의 USD 가격)다. 임의 가격은 넣지 않는다.
Spot 가격이 바뀌면 이 단일 단가 계산은 기간 가중평균 가격을 입력하고 근거를 별도 보관한다.
생성~종료 API/CloudTrail 기록과 Spot 가격 이력을 수집하고 운영시간의 오차를 명시한다.

```bash
python3 evaluation/e3_report.py --evidence "$EVIDENCE" --cost-ledger /root/e3-cost-ledger.json
```

ledger가 없으면 비용은 null이다. 이 상태로 비용 절감률을 주장하지 않는다.
계산은 VM 추정 비용이지 AWS 청구액이 아니며 NFS/EBS/FIS/네트워크 등 공통·추가 비용은 별도 명시한다.
VM 비용/유효 tokens, 고정 시간 내 유효 tokens, 전환/복원 중단, checkpoint stall을 같이 비교한다.
checkpoint stall은 완료된 export 시간과 같지 않으므로 실제 trainer/컨트롤러 이벤트를 대조한다.

캡처 권장 순서:
1. frozen plan과 A/B/C 정책 차이.
2. 시작/위험률 요청·소비/선점 요청/측정 종료 시각.
3. FIS completed + 대상 VM 종료(또는 검증된 no_spot_exposure).
4. 복원 검증·양 rank 학습 재진행.
5. 비용 ledger 출처 + 요약 표 + 경고/실패 목록.
저장된 증거 출력임을 명시한다. 실제 서버 화면을 합성하지 않는다.

## 종료

```bash
python3 evaluation/e3_experiment.py stop --evidence "$EVIDENCE" --confirm "$RUN"
python3 evaluation/e3_experiment.py cleanup --evidence "$EVIDENCE" --confirm "$RUN"
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n fluidcr-realign-121040 get nodeprovisions,pods
```

cleanup은 UID 범위만 삭제하며 PVC/PV/NFS 원본은 보존한다. 실제 EC2 terminated 확인까지 종료가 아니다.
진행 중 복원 때문에 stop/cleanup이 실패하면 finalizer를 지우지 말고 상태를 확인한다.
FIS는 이미 전달된 선점을 취소할 수 없다.
B/C의 run별 IAM policy/알람은 종료 확인 후 아래처럼 정리한다. 공유 역할 자체는 삭제하지 않는다.

```bash
aws cloudwatch delete-alarms --region ap-northeast-2 \
  --alarm-names "$(jq -r .alarm_name "$EVIDENCE/e3-fis-setup.json")"
aws iam delete-role-policy \
  --role-name "$(jq -r .role_name "$EVIDENCE/e3-fis-setup.json")" \
  --policy-name "$(jq -r .policy_name "$EVIDENCE/e3-fis-setup.json")"
```

## 당일 예산과 중단 기준

10시부터 환경 확인/파일럿, 이후 측정 3 x 90분, 나머지 최대 3시간은 생성/정리/장애 대응에 쓴다.
프로비저닝과 정리 지연을 포함하면 종료시각을 보장할 수 없다.
17시 이후 새 90분 arm은 시작하지 않는다. 더 정확히는 남은 시간에 측정+종료 여유가 없으면 시작하지 않는다.
일부 arm만 창을 줄이지 않는다. 세 arm이 모두 완료되지 않으면 미완료로 보고한다.
파일럿에서 통합 제어가 검증되지 않으면 E1/E2 기능 증거와 E3 미완료 원인을 제출하고,
E3 성공을 만들어내지 않는다.
