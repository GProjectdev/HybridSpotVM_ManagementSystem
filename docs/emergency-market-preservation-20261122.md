# Emergency recovery market preservation (2026-11-22)

Researcher instruction: 정책에 따라 행동할 수 있게 Spot은 Spot으로 On-demand는 On-demand로 처리.

The policy-selected deployed market is preserved during interruption recovery.
A Spot notice/rebalance no longer forces OnDemand. Planned risk/price market
changes remain in the ordinary policy path and are serialized against recovery.
The change does not evaluate a new risk forecast in the emergency path or alter
paper-eq6-7, checkpoint timing, archive selection, source fencing or Verified gates.
OnDemand is not a valid source for a Spot interruption event. This change does
not invent a generic OnDemand failure detector. Planned OD capacity and the
existing explicit fixed-composition routes retain their existing behavior.

Both group and partial emergency producers now request Spot. Same-market partial
capacity and cleanup require independently UID/provider/event-bound emergency
evidence. Planned same-market operations remain rejected. Legacy emergency
Spot-to-OD intents remain readable solely for compatibility and safe retirement;
new emergency producers do not emit them. No live operations existed at upgrade.

This is a controller-version amendment for the next C attempt, not a retrospective
change to A/B02/C01. The previous aborted results and manifests remain unchanged.
C02 bootstrap stopped before GPU provisioning because its qualified OnDemand
price receipt was not copied. Preserve its operational stop/cleanup evidence;
do not produce scientific metrics for it. The next runner must verify this input
before bootstrap and before any paid provisioning.

## Group event continuity follow-up

The actual C retry confirmed a replacement Spot provider, but group request
creation stopped when the watcher republished the same interruption with a newer
event ID. The partial route already had a provider/signal/notice-bound successor
check; the group route still compared the event ID literally. The run was stopped
for repair and is not used for scientific performance analysis.

Group intent now captures the original emergency instance/signal/notice binding.
A later same-provider forward notice can continue the immutable original intent;
foreign provider/UID, backwards notice, signal downgrade, or unbound legacy
successor remain rejected. A CPU regression covers original intent preservation,
replacement Ready, successor event, actual RestoreRequest creation, and negatives.
Existing archive selection/fencing/Verified gates are unchanged.
