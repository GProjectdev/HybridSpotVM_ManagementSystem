# Successful application completion

`TrainingRun.spec.completionPolicy: AllRanksSucceeded` opts into automatic cleanup. The default `Retain` preserves previous behavior. Select the trainer container and enable the execution gate before propagation. This setting is immutable.

FluidCR reports successful child exit with current Pod UID, node, rank, positive observed step and completion timestamp. The launcher holds the completed child stopped while its status endpoint remains alive. Runtime aggregates all expected ranks, and TrainingRun accepts only fresh observations matching the current workload, member workload and execution permit. It persists completion evidence, waits for checkpoint quiescence, scales the workload down through Karmada, waits for owned Pods to disappear, and retires only owned Cloud capacity. Shared output storage and external Member1 capacity are retained.

Completed means all supervised processes exited successfully and cleanup was confirmed. It does not certify a model target step, convergence or durable output; applications and acceptance checks must verify those separately. Failed exits, stale receipts and partial rank completion never authorize successful cleanup.
