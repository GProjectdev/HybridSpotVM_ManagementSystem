# E2: GPT-2 Spot interruption and group recovery

이 실험은 AWS member cluster 하나에서 GPT-2 small, world_size=2,
On-Demand 1대 + Spot 1대를 사용한다. E1의 클러스터 이동 실험이 아니다.
중지된 On-Premise GPU VM 및 NodeProvision을 삭제하거나 시작하지 않는다.
스크립트는 Karmada/AWS/MGMT kubeconfig만 사용하며 ONPRE1에는 요청하지 않는다.
별도로 실행 중인 컨트롤러의 동작까지 차단하는 기능은 아니다.

## 사전 조건

- 기존 E1에서 검증한 System/FluidCR/StatefulMigration/PVMigration, GPU runtime을 유지한다.
- Karmada 원본 수정이나 컨테이너 재빌드는 이번 스크립트 변경에 필요하지 않다.
- AWS GPU worker가 사용할 AMI, subnet, security groups, 자격증명 Secret을
  `evaluation/policy-source.json`에서 확인한다. On-Premise용 템플릿을 그대로 쓰지 않는다.
- `evaluation/config.gpt2.example.json`의 AWS `nfs-client`, NFS 서버, 데이터/모델 경로,
  trainer 이미지가 실제 환경에 존재해야 한다. 외부 모델 다운로드는 하지 않는다.
- `KARMADA_KUBECONFIG`, `AWS_KUBECONFIG`, `MGMT_KUBECONFIG` 환경변수를 설정한다.
- Python 3.10+, kubectl, AWS CLI와 조회/실험 권한이 필요하다.
- 현재 사용량, GPU On-Demand/Spot quota, 복구 중 겹치는 VM 용량을 확인한다.
  stopped 노드 보존과 실제 EC2 재고 확보는 별개의 문제다.
- 이번 namespace의 이전 실행 workload/NodeProvision을 정리한다. 공유 NFS 파일은 삭제하지 않는다.
- FIS 실험 역할(신뢰 주체 `fis.amazonaws.com`), 해당 Spot에 대한 중단 권한,
  호출자의 FIS 생성/실행/조회 및 iam:PassRole 권한을 준비한다.
- 유효한 CloudWatch stop alarm ARN을 준비한다. 이번 진입점은 alarm 없는 실행을 허용하지 않는다.
  IAM/알람을 자동 생성하지 않으며 범용 관리자 권한 부여도 하지 않는다.

