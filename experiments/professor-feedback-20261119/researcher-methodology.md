---
title: "Hybrid Cloud Spot DDP 정량 평가 계획 — 교수님 피드백 반영"
date: "2026-11-19"
status: "평가 설계안 / 정식 측정 결과 아님"
---

# Hybrid Cloud Spot DDP 정량 평가 및 변수 영향 분석 계획

**작성 기준:** 2026-11-19  
**목적:** 졸업논문·IEEE Access 정량 평가 및 교수님 피드백 반영  
**진행 목표:** 11월 27일 주요 결과 확보, 필요 시 12월 2일까지 반복·검산  
**중요:** 본 문서는 **실험계획**이다. 11월 18일의 기능 수용검사 결과를 정식 E1/E2 성능 측정값으로 취급하지 않는다.

## 1. 교수님 피드백 및 반영 원칙

> “일단 진행하는데ᆢ 타논문의 것이라도 그논문의 알고리즘상의 변수의 영향등을 관찰하는것은 그부분도 설명이될수있게하게”

**해석:** E1·E2 정량 비교는 계속 진행하되, 기존 논문에서 채택한 위험률·자원 선택·Checkpoint 정책의 **입력 변수 → 정책 결정 → 실제 동작 → DDP 운영 결과**를 추적하여 설명한다. 기존 알고리즘의 신규성을 주장하거나 모든 변수별로 별도의 대형 GPU 실험을 만들라는 뜻으로 단정하지 않는다.

**평가의 세 축**

1. **E1 정상 운영 효과:** All On-Demand 대비 통합 관리 시스템의 완료시간·GPU 비용·Checkpoint/자원 관리 오버헤드.
2. **E2 실제 Spot 선점·복구:** 중단 없는 제안 시스템 대비 선점 시 추가 완료시간·GPU 비용·복구/재학습 부담; 동일 사양의 All On-Demand와도 경제성 비교.
3. **공통 알고리즘 변수 분석:** 위험률 λ, 자원 정책 기준 α, 실측 Checkpoint 비용 D 등으로 정책 결정이 어떻게 바뀌는지 검증하고 GPU 실측과 연결.

**연구의 차별성:** Desai 등의 위험률 기반 정책과 기존 DDP Checkpoint/Restore 엔진의 자체 알고리즘이 아니라, 이를 Karmada 기반 Public Cloud GPU Worker의 요청·배치·교체·복원·반환과 연결한 **통합 관리의 실제 운영 비용·성능 및 적용 한계**를 검증한다.

## 2. 평가 환경과 비교 원칙

| 항목 | 확정 원칙 및 주의사항 |
|---|---|
| 클라우드 | AWS **ap-northeast-2c** 고정, 기존 Karmada Management + AWS Member 사용 |
| DDP 크기 | **4-Rank 우선**(VM당 GPU 1개/Rank 1개); 4-Rank 자원 공급 불가 시 **2-Rank 대안**. 동일 비교 블록에서 크기 혼용 금지 |
| GPU 유형 | 비교 블록의 A/B/C 모두 동일 Instance Type·AZ·AMI·Driver·이미지; G5/G6 간 혼합 금지. xlarge에서 Spot 부족이 관측되었으므로 실제 공급과 Quota 확인 |
| 모델·데이터 | GPT-2 Small/WikiText-103, ResNet-50/CIFAR-10 (기존 전처리·모델·자산 Hash 재사용) |
| 학습량 | 모델별 Global Batch·초기 상태·목표 Optimizer Step·Seed를 A/B/C에 맞춤. Rank 수 변경 시 per-rank batch 조정 후 별도 블록 |
| 저장·복원 | 현재 검증 가능한 Application-level + CRIU 기반 Full-group 복구 경로. **Restore 업데이트 완료 후** Image Digest/Commit 고정 |
| 위험 입력 | 기존 SkyPilot Spot availability trace → 기존 SARIMA 예측값. **서울 G5/G6의 실제 자연 선점 확률이 아님** |
| NFS | 온라인 gp3 변경이 `completed` 상태이고 동일 저장 조건임을 확인한 뒤 정식 측정 |
| 과금 | 모든 최초·대체·중첩·유휴 GPU VM의 실제 과금 가능 구간 및 Spot/OD 가격 이력. NFS/EBS/VPN/FIS/관리 노드 비용 제외 시 명시 |
| 안전 | 실제 EC2 생성·FIS·IAM 및 인프라 변경은 사용자 승인 범위 내에서만. Run 종료 후 provider ID 기반 정리 확인 |

