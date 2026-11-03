# E1 Spot target release v5.0.4

Source: two existing ONPRE1 GPU workers. Target: two newly provisioned AWS
Spot workers. This is not an all-Spot source environment. No On-Demand fallback.
Keep the source workers, NFS and WireGuard running throughout migration.

The group placement controller now honors explicit fixedOnDemand composition.
Without explicit composition, existing all-On-Demand bootstrap is preserved.
E1 renders minOnDemand=0 and fixedOnDemand=0. Replacement and periodic
checkpointing remain disabled. The driver waits for UID-bound pending AWS
placement before releasing policy suspension. Member snapshots no longer query
Karmada-only ResourceBinding and PropagationPolicy resources.

## Build and publish on MGMT

```bash
cd /root/hybridspot-validation/System
git fetch origin restore-automation-20260928
git merge --ff-only origin/restore-automation-20260928
docker build --build-arg COMPONENT=policy-manager \
  -t docker.io/jeongseungjun/hybrid-spot-vm-system:policy-manager-v5.0.4 .
docker push docker.io/jeongseungjun/hybrid-spot-vm-system:policy-manager-v5.0.4
```

Only deploy after build and push succeed. No other component image changes.

```bash
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system \
  set image deployment/policy-manager \
  manager=docker.io/jeongseungjun/hybrid-spot-vm-system:policy-manager-v5.0.4
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system \
  rollout status deployment/policy-manager --timeout=300s
```

## Preconditions

- Finish scoped cleanup of the old E1 workload, policy and target NodeProvisions
  in both Karmada and member clusters. Do not delete shared namespaces or source
  worker NodeProvisions. Deleting only member requests can cause recreation.
- Two source nodes must be Ready with allocatable GPU=1 each.
- At least 8 vCPU of G/VT Spot quota must be free; quota does not guarantee supply.
- All four kubeconfig variables must be exported, as in e1-gpt2-experiment.md.
- Use a fresh RUN. Never resume the interrupted evidence directory.
- Old evaluation configurations must not be used to render this Spot-only target.

## Run

```bash
export RUN="e1s-$(date -u +%m%d%H%M%S)"
export EVIDENCE="$PWD/evidence/e1/$RUN"
python3 evaluation/e1_experiment.py render \
  --policy-source evaluation/policy-source.json \
  --run-id "$RUN" --out "$EVIDENCE"
jq '.spec.policy, .metadata.annotations' "$EVIDENCE/policy.unbound.json"
python3 -u evaluation/e1_experiment.py execute \
  --evidence "$EVIDENCE" --confirm "$RUN"
```

After move intent, verify both generated NodeProvisions have spec.marketType=Spot
and record their EC2 instance IDs. Preserve the printed DOWNLOAD tar.gz. A candidate
result still requires restore audit review; failures and manual interventions must
not be reported as successful automatic migration. The runner does not delete VMs.