FIS 설정 기준: [AWS 공식 Spot 중단 실습](https://docs.aws.amazon.com/fis/latest/userguide/fis-tutorial-spot-interruptions.html).
FIS 중단은 실제 Spot VM에 영향을 준다. 이미 전달한 중단을 실험 정지로 되돌릴 수 있다고 가정하지 않는다.

## 1. 코드 업데이트 및 렌더링 (mgmt)

현재 브랜치를 유지한 채 업데이트한다.

```bash
cd /root/hybridspot-validation/System
git fetch origin
git merge --ff-only origin/restore-automation-20260928
export RUN="e2-$(date -u +%m%d%H%M%S)"
export EVIDENCE="$PWD/evidence/e2/$RUN"
python3 evaluation/e2_experiment.py render \
  --policy-source evaluation/policy-source.json \
  --run-id "$RUN" --out "$EVIDENCE" \
  --goal-steps 10000 --resume-steps 10 --max-seconds 7200
jq '{run_id,model,cluster,scenario,goal_steps,max_seconds}' "$EVIDENCE/run.json"
jq '.spec | {sourceCluster,policy,checkpoint,capacity}' "$EVIDENCE/policy.yaml"
printf 'RUN=%s\nEVIDENCE=%s\n' "$RUN" "$EVIDENCE"
```

main을 사용하는 경우 merge 대상만 `origin/main`으로 바꾼다.
렌더링은 로컬 파일만 만든다. 구성은 alpha=0.05, fixedOnDemand=1,
fixed checkpoint interval=300초, 계획 기반 부분 교체 비활성, 실제 장애 복구 활성이다.
파일 확장자가 yaml인 입력도 JSON 호환 YAML이므로 jq로 볼 수 있다.
goal=10000은 학습 상한이며 그 step까지 기다리는 실험이 아니다.
복원 후 두 rank의 감사 통과 및 10 step 재진행, FIS/그룹 복구 검증이 완료되면 먼저 종료한다.
2시간 내 완료 보장은 아니다. 파일럿 속도를 보고 새 RUN의 시간 제한을 조절한다.
실행 도중 목표를 바꾸거나 실패를 성공으로 덮어쓰지 않는다.

## 2. 준비 및 실행 (터미널 A)

```bash
python3 evaluation/e2_experiment.py prepare --evidence "${EVIDENCE:?}" --confirm "${RUN:?}"
python3 -u evaluation/e2_experiment.py start --evidence "$EVIDENCE" --confirm "$RUN"
```

prepare는 create-only이며 부분 실패 후 재실행하지 않는다. created.json을 확인한다.
start는 유료 VM 생성을 유발하고 로그/스냅샷을 수집한다. FIS는 자동으로 실행하지 않는다.
두 rank 복원 감사와 후속 진척, FIS 완료, 원본 VM 중단, Verified 그룹 복원 및 old NodeProvision 정리를
확인한 뒤 정상 종료한다. timeout/오류도 기록하며 VM/PVC는 자동 삭제하지 않는다.

## 3. 체크포인트 확인 후 실제 중단 (터미널 B)

터미널 A에서 출력한 실제 RUN을 B에도 설정한다. 아래 ARN은 실제 준비한 값으로 설정해야 한다.
두 rank가 학습하고 첫 전체 체크포인트가 영속 저장될 때까지 기다린다.

```bash
export RUN=실제_RUN
export EVIDENCE="/root/hybridspot-validation/System/evidence/e2/$RUN"
export FIS_ROLE_ARN=실제_FIS_역할_ARN
export FIS_ALARM_ARN=실제_CloudWatch_알람_ARN
python3 evaluation/e2_experiment.py plan-interruption \
  --evidence "$EVIDENCE" --role-arn "$FIS_ROLE_ARN" --alarm-arn "$FIS_ALARM_ARN"
jq . "$EVIDENCE/fis-target.json"
jq . "$EVIDENCE/e2-checkpoint.json"
python3 evaluation/e2_experiment.py interrupt --evidence "$EVIDENCE" --confirm "$RUN"
```

plan이 체크포인트 없음으로 거부되면 오류를 우회하지 않는다. 학습/FluidCR 상태를 확인하고 다시 plan한다.
현재 Pod 2개의 UID, 같은 checkpoint ID, generation, 파일 digest/durableRef를 검사한다.
interrupt도 이를 재검사하며 계획 당시 Spot NodeProvision UID/EC2 ID와 달라지면 거부한다.
한 RUN당 중단은 한 번만 허용한다. start_requested 뒤 네트워크 오류가 나면
AWS FIS 실제 상태를 먼저 확인하며 fis-started.json을 삭제하고 재시도하지 않는다.

## 4. 관찰 및 캡처

```bash
tail -n 30 -F "$EVIDENCE/runner-events.jsonl"
```

다른 터미널에서 아래 명령으로 실시간 화면을 캡처할 수 있다.

```bash
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n fluidcr-realign-121040 get nodeprovisions,pods -o wide
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n fluidcr-realign-121040 get restorerequests -o yaml
python3 evaluation/e2_experiment.py snapshot --evidence "${EVIDENCE:?}"
```

발표에서는 다음 증거를 보여준다.

| 주장 | 파일 및 캡처 대상 |
|---|---|
| 원래 2-rank 학습 진행 | training-events.json의 rank별 step/loss |
| 중단 전 그룹 체크포인트 확보 | e2-checkpoint.json의 Pod UID, checkpointID, sha256, durableRef |
| 실제 Spot 중단 | timeline.json의 interruption_requested, fis-final.json, fis-instance-final.json |
| 복원과 대체 자원 준비 | snapshots.json의 시간순 NodeProvision/RestoreRequest, e2-resources.json |
| 상태 정합성과 학습 재개 | rank별 restored, restore_audit, update_audit 및 후속 step |
| 종료와 판정 한계 | summary.json, runner_phase 및 candidate_pass |

로그를 화면으로 재출력한 캡처는 **저장된 실험 기록**이라고 표시한다.
실시간 서버 화면처럼 재현/조작하지 않는다. 복구 지연은 사건 timestamp 기준을 명시하고,
polling 관측 시간과 컨트롤러 사건 시간을 구분한다. 한 번의 성공으로 신뢰성/성능 우월성을 주장하지 않는다.

## 5. 결과 다운로드

정상/오류 종료 시 start의 finally에서 snapshot/report를 시도한다.
수동 재생성은 클러스터 읽기 없이 report만 실행할 수 있다.

```bash
python3 evaluation/e2_experiment.py report --evidence "${EVIDENCE:?}"
jq . "$EVIDENCE/presentation/summary.json"
cat "$EVIDENCE/presentation/capture-guide.md"
```

출력의 `DOWNLOAD: .../<RUN>-e2-presentation.tar.gz`를 다운로드하여 전달한다.
요약, 정제된 학습 이벤트, 스냅샷, 체크포인트/최종 상태, SHA-256 목록을 포함한다.
누락된 파일은 미수집 증거이며 성공을 의미하지 않는다. candidate_pass도 논문 최종 판정은 아니다.
공유 패키지는 allowlist만 포함한다. 원래 evidence 디렉터리는 로컬에 별도 보존하고,
policy.yaml/manifest/원본 오류 등은 민감한 설정이 있을 수 있으므로 통째로 공개하지 않는다.

## 6. 종료 및 정리

다운로드/검증 후 실행기 프로세스가 종료된 것을 확인하고 실행한다.

```bash
python3 evaluation/e2_experiment.py cleanup --evidence "${EVIDENCE:?}" --confirm "${RUN:?}"
```

중도 종료는 실행 터미널에서 Ctrl+C를 한 번 누르고 stop/수집이 끝날 때까지 기다린다.
정리가 active operation 때문에 거부되면 강제로 finalizer를 지우지 않는다.
실패 실행에 한해서 기존 UID-scoped abort 경로를 사용한다.

```bash
python3 evaluation/e2_experiment.py abort --evidence "${EVIDENCE:?}" --confirm "${RUN:?}"
python3 evaluation/e2_experiment.py cleanup --evidence "$EVIDENCE" --confirm "$RUN"
```

cleanup은 해당 RUN 자원에만 삭제를 요청하며 PVC/PV/NFS 내용은 보존한다.
EC2 종료까지 보장하는 동기 명령은 아니다. evidence에 기록된 인스턴스 ID를 조회해 terminated를 확인한다.
FIS template/CloudWatch alarm/IAM 역할은 자동 삭제하지 않는다.
이 도구는 로컬 단위 테스트 대상이며 실제 E2 GPU/FIS 실험의 성공은 서버 실행 결과로 검증해야 한다.