**중요한 규모 표현:** 4-Rank는 Spot **4대**를 뜻하지 않는다. 총 GPU Worker 4대이며 OD 최소 1대 제약 아래 정책이 Spot/OD 구성을 정한다. Spot 할당량 `32`의 **vCPU 단위 및 실제 계정 적용값은 미확인**이며 공급 보장이 아니다.

## 3. E1 — 정상 운영 End-to-End 비용·성능

### 3.1 연구 질문

> 사용자 요청부터 GPU Worker 생성, DDP 학습, Checkpoint 조율, 위험률 기반 자원 교체, 학습 완료 및 반환까지 포함했을 때, 제안 통합 관리 방식은 All On-Demand에 비해 어떤 비용–성능 관계를 나타내는가?

### 3.2 비교군

| 항목 | A: All On-Demand | B: 제안 통합 관리 |
|---|---|---|
| 4-Rank 후보 | 4OD | 기존 위험률·가격 정책이 결정하는 Spot/OD 조합 |
| 2-Rank 대안 | 2OD | 최소 1OD 유지, Spot 포함 시 1OD+1Spot |
| 자원 변경 | OD 구성 유지 | 위험 정보 갱신에 따라 구성 변경 가능 |
| 주기 Checkpoint | 비활성 | 실제 구현된 Bootstrap / 실측비용 기반 조율 |
| 인위적 중단 | 없음 | 없음 |
| 복구 경로 | 동일 기반 인프라, 정상 운영 | 정책 교체가 발생하면 기존 복구 시스템 사용 |

이 비교는 **전체 운영 패키지**의 효과다. GPU 가격 할인, 자원 정책, Checkpoint 알고리즘 각각의 순수한 인과적 기여를 분리했다고 주장하지 않는다.

### 3.3 위험 입력 선정 및 시간 통제

- 기존 확보·고정한 **SkyPilot 미국 V100 가용성 자료 기반 SARIMA held-out 예측 시계열**을 우선 사용한다. 입력·원본·예측 Hash, 학습/보류 구간, 적용 Region/AZ/Instance Type과 실험 clock을 저장한다.
- 이전 코드·입력 검사에서 `held-out hour 14: λ≈0.000921/h → 1OD+3Spot`, `hour 15: λ≈0.345840/h → 4OD`가 **목표 정책 결정**으로 확인됐다(4-Rank, 당시 가격/설정 기준). 이것은 정식 GPU 실측 결과가 아니다.
- 예측 시간 1시간은 실제 **3600초 재생**을 원칙으로 하며, 기능 진단에서 사용한 압축/수동 위험 입력 전환을 정식 E1 결과와 섞지 않는다.
- 초기 Spot이 존재하고 목표 학습 중 위험 정책 변화가 나타나는 **사전 정의된 적격 구간**을 사용한다. 정식 성능 결과를 보고 더 유리한 구간으로 변경하지 않는다.
- A는 정책 기반 Spot 자원 선택을 적용하지 않으므로, 위험 입력은 비교 조건·시계 기록용이다. B의 정책 판단은 실제 가격·경제성 조건까지 포함한 **기존 코드 결과**로 판정한다.

### 3.4 단계별 실행

1. 복구 방식·NFS·이미지·GPU 유형·Batch·목표 Step·위험입력·실험시계를 동결한다.
2. A에서 총 Rank 수만큼 OD VM을 요청하고, DDP의 목표 학습량과 모든 VM 종료를 검증한다.
3. B에서 같은 Rank 수를 요청하고, 기존 위험률/가격 정책에 따라 초기 Spot/OD 구성을 결정한다.
4. B에서 실제 시계에 따라 위험 입력을 갱신하며 자원 결정, Checkpoint 결정, VM 생성·교체, 복원·재개, 반환을 이벤트 UID/시각과 연결한다.
5. 같은 학습 목표의 완료시간, 유효 처리율, GPU 비용 및 관리 오버헤드를 A/B로 비교한다.
6. 자연 중단·공급 실패·복원 실패가 발생하면 실패/계획 이탈로 별도 기록하고 성공 시도만 선별하지 않는다.

