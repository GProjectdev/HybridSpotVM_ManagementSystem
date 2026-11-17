# 2026-11-17 정량 평가 사전 점검

이번 평가의 기준은 `IEEE_Access_Hybrid_DDP_정량평가_실행계획_v2_20261117`이다.
과거 E1/E2 기능 검증과 이번 N1/N2/F1–F4 정량 평가의 이름과 증거를 구분한다.
이 문서는 평가 조건을 확정하는 절차이며 실제 성공 결과를 대신하지 않는다.

## 실행 순서

1. AWS profile `new-account-v2`의 계정과 Seoul `ap-northeast-2c`를 확인한다.
   기존 실행의 NodeRequest/NodeProvision, EC2 및 연결 EBS 종료 증거를 먼저 확인한다.
2. 실행마다 새 namespace와 Checkpoint PVC를 사용한다. 과거 평가 도구의 고정 namespace를
   재사용하거나 기존 StatefulSet에 설치 스크립트를 재실행하지 않는다.
3. g5.xlarge 1 OD + 1 Spot, minOD=1로 시작한다. 새 rank 0이 OD에 배치됐음을
   Pod UID/NodeProvision UID/provider ID로 확인한 뒤 rank 1을 시작한다.
4. 전체 WikiText-103 tokenized 데이터와 GPT-2 Small의 revision 및 SHA256을 기록한다.
   `--max-train-blocks` 데이터는 기능 점검용으로만 표시하고 정량 평가에 섞지 않는다.
5. 별도 예비 실행에서 15–20분 정상 학습을 수집한다. Warm-up을 제외한 Step Time
   중앙값으로 정상 학습 60–90분에 해당하는 동일 목표 Step/Token 수를 사전 고정한다.
6. 성공한 Adaptive Checkpoint의 실행 시간/횟수로 Fixed 후보를 정하고 모든 평가 전에
   하나로 고정한다. 기능 점검용 60초 또는 임의 기본값을 실측 최적값이라고 쓰지 않는다.
7. GPT-2의 실제 Checkpoint → durable NFS → 새 VM Restore → Verified → 학습 재개를
   검증한다. 프로세스 재시작의 application-state 로드는 VM 복원 성공과 구분한다.
8. 실행 중 위험 입력 → 대체 OD 준비 → 부분 복원 → 이전 Spot 실제 종료까지 확인한다.
   새 VM 준비만 성공한 경우 전체 전환 성공으로 기록하지 않는다.
9. Static/Risk-Aware와 Fixed/Adaptive의 독립 선택 및 실제 schedule/실행 간격을 확인한다.
   위험 입력은 Collector 실측 또는 코드 경계 기반 통제 입력으로 출처를 명시한다.
10. 실행 종료 시 NodeRequest로 보충을 중지하고 원본 Workload를 삭제한다. UID 소유 관계를
    확인한 NodeProvision의 정상 삭제 후 실제 EC2/EBS 종료까지 확인한다. Finalizer를
    강제 제거하지 않으며 NFS·PV/PVC·결과·운영 인프라는 보존한다.

## 비교군과 판정

| Run | 구성/자원 정책 | Checkpoint | 사건 |
|---|---|---|---|
| N1 | 2 OD | 공통 Fixed | 없음 |
| N2 | 1 OD + 1 Spot | 공통 Fixed | 없음 |
| F1 | Static Hybrid | Fixed | 예정 FIS |
| F2 | Static Hybrid | Adaptive | 동일 위험 입력/예정 FIS |
| F3 | Risk-Aware | Fixed | 동일 위험 입력/예정 사건 |
| F4 | Risk-Aware | Adaptive | 동일 위험 입력/예정 사건 |

F3/F4가 먼저 Spot을 반환하면 FIS를 다른 VM에 옮겨 실행하지 않는다.
이는 같은 예정 사건에서 Spot 노출을 제거한 조건이며 자연 선점을 예측한 결과가 아니다.
E3는 위 Run의 원본 자료에서 계산하며 별도 GPU Run을 추가하지 않는다.

GO는 GPT-2 C/R, 실행 중 전환, 정책 독립성, UTC 사건 수집, 전용 FIS 제한을 실제
증거로 확인한 뒤 결정한다. 실패한 CR을 수동 Completed/Verified로 만들지 않는다.
미검증 항목이 남으면 해당 비교군을 NO-GO로 기록하고 N1/N2의 가능 여부도 별도로 판단한다.

