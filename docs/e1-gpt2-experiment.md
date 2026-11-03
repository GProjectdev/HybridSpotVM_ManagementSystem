# E1 GPT-2 Small: execution and evidence

## Scope

Run two-rank GPT-2 Small on onpre1, export a full-world checkpoint, change only
the StatefulSet PropagationPolicy to aws, observe cold target capacity and shared
PVC rebinding, then check restored learning. This is NOT a Spot interruption,
checkpoint-interval comparison, or data-copy benchmark. AWS target uses two
On-Demand GPU VMs. onpre1 GPU workers are AWS-hosted VPN-joined testbed nodes.

`evaluation/e1_experiment.py` uses Python standard library and kubectl. No image
rebuild is needed for this driver. Retain the deployed v5.0.3 System/PV controllers,
stateful-member v5.0.1, compatible runtime, payload and group-control settings.

The source StorageClass is **e1-aws-nfs**, NOT legacy-nfs-client. Source dataset,
model and checkpoint use **10.50.1.31**. Keep WireGuard and NFS running. Both source
GPU workers need compatible CRI-O/CRIU/CUDA runtime and writable checkpoint mounts.
The real workload validates reads/writes; a Bound probe PVC alone does not.

Existing RIC `persistentvolume-status` was patched in place. Do not reapply the
old RIC kustomization blindly: a second PV status interpreter conflicts.

## MGMT commands

Run in a persistent SSH/tmux terminal. Do not put another experiment in
fluidcr-realign-121040. The driver rejects existing workload/migration objects;
inspect stale resources rather than deleting a whole namespace.

```bash
cd /root/hybridspot-validation/System
git status --short
git fetch origin restore-automation-20260928
git merge --ff-only origin/restore-automation-20260928

export ONPRE1_KUBECONFIG=/root/on-premise/member/onpre1.kubeconfig
export MGMT_KUBECONFIG=/etc/kubernetes/admin.conf
export AWS_KUBECONFIG=/root/aws-cluster/aws.kubeconfig
export KARMADA_KUBECONFIG=/etc/karmada/karmada-apiserver.config
export RUN="e1g-$(date -u +%m%d%H%M%S)"
export EVIDENCE="$PWD/evidence/e1/$RUN"

python3 evaluation/e1_experiment.py render \
  --run-id "$RUN" --out "$EVIDENCE" \
  --policy-source evaluation/policy-source.json

jq '{run_id,model,world_size,storage_class,nfs_server,training_image,goal_steps}' "$EVIDENCE/run.json"
printf 'RUN=%s\nEVIDENCE=%s\n' "$RUN" "$EVIDENCE"

python3 -u evaluation/e1_experiment.py execute \
  --evidence "$EVIDENCE" --confirm "$RUN"
```

policy-source.json must be the reviewed policy template with valid AWS capacity,
credential-reference and networking settings, not a kubeconfig or Secret.
Render is offline and creates no resources. Inspect its JSON manifests before
execute. `--confirm` authorizes resource creation and the AWS move; it is not
an interactive prompt. Do not execute twice with the same evidence directory.

Default: 100 warmup steps per rank, then checkpoint, migrate, and observe 50 more
steps after restore on both ranks. goal_steps=10000 is an upper guard, NOT the
experiment completion criterion. Source wait 20 min, checkpoint export wait
20 min, move wait 40 min; these are failure limits, not promised runtimes.
API polling and final collection add time. No FIS or risk-rate changes occur.

## Watch / collect / download

In another MGMT shell, set EVIDENCE to the exact printed path:

```bash
tail -n 30 -F "$EVIDENCE/timeline.jsonl"
```

The execute command collects while running, then collects final state and prints
`DOWNLOAD: /root/hybridspot-validation/System/evidence/e1/<RUN>-<number>.tar.gz`.
Normal completion is `candidate_requires_evidence_review`, not a paper-ready
success declaration. A timeout/Ctrl-C records failure and also attempts packaging.
VMs, Pods and PVCs are retained on BOTH success and failure; billing continues.
Do not run the old checkpoint.py cleanup on this E1 experiment. After collecting,
perform separately reviewed UID-scoped teardown; keep existing onpre1 workers.

After the execution process has exited, collect newer diagnostic state if needed:

```bash
python3 evaluation/e1_experiment.py collect --evidence "$EVIDENCE"
# Offline packaging if the API is unavailable:
python3 evaluation/e1_experiment.py pack --evidence "$EVIDENCE"
```

Do not run collect/pack concurrently with execute: files could change mid-archive.
Download the **latest printed tar.gz** through your existing MGMT SSH/SFTP entry,
or in Windows PowerShell (replace MGMT_ALIAS and the exact archive filename):

```powershell
scp MGMT_ALIAS:/root/hybridspot-validation/System/evidence/e1/EXACT_ARCHIVE.tar.gz "$HOME/Downloads/"
```

Send that archive for analysis. No multi-GB checkpoint archives, dataset/model
files, SSH private keys, Kubernetes Secrets or kubeconfig files are needed.
Resource snapshots use allowlisted fields. Raw trainer/controller logs and detailed
errors stay under `private/` and are excluded from the archive. Review/redact those
separately if failure diagnosis needs them. The archive contains structured training
events, internal IPs, topology and rendered manifests: inspect before sharing.

## What the archive supports

| Evidence | Paper/presentation use |
| --- | --- |
| run.json, rendered manifests | Exact model, data path, code hash and placement conditions |
| timeline.jsonl / timeline.csv | Request and observer milestones; failure/intervention history |
| private/trainer-logs by cluster/Pod UID (local only) | Original logs, excluded from the sharing bundle |
| training-events.jsonl / progress.csv | Rank 0/1 global-step vs UTC, rollback, resume/update audits |
| snapshots: nodes, NodeProvisions, Pods | Source/target topology and cold capacity timeline |
| snapshots: PVMetadata, PVMigration, PV/PVC | UID-bound shared NFS rebinding, NOT a data-copy claim |
| checkpoint-exported.json, RestoreRequest/Plan snapshots | Full-world artifact and restore identity |
| deployment/Pod images | Deployed image identities; raw controller logs stay in private/ |
| sha256.json | File integrity of the evidence bundle |

Prepare a migration timeline figure, two-rank progress figure, before/after
topology, and acceptance table after archive review. Stage durations may overlap;
do not sum them blindly. Distinguish controller timestamps from polling observations.
Polling has gaps, controller restarts/log rotation can lose evidence, and local
clock skew must be checked before high-precision timing claims. Unproven intervals
remain unknown. Do not subtract abnormal gaps or label an estimated run as measured.

The candidate gate checks restored step >0, matching restore steps, successful
rank-local model/optimizer/RNG/step audit and optimizer-update audit, 50 further
steps, no source Pods remaining and controller-verified active placement in AWS.
Offline review must still link both ranks to the same checkpoint and verify all
volume receipts and source fencing; Pod Running alone is never a pass.
