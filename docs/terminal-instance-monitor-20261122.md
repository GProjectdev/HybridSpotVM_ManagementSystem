# Operational watch: retired EC2 IDs (2026-11-22)

C05 was normally stopped and cleaned up after the safety monitor rejected an
exact-ID response that no longer contained a previously terminated FIS target.
The AWS reader already represented disappearance as `NOT_OBSERVED`, and normal
cleanup could reuse prior positive termination receipts. The training monitor's
current-ledger validator rejected that state before reaching its historical
accounting path. This is an operational failure, not a completed scientific run.

`validate_current_ledger` now accepts a fresh absent-ID response only with a
previous positive AWS termination receipt bound to the same Run policy UID,
NodeProvision UID/name/namespace and instance ID. Launch and original terminal
observation must precede the current query. Historical replay cannot borrow a
future termination receipt. The current row remains `NOT_OBSERVED`; its timestamp
is never rewritten into a new termination time. Accounting retains the original
positive termination boundary and all incurred cost.

Live and replacement VMs still require fresh provider ownership and state.
Unknown disappearance, foreign ownership, stale queries/prices, contradictory
live-after-terminal state, missing ledger rows, FIS target mismatches, deadlines,
space, cost and maximum simultaneous capacity remain fail-closed. Training and
successful-cleanup observation paths both disclose the historical receipt used.
No Controller, image, training setting, checkpoint/restore policy or risk model
changes are included in this operational amendment.

Validation: 70 CPU tests, including nine retired-ID regressions and existing
monitor/FIS/ownership/cleanup boundary tests. A read-only replay used the completed
C05 snapshot at 09:24:36 UTC and evaluation time 09:24:42 UTC. Old code reproduced
`stale/unknown/foreign current cleanup ledger`; amended code kept four live VMs
and nonzero incurred cost, with no safety-stop decision. The failing monitor's
exact fresh provider response was not persisted, so this is a contemporaneous
recorded-ledger reproduction, not a reconstruction of an unrecorded API response.
No scientific performance analysis of C05 was run, and its original stop record
and frozen package are unchanged.

The amendment must be included in a new Run's reviewed operational package.
It does not retroactively validate C05 or resume its terminated resources.
