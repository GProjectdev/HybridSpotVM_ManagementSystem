# System readiness work, 2026-11-18

The next evaluation design is pending. This work checks execution prerequisites
and recovery correctness independently of the superseded E1/E2 schedule.
No quantitative performance result is inferred from an acceptance check.
`experiments/ACTIVE_PROTOCOL.json` records the previous plan as superseded and
formal dispatch as unauthorized pending the replacement plan; previous artifacts
remain available for reference. This registry is a record, not a cluster ACL.

## Confirmed fixes

The caller could create/start tagged FIS templates but lacked GetExperiment and
StopExperiment on experiment resources. The current AWS account now has a
separate inline policy allowing just these two operations on Seoul experiment
resources carrying the existing HybridSpotValidationRun authorization tag.
The prior policy and the narrow change receipt are retained privately under
`/home/ubuntu/system-hardening-20261118/`. Simulation changed both actions from
implicitDeny to allowed. A subsequent wait-only FIS probe confirmed actual start/get/stop acceptance
and stopped normally; its temporary template was deleted. This is API
authorization evidence, not a Spot interruption or GPU restore result.

The GPU attempt then exposed an additional authorization gap: StartExperiment
was allowed on the template ARN but not on the newly created experiment ARN.
A second narrow policy now permits StartExperiment on experiment resources only
with the matching request tag, retaining the existing resource-tag restriction
on templates. Preflight checks both resource types. See AWS policy examples,
"Example: Start an experiment":
https://docs.aws.amazon.com/fis/latest/userguide/security_iam_id-based-policy-examples.html

Denied AWS action/resource identifiers are now preserved in the atomic FIS intent
without serializing arbitrary stderr or credentials. Failure does not authorize
a blind retry.

The authorization family value `v6-readiness-20261117` is an IAM scope, not the
experiment date or trace provenance. A new run retains its actual UTC, run ID,
policy UID and exact target ARN. Do not change the date of evidence to fit IAM.
The earlier final FIS dispatcher also requires this explicitly reviewed tag and
records an atomic intent before creation/start to prevent blind duplicate faults.

The frozen historical-risk replay previously accepted only g5.xlarge prices.
It now accepts explicit execution product arguments and rejects prices for a
different instance type/AZ. The default remains g5.xlarge/ap-northeast-2c for
compatibility. For a G6 execution, pass:

```bash
python3 internal/collector/paper_sarima_replay.py \
  --forecasts FROZEN_FORECASTS.json --manifest LOCKED_MANIFEST.json \
  --start-utc REVIEWED_START_UTC --prices FRESH_PRICES.json \
  --execution-instance-type g6.xlarge \
  --execution-availability-zone ap-northeast-2c \
  --cert TRUSTED_SERVER_CERT --key PRIVATE_SERVER_KEY
```

The original trace/forecast hashes, causal cutoff and uncompressed clock do not
change. Actual price product metadata is separate from the historical US/V100
availability proxy. This is neither a new predictor nor measured Seoul hazard.
This host-side replay tool is not an upgrade of a currently running risk feed.

## Repeatable, read-only preflight

The tool does not read Secrets, logs, or Pod environment variables. It checks
all four APIs, Ready nodes/member registrations, required management and AWS
Deployments, the AWS spot-watcher DaemonSet and optional caller FIS simulations.
The requested output must be a new filename, preserving earlier receipts.

```bash
python3 scripts/system_preflight.py \
  --mgmt-kubeconfig /etc/kubernetes/admin.conf \
  --karmada-kubeconfig /etc/karmada/karmada-apiserver.config \
  --aws-kubeconfig /root/aws-cluster/aws1.kubeconfig \
  --onpre1-kubeconfig /root/on-premise/member/onpre1.kubeconfig \
  --fis-profile REVIEWED_PROFILE --account EXPECTED_ACCOUNT_ID \
  --fis-authorization-tag REVIEWED_AUTHORIZATION_FAMILY \
  --out /PRIVATE/preflight.NEW.json
```

An API being reachable is not sufficient when a node is NotReady, a Deployment
has not observed its current generation, or FIS observation/stop is denied.
PassRole/trust/target role permissions, actual GPU C/R, NFS durability and live
FIS behavior still need their own receipts. IAM role/SSO principals require
separate permission verification; this simulation currently supports IAM users.
No VM, role/policy, alarm or experiment is created by preflight.

## Current ownership contract

As documented in `v6-workflow-validation.md`, training completion does not delete
the user-owned NodeRequest. Whole-pool release still requires explicit quiesced
NodeProvision cleanup and provider-finalizer verification. An evaluation runner
performing that cleanup is not a new completion-driven controller feature.
Do not add implicit destructive lifecycle behavior while an evaluation design
is being revised. Preserve UID, source fencing, NFS/PVC/PV and restore gates.

## Live acceptance

The separately authorized G6 check uses two one-GPU VMs, one rank per VM,
ResNet-50/CIFAR-10, and a single exact-owned Spot interruption. It is limited to
one hour and an estimated GPU budget of USD 5, with cleanup beginning after
45 minutes. A temporary third VM is permitted only for replacement overlap.
Actual success requires a different replacement provider ID, Verified group
restore with source-fence evidence, both rank restore-state audits and subsequent
finite-gradient/changed-parameter updates. All exact-owned EC2 instances must
then terminate normally; checkpoint/PVC/PV/NFS evidence is retained.

The GPU attempt ran two one-GPU G6 workers, both ranks and durable checkpointing.
FIS template creation succeeded, but actual start was denied before interruption
because of the second IAM gap above. It then cleaned up normally: two EC2
instances terminated, both tracked root EBS volumes absent, and temporary
keypairs/credential clone/role policy/alarm/webhook exception removed. PVC/PV/NFS
and evidence were retained. The conservative GPU compute upper estimate was
USD 0.6757988857, pricing both workers at the OD rate from launch to termination
observation. This is not an invoice and excludes EBS/network/FIS/infrastructure.

After the permission correction, the no-fault FIS wait-only probe was started,
queried and stopped; no EC2 action or target was present. The temporary template
was deleted. Preflight passed all 26 checks. Neither this API probe nor the
pre-fault training/checkpoints prove new-VM ResNet restoration. That GPU recovery
acceptance remains unverified and must be run within a separately applicable
execution window; the initial paid validation deadline was not extended.

Private evidence: `/home/ubuntu/system-hardening-20261118/`.
A sanitized receipt set lives in `operations/system-hardening-20261118/`.
The earlier IEEE review remains a historical snapshot and was not rewritten to
make a failed acceptance appear successful.

Validation: evaluation tools 262 tests run (260 passed, 2 existing dependency
skips); preflight 5 passed; replay 4 passed. Targeted FIS/evidence tests were
rerun after the final diagnostic/projection changes. Core controller code,
CRDs, workload image and running controller image references were unchanged.


The observer also dropped actual Pod GPU limits from its safe projection. It
now retains allowlisted CPU/memory/storage/GPU limits without arbitrary resource
keys. Archived pre-cleanup Pod objects verified separate nodes and GPU request
and limit 1 for both ranks; the failed run reached committed step 1024 before
the denied fault request. This remains pre-fault evidence only. Local run
metadata was closed after independent provider termination verification, keeping
a backup of its previous running metadata.
