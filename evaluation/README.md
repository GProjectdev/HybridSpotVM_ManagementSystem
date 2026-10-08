# ResNet18 / CIFAR-10 평가 도구

평가는 [독립 실행 가이드](../docs/evaluation-separate-guide.md)에 따라 각각 진행한다.
비용은 cost.py(A/B), 주기는 checkpoint.py(F60/F300/F600/D)를 사용한다.
설정과 증거를 분리하고 이전 실행의 VM 정리가 끝난 뒤 다음 실행을 시작한다.

[한국어 단계별 실행 가이드](../docs/evaluation-resnet18-guide.md)를 먼저 읽으세요.

- `run.py`: 설정으로 실행별 YAML 생성, 준비, 학습 실행/종료, UID 제한 정리
- `resnet18/train_resnet18.py`: FluidCR 연동 ResNet18 DDP 학습
- `resnet18/Dockerfile`: Buildah 학습 이미지 빌드
- `collect.py`, `analyze.py`: 로그/리소스 증거 수집, CSV 집계
- `fis.py`: 특정 실행의 Spot VM 하나에 한정한 명시적 선점 실험
- `config.example.json`: NFS, 이미지, 목표 Step 등 개인 설정의 원본
- `policy-source.example.yaml`: 기존 TrainingPolicy가 없을 때 AWS 설정을 채울 원본 템플릿(JSON 문법의 유효한 YAML)

namespace는 `fluidcr-realign-121040`만 사용합니다. CIFAR-10은 기존 NFS에서 읽기 전용으로 사용하며, Checkpoint PVC는 실행별로 분리합니다.

`render`는 `member.yaml`, `karmada.yaml`, `policy.yaml`을 생성합니다. UID 바인딩 때문에 생성한 파일을 일괄 apply하지 말고 `prepare`로 배포하세요. 기존 정책이 있으면 `capture-policy`가 우선이며, 없으면 `policy-source.example.yaml`을 `policy-source.json`으로 복사해 환경별 항목을 채우세요. placeholder는 실행할 수 없습니다.

이 도구는 로컬 파일 생성/단위 테스트와 실제 GPU 평가를 구분합니다. `prepare`는 클러스터 리소스를 생성하고, `start`는 유료 VM 생성을 유발합니다. `fis.py start`는 실제 VM 중단을 유발합니다. `cleanup`은 해당 실행의 VM 종료를 요청하지만 PVC를 보존합니다. Pod 종료만으로 과금이 끝나지 않습니다.

```bash
python3 -B -m unittest discover -s evaluation -p 'test_*.py' -v
python3 -B -m unittest discover -s evaluation/resnet18 -p 'test_*.py' -v
python3 evaluation/run.py --help
```