### 3.5 측정 결과

- **T_E2E:** GPU 요청 시각 → 모든 Rank의 목표 Optimizer Update 완료.
- **C_GPU:** 모든 소유 GPU VM의 과금 가능한 실행 구간 × 유효 가격(Spot 변경 이력 포함); 종료 확인 및 정리 대기 비용 포함.
- Checkpoint 횟수, 실제 간격, 전체 DDP Pause 구간의 합집합, 실측 저장·전파 비용.
- 목표 Spot/OD 비율과 실제 VM 구성의 일치·전환 지연, 그룹 복원·재개·반환 지연.
- 기준 GPU 비용 절감률 `(C_A-C_B)/C_A`, 완료시간 차이 `T_B-T_A`, 모델별 유효 처리율.
- **주의:** E1의 무장애 비용 이득을 실제 선점 회피 효과라고 해석하지 않는다.

## 4. E2 — 선점·복구 부담을 포함한 End-to-End 경제성

### 4.1 연구 질문

> 실제 Spot Worker 중단이 발생했을 때, 제안 시스템의 Worker 교체 및 전체 DDP 복구로 인해 정상 운영보다 추가되는 시간·GPU 비용은 얼마이며, 복구 부담을 포함해도 동일 규모의 All On-Demand 대비 비용상 이점이 유지되는가?

### 4.2 비교군

| 비교군 | 자원/정책 | 중단 조건 | 비교 의미 |
|---|---|---|---|
| A | E1의 All OD 정상 | FIS 없음 | 안정 자원 비용·완료시간 기준 |
| B | 제안 시스템 정상, 정책·위험 입력 동결 | FIS 없음 | 중단 전 시스템 정상 기준 |
| C | B와 같은 모델·GPU·위험 입력·초기 구성·Checkpoint 정책 | **Spot Worker 1개 FIS** | 실제 선점·복구 추가 부담 |

- **B와 C는 동일 비교 블록**이어야 한다. 동일한 위험 시계열이라도 B는 실험 중 다른 Spot 유지 상태가 될 수 있으므로 **C의 예정 시각에 Spot이 존재하는지** 사전 확인한다.
- E1의 B를 E2의 B로 재사용하려면 설정·위험 구간·clock·Checkpoint·하드웨어·완료/과금 원시 증거까지 동일해야 한다. 불일치하면 별도의 정상 B 기준선을 다시 측정한다.
- 기존 고위험 전환 시계열과 C의 Spot 중단 조건이 충돌하면 임의의 장애 시점·입력값을 변경하지 않는다. 연구자에게 **E1 위험 변화 블록 / E2 Spot 유지 블록 분리 + B 기준선 별도 수집**을 제안하고 승인받는다.

### 4.3 장애 주입 및 복구 과정

1. 현재 Owned Spot VM, 최초 유효한 **전체 Rank durable Checkpoint**, 네 Rank의 Fresh progress 및 FIS 대상이 사전 계약과 일치하는지 검증한다.
2. **사전 동결된 목표 Step의 50%**에 도달한 뒤 FIS를 정확히 소유 확인한 Spot VM 한 대에만 적용한다. 50%는 제어된 장애 지점이며 자연 선점 발생시각의 대표값이 아니다.
3. FIS dispatch / 2분 통지 / 실제 EC2 중단 / DDP 마지막 진전 / 대체 VM 생성·Ready / Restore / 모든 Rank의 업데이트 재개를 별도 시각으로 기록한다.
4. 복원 Checkpoint Step과 중단 직전 마지막 공통 Step을 비교하고, **이전 진전 수준을 다시 회복하는 실제 재학습 소요시간**을 구한다.
5. 모든 Rank가 목표 Step까지 완료하고 실제 VM 종료·정리·비용 영수증 확보까지 검증한다.

