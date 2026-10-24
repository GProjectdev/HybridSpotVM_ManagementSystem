#!/usr/bin/env python3
"""Render or apply the one-rank application-failover demo; kubectl runs only on explicit commands."""

import argparse
import json
import os
import re
import subprocess
import sys
from datetime import datetime

NS = "fluidcr-realign-121040"
APP = "r50-fluidcr-demo"
GROUP = "training.dcnlab.com/"
POLICY = "trainingpolicies.training.dcnlab.com"
RUNTIME = "trainingruntimes.training.dcnlab.com"
CHECKPOINT = "fluidcrmigrations.fluidcr.dcnlab.com"
BINDING = "resourcebindings.work.karmada.io"


class DemoError(Exception):
    pass


def require(condition, message):
    if not condition:
        raise DemoError(message)


class Kubectl:
    def __init__(self, context=None, kubeconfig=None):
        require(bool(context or kubeconfig), "explicit kubeconfig or context required")
        self.prefix = ["kubectl"]
        if kubeconfig:
            self.prefix += ["--kubeconfig", kubeconfig]
        if context:
            self.prefix += ["--context", context]
        self.prefix += ["--request-timeout=30s"]

    def run(self, args, document=None):
        try:
            result = subprocess.run(
                self.prefix + args, input=None if document is None else json.dumps(document),
                text=True, encoding="utf-8", capture_output=True, timeout=45, check=False,
            )
        except (OSError, subprocess.TimeoutExpired):
            raise DemoError("kubectl unavailable or timed out") from None
        # kubectl/auth-plugin diagnostics may contain credentials. Never echo them.
        require(result.returncode == 0, "kubectl failed; inspect the selected context separately")
        try:
            return json.loads(result.stdout) if result.stdout.strip() else {}
        except ValueError:
            raise DemoError("kubectl did not return JSON") from None

    def get(self, resource, namespace=None, name=None):
        args = ["get", resource]
        if name:
            args.append(name)
        if namespace:
            args += ["--namespace", namespace]
        return self.run(args + ["-o", "json"])

    def create(self, document):
        return self.run(["create", "-f", "-", "-o", "json"], document)


def uid(obj):
    value = obj.get("metadata", {}).get("uid")
    require(bool(value) and not obj["metadata"].get("deletionTimestamp"), "live object UID required")
    return value


def workload_ref(sts):
    return {"apiVersion": "apps/v1", "kind": "StatefulSet",
            "name": sts["metadata"]["name"], "uid": uid(sts)}


def single(items, message):
    require(len(items) == 1, message)
    return items[0]


def labels(policy, role):
    return {GROUP + "managed-by": "hybridspotvm-system", GROUP + "policy": policy["metadata"]["name"],
            GROUP + "policy-uid": uid(policy), GROUP + "role": role}


def emit_or_create(args, kube, documents):
    if args.apply:
        for document in documents:
            created = kube.create(document)
            print(json.dumps({"kind": document["kind"], "name": document["metadata"]["name"],
                              "uid": uid(created)}))
    else:
        print(json.dumps({"apiVersion": "v1", "kind": "List", "items": documents}, indent=2))


