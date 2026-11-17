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

## Original-paper upgrade (2026-11-17)

`Cost_effective.pdf` §4.3 has now been checked. Select the opt-in
`spec.policy.method: paper-eq6-7`; legacy allocations keep their prior default.
Eq.6 discounts each shard's throughput by `exp(-lambda * horizonHours)`.
Eq.7 switches to OD when that ratio is below alpha **or** measured expected
loss exceeds `OD hourly price * onDemandDurationSeconds / 3600`.
With no valid loss observation, only the throughput leg runs. With a complete
durable checkpoint and UID-bound Verified recovery, the economic leg is added
automatically. Lost work uses actual checkpoint age, not half an interval.
Request-to-verification recovery latency is explicitly an operational proxy.
OD residency starts at the exact replacement's Verified full-group restore.
This is a fixed-world adaptation, without the paper's elastic DP scale-out.

New tools (offline preparation and read-only observation, not a live launcher):

```bash
python3 evaluation/ieee_v2.py prepare --help
python3 evaluation/ieee_v2_observe.py --help
python3 evaluation/ieee_v2.py metrics --help
```

Preparation derives a fresh namespace per run and writes suspended, unbound
policies. E2 requires a separately locked risk manifest. Observation stores
sanitized run/Pod-UID-scoped events; metrics deduplicate replay and require both
ranks' completion plus a reviewed, terminated-instance billing ledger for cost.
They leave missing stalls/state review incomplete rather than inventing values.
The legacy live runner must still not be used for the new matrix.

The 195-second causal binning fix is tested. A four-week fit / next-seven-day
preview now generates 168 forecasts; 65 cross the throughput switch threshold.
Numerical optimization uses a fixed 500-iteration budget; model, window, seasonal
period, alpha and data split are unchanged. This preview has declared relative
clock/population assumptions and remains **PREVIEW_NOT_LOCKED**. See
`reports/upgrade_checks.json` and the dated preflight addendum. No new paid VM
was started for this upgrade. Full-group GPU recovery/interruption acceptance
and live-run gates remain outstanding.

## Evidence and replay follow-up

`config/risk_replay_manifest.json` locks the existing forecast hash and the
first held-out hour, before any paid E2 outcome. It declares an external
US-V100 availability proxy, not observed Seoul eviction risk. The clock/TLS
binding is deliberately not activated now. The replay image accepts only the
current hour, uses TLS, expires after the frozen series, and preserves both
historical and live publication timestamps. It never refits the estimator.
Optional current execution prices are separate from the historical risk input;
stale/invalid supplied prices make the feed unavailable rather than silently
reusing a price. Without prices the policy remains throughput-only.

Preparation now emits separate files so applying prerequisites cannot bind a
zero-replica Workload too early or start the Proposed composition too early:

- `member-storage.json`, `karmada.json`: reviewed storage/prerequisite objects;
  the latter excludes the StatefulSet and risk profile.
- `risk.bootstrap.json`: common static lambda=.05 while initial VMs prepare.
- `policy.bootstrap.json`: suspended/unbound, initial 2OD for N0, 1OD+1Spot
  otherwise. Unsuspending can create paid VMs; it was not done in this upgrade.
- `workload.stopped.json`: create only after bootstrap VMs are Ready. Stage
  rank0 onto an actual OD node before starting rank1, as the pilot did.
- `risk.armed.json`: same locked replay for every E2 arm. Bind the per-run
  replay start clock after both ranks' first committed Step, switch from the
  bootstrap profile with exact UID/generation guards, and wait for fresh risk.
- `policy.unbound.json`: desired policy reference, **not** a second CR to apply
  over the live object. For P0/P1 remove only `fixedOnDemand` using a UID- and
  resourceVersion-tested patch after risk activation. S0/S1 remain fixed=1.

The first frozen risk value is already high. Enabling Proposed before this
bootstrap would change initial composition and defeat the common-start protocol.
Bootstrap and activation times must be retained in every run's event record.

Useful read-only/offline commands (from repository root):

```bash
python3 evaluation/ieee_v2.py prepare --config experiments/ieee-access-v2/config/run_config.json --policy-source /PRIVATE/current-capacity-template.json --risk-source experiments/ieee-access-v2/config/risk_replay_manifest.json --run-id p0-unique --arm P0 --out /PRIVATE/new-run
python3 evaluation/ieee_v2_observe.py --directory /PRIVATE/new-run --seconds 7200 --aws-profile new-account-v2 --aws-account-id 935317843233
python3 evaluation/ieee_v2_prices.py --profile new-account-v2 --account 935317843233 --ondemand-receipt /PRIVATE/current-public-od-receipt.json --out /PRIVATE/prices.json
python3 internal/collector/paper_sarima_replay.py --forecasts /PRIVATE/trace-forecast-preview.json --manifest experiments/ieee-access-v2/config/risk_replay_manifest.json --start-utc RUN_ACTIVATION_UTC --bind 0.0.0.0 --port 8443 --cert /TLS/tls.crt --key /TLS/tls.key --prices /PRIVATE/prices.json
```

Observer requires explicit Karmada/AWS kubeconfig environment variables; set
`MGMT_KUBECONFIG` to archive policy decision logs too. Refresh prices at least
every five minutes. The OD receipt must be reviewed/refreshed within 24 hours.
The replay certificate must cover the service hostname, and its CA must be
trusted by the Collector (e.g. mounted CA and `SSL_CERT_FILE`); never disable
verification. No replay Deployment or clock was activated in this follow-up.

The observer retains UID-bound policy/restore evidence and exact-instance EC2
observations after CR deletion. Missing instances are NOT_OBSERVED, not evidence
of termination. Polling termination time is an upper bound, not exact billing
end. Keep provider termination responses and reviewed price segments.

Opt-in runtime events identify application checkpoint pause, checkpoint saved,
and resume boundaries. Their union includes recovery waits, not just I/O stall;
unmatched boundaries remain incomplete. They do not measure NCCL degradation
before a coordinated pause. Unexpected natural failure uses actual provider,
watcher and last/next committed-Step evidence; a gap alone is not a CP stall.

New runs require both-rank matching final model/optimizer/data-contract hashes
and goal Step observations. Final hashing is excluded from useful training
completion time and remains part of billed VM lifetime. Protocol review remains
required even when hashes match. See `reports/evidence_upgrade.json`.

Before any paid run, bind fresh Namespace/Policy/Workload UIDs, extend only that
namespace's admission selector if necessary, verify injection, and install the
run-specific deadline/normal-cleanup procedure. **max_seconds in a rendered JSON
file does not itself stop EC2.** The new tools are not an unattended live runner.
Existing VM provisioning, C/R, UID/fencing and cleanup controllers are retained.
Full-group GPU acceptance and interruption-during-transition remain unresolved.
