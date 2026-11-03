# 설치 가이드

설치 및 사용의 단일 기준 문서는 [루트 README](../README.md)입니다.
main에는 restore-automation-20260928의 구현이 통합되었습니다.
새 설치는 main의 고정 commit으로 이미지를 빌드하고 의존 시스템 버전도 함께 기록하세요.

## 설치 순서

1. 일반 Kubernetes 기반 MGMT, upstream Karmada, 소스/대상 Member와 하이브리드 네트워크를 사전 구성합니다.
2. GPU runtime, NFS/StorageClass, artifact 저장소, AWS 권한/quota 및 외부 연동 시스템을 준비합니다.
3. Buildah로 컴포넌트별 이미지를 빌드/push합니다.
4. scripts/install-system.py로 CRD와 Karmada RBAC/RIC를 렌더링하고 검토 후 적용합니다.
5. 제한된 컨트롤러 kubeconfig Secret, 각 Member runtime collector 및 AWS SpotWatcher를 설치합니다.
6. MGMT 컨트롤러와 placement webhook을 설치하고 실제 endpoint에 맞는 TLS 및 Karmada webhook 등록을 검증합니다.
7. 사용자 소유 workload, 위험률 profile, UID-bound TrainingPolicy를 등록합니다.
8. 실제 checkpoint/restore와 학습 재개를 검증합니다.

설치 도구는 기본적으로 오프라인 렌더링만 합니다.
--apply에는 --kubeconfig와 --confirm-context가 필요합니다.
이전 설치 문서의 Docker 명령, 자동 정책 등록 또는 특정 실험 IP/이미지 태그는 신규 설치에 사용하지 마세요.
기존 운영 클러스터에서는 active migration을 종료하고 manifest/CRD를 백업한 뒤 단계별로 업그레이드하세요.

## 범위 밖

이 저장소는 Kubernetes 클러스터, Karmada 서버, VPN, NFS 서버, IAM 자격증명,
NodeProvisioner, FluidCR, Stateful Migration 또는 PV Migration 자체를 자동 설치하지 않습니다.
각 의존 저장소에서 호환되는 CRD/controller/runtime을 설치해야 합니다.
다른 backend는 단순 이미지 교체가 아니라 README의 API 및 안전성 계약에 맞춘 어댑터와 검증이 필요합니다.
Karmada 자체의 코드나 controller-manager 이미지를 수정하지 않습니다.