### 4.4 비교와 지표

- **B vs C:** `ΔT_fault=T_C-T_B`, `ΔC_fault=C_C-C_B`로 단일 선점·복구 추가 부담.
- **A vs C:** 동일 모델/Rank/하드웨어의 전체 비용–완료시간 비교; 경제적 이점이 유지되는지 확인.
- 실제 학습 **No-progress 시간**, 복원 이후 **재학습 소요시간**, 대체 VM 준비·Restore·그룹 재개 지연, Checkpoint Stall, 긴급 저장 성공 여부.
- 긴급 Checkpoint 성공으로 재학습량 0이 되더라도 VM 대기·복원·추가 비용을 측정한다. 복구 성공만으로 다른 엔진보다 우수하다고 주장하지 않는다.

## 5. 교수님 피드백 — 알고리즘 변수 영향 분석

### 5.1 분석 대상 및 분류

| 변수/상태 | 기본/출처 | 무엇을 관찰하나? | 방법 |
|---|---|---|---|
| **위험률 λ_t** | 보류 예측 시계열 | 목표 Spot/OD 수·Checkpoint 간격·전환 경계 | 정책 재생 + GPU 실측 연결 |
| **α** | 기존 운영 0.8 | 위험률 대비 Spot 유지/OD 전환 조건 | 기존 실제 정책 함수의 오프라인 민감도 |
| **PT** | 기존 1시간 | 위험/유효 처리율 계산의 분석 범위 | **기본 실험 고정**, 다른 PT를 실제 예측 정확도로 주장하지 않음 |
| **Spot/OD 가격** | AWS 동일 타입/시각·AZ 조회 | 자원 구성의 경제성 판단 | 정책 결정 로그와 유효 가격 영수증 |
| **Checkpoint 비용 D** | 같은 스토리지·4Rank 실제 round | 선택 주기와 관리비용의 관계 | 실측 비용 입력을 사용한 기존 정책 함수 분석 |
| **Checkpoint Interval f** | Coordinator 선택, 실측 실행 | 저장 횟수·Pause·복원시점·재학습 부담 | 실제 child round/Pod UID/Step 로그 |

**주의:** 현재 GPU에서 관측된 Checkpoint 정책은 **위험 구간 초기화 분기**(`risk-band-bootstrap`)와 **실측 비용 기반 분기**(`checkpoint-cost-analytic`)다. 별도 **Desai 비동기 최적화 분기**(`paper-equations-1-5-analytic`)는 실제 GPU 적용이 입증되지 않았다. 사용하지 않은 분기를 실행했다고 쓰지 않는다.

### 5.2 구체적인 민감도 분석 절차 (가능한 한 추가 GPU 0 Run)

**A. 자원 정책 변수:**

1. 실제 사용 중인 기존 정책 함수에 **사전 고정된 held-out λ 시계열, GPU 타입·가격·복구 비용 조건**을 입력한다.
2. α는 기본 0.8을 유지하며, **0.7 / 0.8 / 0.9**를 *연구자 선정 민감도 지점*으로 오프라인 비교한다. 원문이 모든 값을 최적이라고 입증했다는 의미가 아니다.
3. 나머지 변수·데이터 시점·복구 비용 freshness를 동일하게 고정하고 **목표 OD/Spot 구성과 결정 사유**를 기록한다.
4. 가격/복구 비용이 없어 Throughput-only Bootstrap으로 결정됐으면 해당 분기를 기록하고, Desai 경제성 식 전체가 실제 평가됐다고 표현하지 않는다.

**B. Checkpoint 조율 변수:**

1. GPU 실험에서 확보한 실제 **checkpointSeconds / copySeconds / observedAt**와 정책 경로를 기록한다.
2. 동일 λ에서 실측 D의 대표값(예: 중간값·관측 범위)을 입력으로 바꾸어 결정 interval을 계산한다. 실측이 없거나 다른 저장장치 상태에서 온 값은 사용하지 않는다.
3. 동일 D에서 held-out λ를 변화시키고 **선택된 Interval과 Bootstrap/비용 기반 분기**를 기록한다.
4. GPU 실험 로그의 **결정 interval → 실제 checkpoint pause/start/finish → 학습 재개**를 연결한다. 설정값을 실제 실행 간격으로 둔갑시키지 않는다.

