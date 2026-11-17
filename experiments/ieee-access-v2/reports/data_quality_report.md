# Data quality — preflight only

- Functional replacement/debug runs and the independent normal pilot have different run IDs/namespaces/PVCs.
- Earlier elapsed-window sizing was rejected; only the later uninterrupted 900-second window sizes formal work.
- Raw events preserve Pod UID, attempt and replayed steps. Never sum both ranks or replayed tokens as useful work.
- No N0/N1/S0/S1/P0/P1 formal completion, cost savings or statistical significance is claimed.
- CR checkpoint wall time and asynchronous export time are recorded separately. Exact CP-only DDP stall needs additional control-event instrumentation.
- Some checkpoint CR pod entries report rank 0 for both pods. Use actual runtime rank/Pod UID mapping, not that field, for analysis; this inconsistency remains to be corrected.
- A natural Spot interruption contaminated the reverse-transition debug run. Retain the attempt; do not relabel it as no-failure or FIS success.
- Numeric account GPU quotas are not verified because IAM denies Service Quotas reads.
- Historic terminated instance records may disappear from a later EC2 response. Retain the earlier direct AWS terminated receipt and its timestamp; absence alone is not termination proof.
- Installed digest/rollout evidence is separate from actual GPU E2E evidence. AWS artifact desired=0 after cleanup cannot prove execution on a future worker.