def enroll(args, kube):
    require(args.source_cluster != args.target_cluster, "source and target clusters must differ")
    sts = kube.get("statefulsets.apps", args.namespace, args.workload)
    require(sts.get("spec", {}).get("replicas") == 0, "enroll requires initial StatefulSet replicas=0")
    ref = workload_ref(sts)
    policies = kube.get(POLICY, args.namespace).get("items", [])
    require(not any(p.get("spec", {}).get("workloadRef", {}).get("name") == ref["name"]
                    for p in policies), "workload is already enrolled")
    node = Kubectl(args.target_context, args.target_kubeconfig).get("nodes", name=args.target_node)
    require(not node.get("spec", {}).get("unschedulable"), "target node is cordoned")
    require(any(c.get("type") == "Ready" and c.get("status") == "True"
                for c in node.get("status", {}).get("conditions", [])), "target node is not Ready")
    document = {
        "apiVersion": "training.dcnlab.com/v1alpha1", "kind": "TrainingPolicy",
        "metadata": {"name": args.policy, "namespace": args.namespace,
                     "annotations": {GROUP + "periodic-checkpoint": "disabled"}},
        "spec": {"workloadRef": ref, "sourceCluster": args.source_cluster,
                 "expectedWorldSize": 1, "targetWorkers": 1,
                 "replacement": {"enabled": False},
                 "policy": {"minOnDemand": 1, "fixedOnDemand": 1},
                 "checkpoint": {"resume": True, "minIntervalSeconds": 600, "maxIntervalSeconds": 600},
                 "capacity": {"mode": "Existing", "aws": {"karmadaCluster": args.target_cluster},
                              "existingNodes": [{"rank": 0, "nodeName": args.target_node,
                                                 "nodeUID": uid(node)}]}}
    }
    emit_or_create(args, kube, [document])


def live_policy(args, kube):
    policy = kube.get(POLICY, args.namespace, args.policy)
    uid(policy)
    sts = kube.get("statefulsets.apps", args.namespace, args.workload)
    require(policy.get("spec", {}).get("workloadRef") == workload_ref(sts),
            "policy does not match current workload UID")
    spec = policy["spec"]
    require(spec.get("targetWorkers") == 1 and spec.get("expectedWorldSize") == 1,
            "this demo requires exactly one rank")
    require(spec.get("capacity", {}).get("mode") == "Existing", "capacity.mode must be Existing")
    source = spec["sourceCluster"]
    runtime_name = spec.get("runtimeRef", {}).get("name", policy["metadata"]["name"] + "-runtime")
    placement = policy.get("status", {}).get("placement", {})
    # Match ReadPolicyInput: initial discovery does not require a placement record.
    if (placement.get("verified") is True and placement.get("policyUID") == uid(policy)
            and placement.get("workloadUID") == uid(sts)
            and placement.get("initialSourceCluster") == source
            and placement.get("initialRuntimeName") == runtime_name
            and placement.get("activeCluster") and placement.get("runtimeRef", {}).get("name")):
        source = placement["activeCluster"]
        runtime_name = placement["runtimeRef"]["name"]
    require(bool(source), "source cluster missing")
    runtimes = kube.get(RUNTIME, args.namespace).get("items", [])
    matches = [r for r in runtimes
               if r.get("metadata", {}).get("labels", {}).get(GROUP + "policy-uid") == uid(policy)
               and r.get("metadata", {}).get("name") == runtime_name
               and r.get("spec", {}).get("workloadRef") == workload_ref(sts)
               and r.get("spec", {}).get("sourceCluster") == source]
    runtime = single(matches, "exactly one current policy-owned source runtime required")
    uid(runtime)
    require(isinstance(runtime["metadata"].get("generation"), int)
            and runtime["metadata"]["generation"] >= 1, "runtime generation missing")
    reports = [c["status"] for c in runtime.get("status", {}).get("clusters", [])
               if c.get("clusterName") == source and isinstance(c.get("status"), dict)]
    report = single(reports, "exactly one source runtime report required")
    require(report.get("observedGeneration") == runtime["metadata"].get("generation")
            and report.get("sourceWorldUID") == uid(sts),
            "source runtime identity snapshot is stale")
    pods = report.get("sourcePods", [])
    pod = single(pods, "one source Pod snapshot required")
    require(pod.get("rank") == 0 and pod.get("name") == args.workload + "-0"
            and pod.get("uid") and pod.get("nodeName"), "invalid source Pod snapshot")
    return policy, runtime, source, pod