## 증거와 시간 제한

공통 키는 `run_id,timestamp_utc,event,rank,global_step,instance_id,policy_id,details`다.
원본 rank 로그와 Pod UID/attempt, 정책 세대, CR 상태, Instance/볼륨 ID, 가격 이력을 보존한다.
복원 후 로그에 포함된 이전 실행 이벤트는 현재 Pod 생성 시각과 attempt를 대조해 중복 제거한다.
유효 토큰은 완료 Step 기준으로 계산하며 rollback/rework를 두 번 세지 않는다.
Checkpoint 벽시계 시간과 실제 DDP 진행 공백은 각각 기록한다.
겹치는 VM 준비와 학습 시간을 Stall에 중복 합산하지 않는다.

하루 최대 5시간과 승인된 종료 시각 중 이른 시각을 적용하고 정상 정리에 필요한 여유를 둔다.
성공 조건이 충족되면 일찍 종료한다. 자동 정리 타이머를 실험 전에 설치하고, 종료 후 해제한다.
AWS/Git 인증정보, kubeconfig, Secret, 개인 키, 원본 학습 데이터는 Git에 넣지 않는다.

## v2 원본 CSV 내보내기

수집한 `training-events.jsonl`에서 현재 Run만 내보낸다. Node/EC2 매핑은
NodeProvision UID와 AWS provider ID를 대조해 별도 JSON으로 작성한다.

```bash
python3 evaluation/export_v2.py --events /path/to/training-events.jsonl \
  --output /path/to/experiments/PREFLIGHT --run-id RUN_ID \
  --policy-id NODE_REQUEST_UID --node-instances /path/to/node-instances.json
```

동일 이벤트 재수집은 제거하지만 다른 attempt의 재실행 Step은 보존한다.
누락 Instance ID는 빈 값으로 두고 추정으로 채우지 않는다. 이 CSV만으로 비용,
완료 여부, 유효 처리율을 확정하지 않는다. VM/Checkpoint/Restore/FIS 원본 증거와
연결한 후 별도로 계산한다. 나노초 원본 시각은 유지하며 Python 3.10에서도 비교할 수 있다.

## 공통 이미지 준비와 FIS 범위

Provisioner의 기존 `NodeProvisionNetConfig.spec.softwareConfig.imagePrepulls`에
동일한 digest의 학습 이미지를 `nodeTarget: gpu`로 등록할 수 있다. 모든 비교군에
같은 설정을 적용하고, `PrePullingImages` 완료부터 Ready까지의 시간도 기록한다.
이미지를 준비하는 동안 기존 학습이 계속됐는지와 실제 진행 공백을 구분한다.

FIS 템플릿 생성 권한은 템플릿뿐 아니라 지정한 action ARN에도 필요하다.
`aws:ec2:send-spot-instance-interruptions` 하나만 허용하고, 실행 Role의 EC2 권한은
현재 Run에서 UID를 확인한 Spot Instance ARN 및 Run 태그에 모두 제한한다.
IAM 변경 후 전파를 기다린 뒤 템플릿을 생성한다. Role의 대상 허용 및 다른 VM 거부,
운영자의 해당 템플릿 실행 권한을 시뮬레이션으로 확인한다. 이 검사는 실제 FIS 중단 성공과
구분하며, 이전 실행의 Role/템플릿을 새 VM에 그대로 적용하지 않는다.
공식 권한 정의: https://docs.aws.amazon.com/service-authorization/latest/reference/list_fis.html

## 완료 기록과 다음 설정 세대

`RecoveryReconciler`는 `spot-vm-management-controller` 프로세스가 담당한다.
완료된 SpotRecovery는 후속 NodeRequest 설정 변경으로 Rejected로 덮어쓰지 않는다.
이미 덮어써진 기록의 재구성은 원본 NodeProvision 부재, 동일 policy/workload UID,
Completed SpotReplacement, 해당 replacement UID, Verified RestoreRequest와 기존
복원·source fencing 증거가 모두 일치할 때만 허용한다. 이 재구성 경로에는 삭제 동작이 없다.
진행 중인 새 삭제 작업의 policy generation 검증은 계속 엄격하게 적용한다.

정상 학습 Pilot은 Checkpoint/프로세스 재시작 대기 시간을 제외한 연속 Step 구간을
15분 이상 확인한다. 타이머 경과만으로 정상 학습 15분을 확보했다고 기록하지 않는다.
