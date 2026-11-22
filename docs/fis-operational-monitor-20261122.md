# C 실행의 운영 감시 보완 (2026-11-22)

`gpt2-c-01`은 정식 C 시도였으며, 성공한 복원 Run으로 바꾸지 않는다.
목표는 4-Rank GPT-2 4,800 Step이고, 공통 Step 2,400 이상에서 단일 Spot에 FIS를 요청했다.
실제 기록은 실행 디렉터리의 원본으로 보존한다. 이 문서는 계측 도구 변경 설명이다.

## 확인한 중지 원인

- 05:03:46 UTC: Observer가 대체 VM을 전역 소유 목록에 등록했지만 완성된 Snapshot 원장은 이전 집합이었다. `incomplete owned instance ledger`로 첫 센서 오류가 발생했다.
- 05:04:02 UTC: FIS 대상 VM의 기존 Rank Pod가 Kubernetes에서는 아직 Running이었다. 그 Pod의 로그 RPC 실패를 일반 Running Rank 오류로 분류하면서 두 번째 센서 오류가 됐다.
- 기존 규칙대로 두 번의 연속 센서 오류가 중지를 요청했다. 학습·Checkpoint·자원 정책을 실행 중 변경하지 않았고, 추가 유료 재실행도 하지 않았다.
- VM 5대 및 루트 EBS 5개의 반환과 ENI 정리를 독립 확인했다. NFS/PVC/PV는 유지했다.

긴급 Checkpoint CR의 `Completed`는 완전한 durable Round와 다르다. 이번 긴급 Round에서 중단 Rank의 export 증거가 없었으므로 전체 복구점으로 인정하지 않는다. 대체 VM은 On-Demand였으며, Restore Verified에 도달하기 전에 감시가 중지했다. 이후 복원 성공·실패를 추정하지 않는다.

## 최소 수정

`evaluation/operational_watch.py`는 관측 때 소유 집합을 고정한 뒤 그 정확한 ID들만 AWS에서 독립 조회한다. 이 조회 결과에 UID/Namespace/공급자 태그/시각/상태 검증을 그대로 적용한다. 새 VM은 이전 Snapshot에 없어도 비용·동시 수 계산에 포함된다. 더 나중에 게시된 과거 원장이 이전 소유 집합에 섞이지 않도록 조회 경계를 기록한다. `NOT_OBSERVED`는 여전히 종료나 비용 0이 아니다.

`evaluation/fis_observation_guard.py`는 다음 조건이 모두 증명될 때만 원래 FIS 대상 Pod 한 개의 로그 부재를 예상된 손실로 분리한다.

- 명시적으로 승인된 동일 C Run, 동결 SHA, 정책/Workload UID, 공통/실제 Spec Hash.
- 동일 Experiment ID, 정책/Run 태그, 단일 Spot ARN, 승인한 `PT2M` Action.
- 신선한 AWS 응답에서 그 정확한 UID 소유 Spot이 shutting-down 또는 terminated.
- 고정한 원래 4-Rank Pod 집합 중 해당 NodeProvision의 노드에 있던 Pod UID 한 개.

다른 Rank, 재생성된 Pod, 살아 있는 대상 VM의 로그 오류, 다른 FIS/UID, 오래된 관측, 잘못된 공급자, 알 수 없는 종료는 계속 오류다. NFS·Management 공간, 가격 신선도, 비용/VM 수, 종료시간, 원본 로그 보존 및 독립 정리 Watchdog은 유지한다. VM/Checkpoint/Restore Controller나 학습 코드는 변경하지 않는다.

## 검증과 한계

CPU 테스트 41개(기존 27 + 신규 14)를 통과했다. 실행 명령:

```bash
cd /home/ubuntu/HybridSpotVM_ManagementSystem/evaluation
python3 -m unittest -v test_fis_observation_guard test_operational_watch
```

실제 C 기록을 이용한 CPU Replay에서는 05:04:02 표본의 정확한 선점 대상 로그 부재를 새 감시가 허용하고 비용 계산을 유지했다. 05:03:46 표본은 그 시각까지 완성된 5-VM 응답이 남아 있지 않아 정확한 as-of Replay를 하지 않았다. 이 첫 상황의 보완은 대체 VM이 생기는 경계의 단위 테스트로 확인했다. 미래 기록을 이전 시각의 가용 증거로 바꾸지 않았다.

Replay의 AWS/저장 공간 센서는 Mock이며, 실제 복원이 이어졌다는 증거가 아니다. 기존 C 원시 자료·중지 판정·Frozen 패키지는 변경하지 않았다. 수정 코드를 다음 실행에 포함하려면 별도의 운영 버전 검토와 실행 승인이 필요하다. 이 문서는 새 유료 실행을 승인하지 않는다.