def checkpoint(args, kube):
    policy, runtime, source, _ = live_policy(args, kube)
    require(len(args.name) <= 243 and re.fullmatch(r"[a-z0-9]([-a-z0-9.]*[a-z0-9])?", args.name),
            "invalid checkpoint name (must also fit the -placement suffix)")
    cp = {
        "apiVersion": "fluidcr.dcnlab.com/v1alpha1", "kind": "FluidCRMigration",
        "metadata": {"name": args.name, "namespace": args.namespace,
                     "labels": labels(policy, "checkpoint"),
                     "annotations": {GROUP + "checkpoint-id": args.name}},
        "spec": {"workloadRef": policy["spec"]["workloadRef"], "resume": True,
                 "container": runtime["spec"].get("container", "trainer"),
                 "ctrlPort": runtime["spec"].get("port", 8298),
                 "appCheckpointTimeoutSeconds": 300, "kubeletTimeoutSeconds": 300},
    }
    pp = {
        "apiVersion": "policy.karmada.io/v1alpha1", "kind": "PropagationPolicy",
        "metadata": {"name": args.name + "-placement", "namespace": args.namespace,
                     "labels": labels(policy, "placement")},
        "spec": {"resourceSelectors": [{"apiVersion": cp["apiVersion"], "kind": cp["kind"],
                                        "name": args.name}],
                 "placement": {"clusterAffinity": {"clusterNames": [source]}}},
    }
    emit_or_create(args, kube, [pp, cp])


def timestamp(value):
    if not isinstance(value, str):
        return False
    try:
        return datetime.fromisoformat(value.replace("Z", "+00:00")).tzinfo is not None
    except ValueError:
        return False


def ready_checkpoint(cp, policy, source, namespace):
    meta, spec = cp.get("metadata", {}), cp.get("spec", {})
    if (not meta.get("uid") or meta.get("deletionTimestamp") or meta.get("namespace") != namespace
            or not isinstance(meta.get("generation"), int) or meta["generation"] < 1
            or meta.get("labels", {}).get(GROUP + "policy-uid") != uid(policy)
            or spec.get("workloadRef") != policy["spec"]["workloadRef"]
            or spec.get("partialCheckpoint")):
        return False
    round_id = meta.get("annotations", {}).get(GROUP + "checkpoint-id")
    reports = [r for r in cp.get("status", {}).get("clusters", []) if r.get("clusterName") == source]
    if not round_id or len(reports) != 1:
        return False
    report = reports[0]
    if (report.get("phase") != "Completed" or report.get("observedGeneration") != meta["generation"]
            or not timestamp(report.get("completionTime")) or len(report.get("pods", [])) != 1):
        return False
    pod = report["pods"][0]
    if (pod.get("podName") != policy["spec"]["workloadRef"]["name"] + "-0"
            or not pod.get("podUID") or not pod.get("nodeName")
            or pod.get("phase") not in ("Resumed", "ContainerCheckpointed")
            or pod.get("checkpointID", round_id) != round_id
            or len(pod.get("checkpointFiles", [])) != 1):
        return False
    archive = pod["checkpointFiles"][0]
    sha = archive.get("sha256", "")
    return bool(re.fullmatch(r"[0-9a-f]{64}", sha)
                and archive.get("containerName") and archive.get("filePath")
                and archive.get("durableRef") == "file-store:" + namespace + "/sha256/" + sha
                and timestamp(archive.get("exportedAt"))
                and archive.get("checkpointID", round_id) == round_id)