**C. 해석 원칙:**

- **오프라인 정책 민감도** = 정책 함수의 결정 변화(실제 완료시간·비용의 인과적 실측이 아님).
- **E1/E2 GPU 측정** = 특정 입력 시계열에서 실제 VM/CP/복원에 따른 시간·비용.
- 둘을 연결하되 α·D를 바꿀 때마다 GPU 전체 성능이 똑같이 변한다고 외삽하지 않는다.
- Checkpoint 주기의 *독립적 최적성*을 주장하려면 동일 하드웨어·위험률·복구 환경의 fixed/adaptive 비교가 별도로 필요하다. 이번 평가에서는 해당 우수성을 사전 확정 결론으로 두지 않는다.

### 5.3 교수님께 제시할 결과·그림

1. **Risk → Decision timeline:** λ_t 시계열과 목표/실제 OD 수, Spot 수, Checkpoint interval, 실제 VM·Checkpoint 이벤트.
2. **Policy sensitivity:** λ × α에 따른 목표 자원구성 및 정책 사유(기존 코드의 계산 결과).
3. **Checkpoint sensitivity:** λ 및 실측 D에 따른 결정 interval, 실제 CP 수행 간격·Stall과의 차이.
4. **E1 cost–time:** A/B GPU 비용과 완료시간, 반복 개별점, 관리 오버헤드.
5. **E2 recovery:** Spot 통지·중단·대체 VM 준비·Restore·전체 Rank 업데이트 재개·이전 진전 회복의 타임라인 및 ΔT/ΔC.

## 6. 결과 판단: 이점·조건·한계

- **이점:** 같은 비교 블록에서 실제로 측정된 GPU 비용·학습시간·완료 여부의 유리한 결과. 반대 결과도 그대로 제시한다.
- **조건:** 실제 위험 시계열·선택한 Spot/OD 구성·Checkpoint 비용·VM 확보 및 복원시간·선점 사건의 위치와 횟수.
- **한계:** 단일 AWS AZ, G 계열 특정 GPU, 제한된 Rank/모델, 공개 US V100 가용성 Proxy, FIS 단일 장애, 실제 Spot 위험률/예측정확도 미검증.
- **손익분기 분석:** 필요한 경우 동일 Rank·모델·GPU·위험/CP 계약에서 측정한 정상·선점 비용을 이용해 *명시한 0회/1회 장애 가정*의 시나리오 계산으로만 수행. E1/E2의 다른 위험 입력이나 2-Rank/4-Rank 결과를 직접 합산하지 않는다.
- **실험 버전:** Restore·VM교체·NFS 최적화 등 의미 있는 변경은 **최종 이미지/Commit/GPU/NFS 사양을 동결한 뒤** A/B/C 공통조건으로 적용한다. 변경 전 기능수용 결과를 새 버전 정식 성능값으로 합치지 않는다.

## 7. 11월 19일 이후 실행 일정

| 시점 | 실무 수행 내용 | 완료 기준 |
|---|---|---|
| **11/19(목)** | Restore 업데이트의 변경 범위 파악·영향 구간 검증, gp3 modification completed 확인, 위험/Checkpoint 변수 오프라인 분석, 실험 스크립트/계측 최종 확인, 의미 있는 안전한 성능 개선 | 정식 실행 전 차단 조건·최종 변경 목록과 테스트 증거·실험 동결 후보 |
| **11/20(금)** | 동일 GPU/Run 설정 파일럿·필요 시 FIS/연속 전환 영향 구간 수용검사, 조건 통과 시 E1 A 기준선 시작 | 일관된 A/B/C 비교 계약, 최초 정식 A 또는 실행 가능 판단 |
| **11/23–24** | E1 B 정상 운영과 비교, 입력→정책→실제 자원/CP 이벤트 수집 | E1 비용·시간·정책 타임라인 |
| **11/25–27** | E2 정상 B 적격성 검토·FIS C, 1차 결과 검산 | 동일조건 A/B/C와 장애 추가 부담 |
| **11/30–12/01** | 핵심 대응 반복, 모델 간 적용 가능성 보완 | 실패·공급 문제 포함 반복 자료 |
| **12/02** | 정책 변수 영향·E1/E2 그래프·경제성 및 한계 정리 | 검산된 원시자료/결론/재현성 파일 |

