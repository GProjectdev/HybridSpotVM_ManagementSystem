# IEEE Access evaluation v2 — 2026-11-17 revision

The source plan is `Hybrid_DDP_IEEE_Access_Experiment_Plan_v2.md` and its
Codex-plan PDF. `config/plan_sources.json` records their hashes. This supersedes
the older N1/N2 and F1–F4 Fixed/Adaptive matrix. No formal results exist here.
JSON-formatted `.yaml` files are valid YAML; they describe the protocol and are
**not Kubernetes objects to apply**.

- E1: N0 = 2 OD without periodic checkpoints; N1 = 1 OD + 1 Spot with Adaptive.
  This compares operational strategies, not an isolated Spot-price effect.
- E2: S0/S1 = Static, P0/P1 = risk-aware; all use the same Adaptive method.
  S1/P1 share the scheduled-event rule. Compare no-event overhead as well.
- Use one historical risk hour per real hour. Lock the 28-day history, held-out
  seven days, selection rule and forecasts before paid E2 outcomes. Never call
  US V100 availability decreases measured Seoul A10G eviction probabilities.
- Keep the FIS target confined to one run-owned Spot ID and tag. If the policy
  already released Spot, record exposure removal and skip FIS.
- Every formal attempt needs fresh namespace/PVC and starts from the same model,
  seed, data and target. The normal pilot reused test workers only for sizing;
  it does not measure formal request-to-completion cost.

See `reports/preflight_report.md` for evidence, unresolved gates and cleanup.
Do not feed these new run names into the old hard-coded evaluation runner.
`evaluation/export_v2.py` can export raw rank events; it does not certify run
completion or compute cost. A run-aware runner and metric validation must pass
before the first formal run.

Protocol controls already present in source:

- Static: `spec.policy.fixedOnDemand: 1`. Proposed: omit that field.
- N0: annotation `training.dcnlab.com/periodic-checkpoint: disabled`.
- Common full-group routing: annotation
  `training.dcnlab.com/planned-partial: disabled` in **all E2 arms**. Verify this
  route before E2; otherwise Static group recovery vs Proposed partial recovery
  would change more than the VM decision switch.
- Do not set the global suspend annotation to implement N0: it also prevents
  VM management/recovery. Periodic disable leaves emergency behavior available.
- Adaptive is the existing implementation. Do not enable the asynchronous
  paper profile using synchronous CRIU/export durations as fabricated inputs.

Raw artifacts, credentials and model/data files stay outside Git. Missing event
measurements remain missing; rank steps and replayed tokens must not be summed.
