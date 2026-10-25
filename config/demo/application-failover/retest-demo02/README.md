# ResNet-50 재검증: demo02

기존 demo 리소스와 NFS 파일을 보존하는 별도 실행 세트입니다. namespace는 fluidcr-realign-121040 그대로 사용합니다. Namespace 생성/삭제 YAML은 없습니다.

## MGMT에 전달

이 폴더 전체를 MGMT의 `/root/gpu-fluidcr-demo/retest-demo02`에 업로드합니다. 로컬 Windows 경로를 MGMT Bash에서 사용할 수는 없습니다.

PowerShell에서 실행하는 예시입니다. SSH 사용자/주소는 실제 MGMT 접속 설정에 맞춥니다.

```powershell
scp -r "C:\Users\JeongSeungJun\Desktop\졸업준비\Codex작업\연구실 추가 작업\GPU workload yaml파일\retest-demo02" root@192.168.30.100:/root/gpu-fluidcr-demo/
```

## 실행 전 조건

- 새 Karmada controller-manager가 리더이며 `pvSync`, `pvMigration`, `pvCleanup` 비활성화 설정을 유지합니다. 이 설정은 다른 legacy PV 데모에도 영향을 주므로 여기서 임의로 되돌리지 않습니다.
- standalone PV controller 이미지 `managed-pv-85f3598`과 이미 검증한 System/Stateful/FluidCR 컨트롤러를 유지합니다. 설치 및 과거 장애 복구 패치를 반복하지 않습니다.
- 양쪽 GPU 노드 Ready, GPU 여유 1개, onpre1 GPU 노드 uncordon 상태여야 합니다.
- 기존 demo 학습 Pod는 없어야 합니다. 이전 PVC/PV와 NFS 파일은 삭제하지 않습니다.
- 이번 이름은 r50-fluidcr-demo02, 체크포인트 경로는 `/mnt/nfs/gpu-fluidcr-demo/r50-fluidcr-demo02`입니다. 데이터셋만 기존 CIFAR-10을 공유합니다.
- 이 세트는 한 번의 새 실행용입니다. 이미 demo02를 실행했다면 재적용하지 마세요. 준비 Job은 경로가 이미 있으면 실패하여 이전 체크포인트 재사용을 막습니다. 기존 완료 Job의 재적용은 검사를 다시 실행하지 않으므로 아래 조회도 먼저 수행합니다.

MGMT Bash:

```bash
export NS=fluidcr-realign-121040
export APP=r50-fluidcr-demo02
: "${KARMADA_KUBECONFIG:?}" "${ONPRE1_KUBECONFIG:?}" "${AWS_KUBECONFIG:?}"
for KC in "$KARMADA_KUBECONFIG" "$ONPRE1_KUBECONFIG" "$AWS_KUBECONFIG"; do
  kubectl --kubeconfig="$KC" -n "$NS" get sts "$APP" --ignore-not-found
  kubectl --kubeconfig="$KC" -n "$NS" get trainingpolicy "$APP" --ignore-not-found
done
for KC in "$ONPRE1_KUBECONFIG" "$AWS_KUBECONFIG"; do
  kubectl --kubeconfig="$KC" -n "$NS" get pvc "dataset-$APP-0" "checkpoint-$APP-0" --ignore-not-found
  kubectl --kubeconfig="$KC" -n "$NS" get job "$APP-prepare" --ignore-not-found
done
```

리소스가 표시되거나 명령에 오류가 있으면 새 배포를 중단하고 기존 demo02 상태부터 확인하세요.

## 실행 순서

이 폴더의 [application-failover-run.md](application-failover-run.md)를 1절부터 순서대로 실행합니다. 이전 상위 폴더의 실행 가이드와 섞지 않습니다.

1. 환경 변수 설정과 노드 확인.
2. onpre1 준비 Job 및 PV, Karmada ConfigMap/Policy/StatefulSet 등록.
3. demo02 이름을 명시한 enroll, workload UID label 반영 확인 후 replicas=1.
4. onpre1 학습 로그에서 ResNet-50 및 step 증가 확인.
5. 새 checkpoint CR 생성. Completed, durableRef, SHA-256, exportedAt 확인.
6. application failover 활성화, onpre1 GPU cordon, trigger 실행. 별도로 Pod를 삭제하지 않습니다.
7. PVMigration Completed, AWS PVC Bound, RestoreRequest Verified, 복원 step 및 이후 step 증가 확인.

AWS PVC는 미리 수동 생성하지 않습니다. PVMigration이 PV를 준비하고 System이 volumeClaimTemplates를 바탕으로 대상 PVC를 준비하는 자동 경로를 검증합니다.

이 세트는 로컬 YAML/참조 검증만 완료했으며, 실제 클러스터 재실행 성공은 아직 검증하지 않았습니다. CR 상태를 강제로 바꾸거나 webhook을 우회하지 않습니다.