**4-Rank 우선 / 2-Rank 대안:** 공급/Quota 문제 시 **연구자에게 사유·영향을 보고**하고 동일 Rank로 A/B/C 전부 구성한다. 4-Rank와 2-Rank 결과를 하나의 paired 비교에 섞지 않는다.

## 8. Codex 실행 준비 및 완료 기준

**기존 11/18 검증을 재사용할 것:** 4-Rank GPT-2/ResNet 새 VM Full-group Restore 성공 증거, GPT-2 FIS 중단·복구 기능 수용, 첫 Spot→OD 교체, Collector/SARIMA 연결, 기존 모델/데이터·manifest/해시, 평가 스크립트의 이미 통과한 테스트.

**새로 확인할 것:** 변경 중인 Restore 버전이 정식 실험에서 무엇인지, gp3 변경 완료 및 저장 성능/공간, 3개 Spot 연속 교체와 자원 정리(필요 시), B와 C의 동일 위험 입력·Spot 노출 가능성, Quota/실제 GPU 공급, 정책 변수 로그·가용한 관측비용, 실험 목표 Step과 FIS 50% 시점.

**E1/E2 정식 실행 차단 조건:** 동일 모델/Rank/GPU/데이터/시작 상태/목표/위험 입력/Checkpoint 정책/Image Digest/Storage 사양이 고정되지 않음; C에서 Spot 대상 불명; 최신 Restore/4Rank 회귀검증 미완; 결과 계측이 불완전; 사용자 비용 승인 부재.

**결과물:** 실행 매트릭스·Frozen Manifest·정책 민감도 CSV/그림·실제 Run별 UID 로그/Checkpoint 영수증/VM 가격 장부·실패 목록·최종 그래프. 원시 자료는 서버에 보존하며 Secret/인증키를 외부 공유 파일에 포함하지 않는다.

## 9. 참고 자료와 근거의 범위

- [D1] O. Desai et al., *Cost-Efficient Training and Checkpointing for Large Models on Preemptible Cloud VMs*, EuroMLSys 2026, §4.1–§4.3, §5. https://doi.org/10.1145/3805621.3807617
- [D2] SkyPilot, *spot-traces*, 공개 AWS availability/preemption 시계열. https://github.com/skypilot-org/spot-traces
- [D3] AWS, *Tutorial: Test Spot Instance Interruptions using AWS FIS*. https://docs.aws.amazon.com/fis/latest/userguide/fis-tutorial-spot-interruptions.html
- [D4] PyTorch, *Distributed Data Parallel Design Note*. https://docs.pytorch.org/docs/stable/notes/ddp.html
- [I1] `261118_ProgressReport.pptx`, 평가 계획 3–7페이지(사용자 제시 기준).
- [I2] `IEEE_Access_Evaluation_Review_20261118.txt`, 실제 정책 분기·구현/기존 수용검사 범위.
- [I3] `evaluation_readiness_v3_20261118.txt`, 4-Rank 실험 가드·B/C clock 조건.
- [I4] `evaluation_validation_update_20261118.txt`, 4-Rank 기능 수용 및 gp3·Restore/연속교체 현황.
- [I5] `risk_reconfiguration_optimization_review_20261118.txt`, 첫 교체·재학습·직렬 교체 및 미검증 최적화 제안.

**논문 작성 시 주의:** D1의 비동기 Checkpoint 방식과 현재 CRIU 비용 기반 실제 구현을 같은 알고리즘이라고 표현하지 않는다. 출처/입력 가정이 다른 정책 민감도는 **계산상 정책 결정**, E1/E2는 **실제 운영 측정**으로 표기한다.
