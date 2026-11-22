# 2026-11-22 평가 준비 도구

운영 Controller·CRD·이미지·학습량·위험률 알고리즘은 변경하지 않는다.

`evaluation/integrated_fis_setup.py`는 기존 4-Rank C의 초기 1OD+3Spot에서
이름 순 첫 Spot을 미리 결합하고, 해당 Instance ARN과 기존 NodeProvision UID/Namespace
태그에만 허용되는 추가 inline policy 및 기존 OD health alarm 구성을 반환한다.
기존 역할의 정책은 덮어쓰지 않는다. 함수 자체는 AWS 호출·권한 적용·FIS 실행을 하지 않는다.
실제 host 적용은 별도 GPU/FIS 승인, 운영 동결 및 현재 provider 소유 확인이 필요하다.
기존 `final_fis`의 Step/UID/durable checkpoint/native cutoff/단일 intent 검사는 유지한다.

`evaluation/stateful_replay.py`는 가격 적분, 시간구간 합집합, 불변 전체 Rank Round,
계획/긴급 저장 분기, 단일 operation, fencing/prepare/resume, 두 번의 새 진전,
Verified 기준 OD 유지, 원형 연결 없는 Trace 경계의 CPU 모델 기본 요소다.
모델의 영수증 검사는 실제 provider fencing이나 파일 무결성 검증을 대신하지 않는다.
전체 정책·액추에이터의 전방향 Replay 및 외부 예측 검증은 아직 완료되지 않았다.

`evaluation/replay_observed.py`는 보존된 A/B02 CSV로 VM별 가격 적분·전체 Rank 목표
시각·저장 Step과 첫 재개 Step 차이를 재구성한다. 원본 판정은 변경하지 않는다.
이는 관측 CSV의 회계 검산이며 독립적인 원시 자료 재감사나 전방향 예측 정확도 증거가 아니다.

```bash
python3 -m unittest evaluation.test_integrated_fis_setup evaluation.test_stateful_replay -v
python3 -m evaluation.replay_observed \
  --evaluation /home/ubuntu/graduate/Evaluation \
  --rules /home/ubuntu/graduate/Evaluation/day22_preparation_20261122/replay_rules_predeclared.json \
  --out /새로운/경로/observed_reconstruction.json
```

입력 Hash·사전 오차 기준·잔차를 보존한다. 관측값을 재현한 계산과
Trace-driven Estimated, Sensitivity/What-if를 구분한다. 새로운 자연 선점 사건은 생성하지 않는다.
