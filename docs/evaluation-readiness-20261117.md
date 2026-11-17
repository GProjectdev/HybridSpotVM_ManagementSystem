# 2026-11-17 revised evaluation readiness

The latest plan is `Hybrid_DDP_IEEE_Access_Experiment_Plan_v2.md`, with the
matching Codex-plan PDF. It supersedes the older N1/N2 and F1–F4 matrix.

The protocol, source hashes, workload sizing and current GO/NO-GO evidence are
under [experiments/ieee-access-v2](../experiments/ieee-access-v2/README.md).
N0 has no periodic checkpoints; N1 uses Adaptive. All S0/S1/P0/P1 arms use the
same Adaptive implementation and common recovery route; only risk-based VM
composition changes. An earlier Fixed interval calibration is no longer a
required experiment and its waiting jobs were stopped.

Functional validation, the normal sizing pilot and formal evaluation results
must remain separate. A forward switch and verified restore do not establish
that a natural interruption during a subsequent in-flight switch recovers.
Do not manually change failed or pending CRs to Completed/Verified.

`RecoveryReconciler` runs in `spot-vm-management-controller`. A completed
historical cleanup receipt survives later policy generations. A replacement
retains its creation operation annotation. Retirement in a later operation
requires an independent Completed/current-generation origin receipt naming
that exact NodeProvision UID, policy UID, workload UID and cluster. It also
requires fresh UID-bound Running evidence for the current world before the
existing restore/source-fencing/deletion gates execute. No live annotation is
rewritten and no finalizer is forced.

Use Buildah and registry-verified digest references for release. Preserve local
changes and infrastructure. Clean only run-owned NodeRequests/Workloads and
UID/provider-matched NodeProvisions, then verify all EC2/EBS/key pairs. Preserve
NFS, PVC/PV and evidence. Remove only this run's webhook selector additions.
