# 2026-11-17 revised evaluation readiness

The active protocol is recorded in `experiments/ACTIVE_PROTOCOL.json`. The
2026-11-18 methodology review is
[here](../experiments/ieee-access-final-20261117/EVALUATION_PROTOCOL_20261118.txt).
The original presentation E1/E2 are implementation evidence I1/I2. Its old E3
is superseded by the new quantitative E1 (operating cost/time) and E2 (the same
Hybrid configuration with/without one interruption). The earlier v2
Static/Proposed matrix is historical and must not be dispatched by mistake.

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

The final added-requirement review is [IEEE_Access_Evaluation_Review_20261118.txt](../experiments/ieee-access-final-20261117/IEEE_Access_Evaluation_Review_20261118.txt). Core six Runs precede replication/stretch and conditional R2/R3 comparisons. Formal results are NOT_RUN; the G6 ResNet acceptance stopped before FIS and new-VM restoration remains pending.