def trigger(args, kube):
    require(args.confirm_workload == args.namespace + "/" + args.workload,
            "--confirm-workload must exactly match namespace/workload")
    policy, _, source, snapshot = live_policy(args, kube)
    target = policy["spec"].get("capacity", {}).get("aws", {}).get("karmadaCluster")
    require(target and target != source, "source is already the target cluster or target is missing")
    checkpoints = kube.get(CHECKPOINT, args.namespace).get("items", [])
    require(any(ready_checkpoint(cp, policy, source, args.namespace) for cp in checkpoints),
            "no ready durable full-group checkpoint for the current policy/workload")
    member = Kubectl(args.source_context, args.source_kubeconfig)
    pod = member.get("pods", args.namespace, snapshot["name"])
    require(uid(pod) == snapshot["uid"] and pod.get("spec", {}).get("nodeName") == snapshot["nodeName"],
            "source context/Pod identity does not match current runtime snapshot")
    node = member.get("nodes", name=snapshot["nodeName"])
    uid(node)
    require(node.get("spec", {}).get("unschedulable") is True, "source node must be cordoned first")
    bindings = kube.get(BINDING, args.namespace).get("items", [])
    ref = policy["spec"]["workloadRef"]
    binding = single([b for b in bindings if all(
        b.get("spec", {}).get("resource", {}).get(k) == v for k, v in ref.items())
        and b.get("spec", {}).get("resource", {}).get("namespace") == args.namespace],
        "exactly one ResourceBinding for current workload UID required")
    binding_uid = uid(binding)
    require(binding.get("spec", {}).get("clusters") and
            [c.get("name") for c in binding["spec"]["clusters"]] == [source],
            "ResourceBinding is not placed exclusively on source")
    annotations = binding["metadata"].get("annotations", {})
    require(not annotations.get(GROUP + "application-failover-trigger"),
            "ResourceBinding already has an application-failover trigger")
    rv = binding["metadata"].get("resourceVersion")
    require(bool(rv), "ResourceBinding resourceVersion missing")
    kube.run(["annotate", BINDING, binding["metadata"]["name"], "--namespace", args.namespace,
              GROUP + "application-failover-trigger=" + binding_uid,
              "--resource-version=" + rv, "-o", "json"])
    print(json.dumps({"binding": binding["metadata"]["name"], "triggerUID": binding_uid}))


def parser():
    result = argparse.ArgumentParser(description=__doc__)
    commands = result.add_subparsers(dest="command", required=True)
    for command in ("enroll", "checkpoint", "trigger"):
        sub = commands.add_parser(command, epilog=(
            "Each accessed cluster requires its own kubeconfig flag/environment variable "
            "or an explicit context. Ambient current-context alone is never used."))
        sub.add_argument("--karmada-context", help="optional context override in the Karmada kubeconfig")
        sub.add_argument("--kubeconfig", default=os.environ.get("KARMADA_KUBECONFIG"),
                         help="Karmada kubeconfig path (default: KARMADA_KUBECONFIG)")
        sub.add_argument("--namespace", default=NS)
        sub.add_argument("--workload", default=APP)
        sub.add_argument("--policy", default=APP)
        if command in ("enroll", "checkpoint"):
            sub.add_argument("--apply", action="store_true",
                             help="create objects; otherwise render JSON after live read-only lookups")
        if command == "enroll":
            sub.add_argument("--source-cluster", default="onpre1")
            sub.add_argument("--target-cluster", default="aws")
            sub.add_argument("--target-context", help="optional context override in the target kubeconfig")
            sub.add_argument("--target-kubeconfig", default=os.environ.get("AWS_KUBECONFIG"),
                             help="target kubeconfig path (default: AWS_KUBECONFIG)")
            sub.add_argument("--target-node", required=True)
        elif command == "checkpoint":
            sub.add_argument("--name", required=True)
        else:
            sub.add_argument("--source-context", help="optional context override in the source kubeconfig")
            sub.add_argument("--source-kubeconfig", default=os.environ.get("ONPRE1_KUBECONFIG"),
                             help="source kubeconfig path (default: ONPRE1_KUBECONFIG)")
            sub.add_argument("--confirm-workload", required=True,
                             help="namespace/workload; authorizes UID-bound failover annotation")
    return result


def main(argv=None):
    args = parser().parse_args(argv)
    try:
        connections = [("Karmada", args.kubeconfig, args.karmada_context)]
        if args.command == "enroll":
            connections.append(("target", args.target_kubeconfig, args.target_context))
        elif args.command == "trigger":
            connections.append(("source", args.source_kubeconfig, args.source_context))
        for role, config, context in connections:
            require(bool(config or context), role + " requires an explicit kubeconfig or context")
        kube = Kubectl(args.karmada_context, args.kubeconfig)
        {"enroll": enroll, "checkpoint": checkpoint, "trigger": trigger}[args.command](args, kube)
    except DemoError as exc:
        print("error: " + str(exc), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
