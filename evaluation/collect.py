"""Collect allowlisted Kubernetes evidence and EVAL_EVENT logs; stdlib only.

Usage: python collect.py --evidence DIR --interval 10 [--aws-status] [--once]
run.json is shared with eval.py. No kubeconfigs, environment values, raw Pod
specs, arbitrary log messages, or command stderr are written to evidence.
"""

import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import re
import subprocess
import time
from datetime import datetime, timedelta, timezone

NAMESPACE = "fluidcr-realign-121040"
REGION = "ap-northeast-2"
RUN_LABEL = "training.dcnlab.com/evaluation-run"
POLICY_LABEL = "training.dcnlab.com/policy"
PARENT_LABEL = "training.dcnlab.com/scheduled-parent"
PARENT_UID = "training.dcnlab.com/scheduled-parent-uid"
PREFIX = "EVAL_EVENT "
EVENTS = {"worker_started", "step", "restored", "completed"}
NAME = re.compile(r"[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}\Z")
INSTANCE = re.compile(r"i-[0-9a-f]{8,17}\Z")


def utc_now():
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def timestamp(value):
    if not isinstance(value, str):
        raise ValueError("timestamp must be a timezone-aware string")
    result = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if result.tzinfo is None:
        raise ValueError("timestamp requires a timezone")
    return result.astimezone(timezone.utc)


def load_run(evidence):
    with (Path(evidence) / "run.json").open(encoding="utf-8") as handle:
        run = json.load(handle)
    if run.get("namespace") != NAMESPACE:
        raise ValueError("run namespace must be " + NAMESPACE)
    if not isinstance(run.get("run_id"), str) or not NAME.fullmatch(run["run_id"]):
        raise ValueError("invalid run_id")
    if run.get("experiment") not in {"cost", "checkpoint"}:
        raise ValueError("experiment must be cost or checkpoint")
    if run.get("arm") not in {"A", "B", "F300", "D", "F60", "F600"}:
        raise ValueError("invalid experiment arm")
    if run.get("model") != "resnet18":
        raise ValueError("model must be resnet18")
    if type(run.get("goal_steps")) is not int or run["goal_steps"] <= 0:
        raise ValueError("goal_steps must be a positive integer")
    app = "eval-" + run["run_id"]
    if run.get("app") != app or run.get("policy") != app or run.get("risk") != app + "-risk":
        raise ValueError("app, policy and risk must be bound to run_id")
    if run.get("region", REGION) != REGION:
        raise ValueError("evaluation region must be " + REGION)
    if run.get("started_at"):
        timestamp(run["started_at"])
    return run


def event_from_message(message):
    if not isinstance(message, str) or not message.startswith(PREFIX):
        return None
    raw = json.loads(message[len(PREFIX):])
    if not isinstance(raw, dict) or raw.get("event") not in EVENTS:
        raise ValueError("invalid training event")
    if not isinstance(raw.get("run_id"), str) or not NAME.fullmatch(raw["run_id"]):
        raise ValueError("invalid event run_id")
    timestamp(raw.get("timestamp"))
    for key in ("global_step", "rank"):
        if type(raw.get(key)) is not int or raw[key] < 0:
            raise ValueError("invalid event " + key)
    result = {key: raw[key] for key in ("timestamp", "global_step", "rank", "run_id", "event")}
    for key in ("attempt_id", "session_id", "session"):
        if key in raw:
            if not isinstance(raw[key], str) or not NAME.fullmatch(raw[key]):
                raise ValueError("invalid event session identity")
            result[key] = raw[key]
    if "step_seconds" in raw:
        value = raw["step_seconds"]
        if type(value) not in (int, float) or not math.isfinite(value) or value < 0:
            raise ValueError("invalid event step_seconds")
        result["step_seconds"] = value
    return result


# Explicit field projection also applies to nested checkpoint result snapshots.
# In particular, env, commands, messages, provider config and credentials vanish.
SAFE_FIELDS = set("""
apiVersion kind name namespace uid generation creationTimestamp observedGeneration
workloadRef policyRef runtimeRef riskRef sourceCluster targetCluster nodeName
instanceId instanceID instanceType capacityType region availabilityZone
phase observedAt startTime completionTime completedAt checkpointID globalStep
previousGlobalStep rank ready worldSize readyRanks iterationTimeSeconds
replicas readyReplicas availableReplicas updatedReplicas unavailableReplicas
iterationSamples iterationObservedAt iterationMethod iterationAggregation
intervalSeconds enabled schedule checkpointIntervalSeconds intervalSource
intervalCostEvaluated decisionStage desiredWorkers onDemandWorkers spotWorkers
policy targetWorkers fixedOnDemand minOnDemand provisioningBlocked minIntervalSeconds maxIntervalSeconds
lambdaPerHour prices spotPricePerHour onDemandPricePerHour validUntil
checkpointSeconds copySeconds measuredCosts checkpoint checkpointFiles pods
sha256 bytes size checkpointTime exportedAt clusters clusterName
lastSuccessfulFullCheckpoint result spec status memberUID boundWorkloadUID
""".split())
SAFE_LABELS = {"app", RUN_LABEL, POLICY_LABEL, PARENT_LABEL, "training.dcnlab.com/policy-uid", "training.dcnlab.com/role"}
SAFE_ANNOTATIONS = {
    "training.dcnlab.com/checkpoint-id", "training.dcnlab.com/source-checkpoint-uid",
    "training.dcnlab.com/schedule-uid", "training.dcnlab.com/started-at",
    PARENT_UID, "training.dcnlab.com/scheduled-parent-name",
}


def project(value):
    if isinstance(value, dict):
        return {key: project(item) for key, item in value.items() if key in SAFE_FIELDS}
    if isinstance(value, list):
        return [project(item) for item in value]
    if value is None or isinstance(value, (str, bool, int)):
        return value
    if isinstance(value, float) and math.isfinite(value):
        return value
    return None


def safe_resource(obj):
    meta = obj.get("metadata", {})
    safe_meta = {key: meta[key] for key in ("name", "namespace", "uid", "generation", "creationTimestamp") if key in meta}
    safe_meta["labels"] = {key: val for key, val in meta.get("labels", {}).items() if key in SAFE_LABELS}
    safe_meta["annotations"] = {key: val for key, val in meta.get("annotations", {}).items() if key in SAFE_ANNOTATIONS}
    return {"apiVersion": obj.get("apiVersion"), "kind": obj.get("kind"), "metadata": safe_meta,
            "spec": project(obj.get("spec", {})), "status": project(obj.get("status", {}))}


def append_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8", newline="\n") as handle:
        handle.write(json.dumps(value, allow_nan=False, sort_keys=True) + "\n")


def atomic_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, allow_nan=False, sort_keys=True, indent=2) + "\n", encoding="utf-8")
    temporary.replace(path)


class Collector:
    def __init__(self, evidence, runner=subprocess.run, environ=None):
        self.evidence = Path(evidence)
        self.run = load_run(evidence)
        if not self.run.get("workload_uid"):
            raise ValueError("prepare must populate workload_uid before collection")
        self.runner = runner
        self.environ = os.environ if environ is None else environ
        self.state_path = self.evidence / "collector_state.json"
        self.state = {"run_id": self.run["run_id"], "cursors": {}}
        if self.state_path.exists():
            self.state = json.loads(self.state_path.read_text(encoding="utf-8"))
            if self.state.get("run_id") != self.run["run_id"]:
                raise ValueError("collector state belongs to another run")
        self.seen = {}

    def error(self, operation, reason, **details):
        append_json(self.evidence / "errors.jsonl", {"timestamp": utc_now(), "run_id": self.run["run_id"],
                    "operation": operation, "reason": reason, **details})

    def command(self, args, operation, as_json=True):
        try:
            result = self.runner(args, capture_output=True, text=True, encoding="utf-8", errors="replace", timeout=45)
            if result.returncode:
                self.error(operation, "command_failed", returncode=result.returncode)
                return None
            return json.loads(result.stdout) if as_json else result.stdout
        except (OSError, subprocess.TimeoutExpired, ValueError):
            self.error(operation, "command_unavailable_timeout_or_invalid_json")
            return None

    def kubectl(self, cluster):
        config = self.environ.get(cluster.upper() + "_KUBECONFIG")
        if not config:
            self.error(cluster, "missing_kubeconfig")
            return None
        namespace = "hybridspot-system" if cluster == "mgmt" else NAMESPACE
        return ["kubectl", "--kubeconfig", config, "--request-timeout=30s", "-n", namespace]

    def log_batch(self, uid, container, restart, output):
        if not NAME.fullmatch(uid) or not NAME.fullmatch(container) or type(restart) is not int or restart < 0:
            raise ValueError("invalid log stream identity")
        key = uid + "/" + container + "." + str(restart)
        path = self.evidence / "logs" / (key + ".jsonl")
        if key not in self.seen:
            self.seen[key] = set()
            if path.exists():
                with path.open(encoding="utf-8") as handle:
                    for line in handle:
                        try:
                            self.seen[key].add(json.loads(line)["record_id"])
                        except (ValueError, KeyError):
                            self.error("logs", "invalid_saved_log_record")
        cursor = self.state["cursors"].get(key, self.run["started_at"])
        for line in output.splitlines():
            observed, separator, message = line.partition(" ")
            if not separator:
                continue
            try:
                at = timestamp(observed)
                if at < timestamp(self.run["started_at"]):
                    continue
                if at >= timestamp(cursor):
                    cursor = observed
                event = event_from_message(message)
                if event is None or event["run_id"] != self.run["run_id"]:
                    continue
            except (ValueError, TypeError):
                self.error("logs", "invalid_timestamp_or_event", pod_uid=uid)
                continue
            safe_message = PREFIX + json.dumps(event, sort_keys=True, separators=(",", ":"))
            digest = hashlib.sha256((observed + " " + safe_message).encode()).hexdigest()
            if digest not in self.seen[key]:
                append_json(path, {"timestamp": observed, "pod_uid": uid, "container": container,
                            "restart": restart, "record_id": digest, "message": safe_message})
                self.seen[key].add(digest)
        self.state["cursors"][key] = cursor

    def collect_logs(self, base, pod):
        meta = pod.get("metadata", {})
        uid, name = meta.get("uid", ""), meta.get("name", "")
        labels = meta.get("labels", {})
        if labels.get("app") != self.run["app"] or labels.get(RUN_LABEL) != self.run["run_id"]:
            return
        if not NAME.fullmatch(uid) or not NAME.fullmatch(name):
            self.error("logs", "invalid_pod_identity")
            return
        for status in pod.get("status", {}).get("containerStatuses", []):
            container, restart = status.get("name", ""), status.get("restartCount", 0)
            if not NAME.fullmatch(container) or type(restart) is not int or restart < 0:
                continue
            generations = [(restart, False)]
            if restart:
                generations.append((restart - 1, True))
            for generation, previous in generations:
                key = uid + "/" + container + "." + str(generation)
                cursor = self.state["cursors"].get(key, self.run["started_at"])
                since = (timestamp(cursor) - timedelta(seconds=1)).isoformat().replace("+00:00", "Z")
                args = base + ["logs", name, "-c", container, "--timestamps=true", "--since-time=" + since]
                if previous:
                    args.append("--previous=true")
                output = self.command(args, "aws/pod_logs", as_json=False)
                if output is None:
                    continue
                # A StatefulSet can replace a same-name Pod between list and logs.
                current = self.command(base + ["get", "pod", name, "-o", "json"], "aws/verify_log_uid")
                statuses = (current or {}).get("status", {}).get("containerStatuses", [])
                same_restart = any(s.get("name") == container and s.get("restartCount", 0) == restart for s in statuses)
                if (current or {}).get("metadata", {}).get("uid") != uid or not same_restart:
                    self.error("logs", "pod_changed_during_poll", pod_uid=uid)
                    continue
                self.log_batch(uid, container, generation, output)

    def poll(self, aws_status=False):
        latest = load_run(self.evidence)
        if any(latest.get(key) != self.run.get(key) for key in ("run_id", "app", "workload_uid")):
            raise ValueError("run binding changed during collection")
        self.run = latest
        if not self.run.get("started_at"):
            if self.run.get("phase") == "prepared":
                return  # The runner launches collection immediately before start.
            raise ValueError("started_at is required for collection")
        snapshot = {"timestamp": utc_now(), "run_id": self.run["run_id"], "resources": []}
        instance_ids = {identity for identity in self.state.get("instance_ids", []) if INSTANCE.fullmatch(identity)}
        for cluster in ("karmada", "aws", "mgmt"):
            base = self.kubectl(cluster)
            if base is None:
                continue
            policy_selector = POLICY_LABEL + "=" + self.run["policy"]
            targets = [("nodeprovisions", ["-l", policy_selector]),
                       ("fluidcrmigrations", ["-l", policy_selector])]
            if cluster == "karmada":
                targets += [("trainingpolicies", [self.run["policy"]]),
                            ("trainingruntimes", ["-l", policy_selector]),
                            ("spotriskprofiles", [self.run["risk"]])]
            if cluster == "aws":
                targets += [("pods", ["-l", "app=" + self.run["app"] + "," + RUN_LABEL + "=" + self.run["run_id"]])]
            if cluster == "mgmt":
                targets = [("deployments", ["checkpoint-coordinator"]), ("deployments", ["policy-manager"])]
            for resource, selection in targets:
                data = self.command(base + ["get", resource] + selection + ["-o", "json"], cluster + "/" + resource)
                if not isinstance(data, dict):
                    continue
                items = data.get("items", [data])
                if not isinstance(items, list):
                    self.error(cluster + "/" + resource, "invalid_resource_list")
                    continue
                items = [obj for obj in items if isinstance(obj, dict)]
                snapshot["resources"].append({"cluster": cluster, "resource": resource,
                                              "items": [safe_resource(obj) for obj in items]})
                for obj in items:
                    if resource == "fluidcrmigrations" and cluster == "aws" and "schedule" in obj.get("spec", {}):
                        meta, ref = obj.get("metadata", {}), obj.get("spec", {}).get("workloadRef", {})
                        parent_uid = meta.get("uid", "")
                        if (NAME.fullmatch(parent_uid) and meta.get("labels", {}).get(POLICY_LABEL) == self.run["policy"]
                                and ref.get("uid") == self.run["workload_uid"] and ref.get("name") == self.run["app"]):
                            children = self.command(base + ["get", resource, "-l", PARENT_LABEL + "=" + parent_uid,
                                                           "-o", "json"], "aws/checkpoint_children")
                            if isinstance(children, dict) and isinstance(children.get("items"), list):
                                snapshot["resources"].append({"cluster": cluster, "resource": resource,
                                    "items": [safe_resource(child) for child in children["items"] if isinstance(child, dict)]})
                    if resource == "pods":
                        self.collect_logs(base, obj)
                    if resource == "nodeprovisions" and obj.get("metadata", {}).get("labels", {}).get(POLICY_LABEL) == self.run["policy"]:
                        identity = obj.get("status", {}).get("instanceId", "")
                        if INSTANCE.fullmatch(identity):
                            instance_ids.add(identity)
        snapshot["instance_ids"] = sorted(instance_ids)
        self.state["instance_ids"] = sorted(instance_ids)
        if aws_status and instance_ids:
            result = self.command(["aws", "ec2", "describe-instance-status", "--region", REGION,
                                   "--instance-ids", *sorted(instance_ids), "--include-all-instances", "--output", "json", "--no-cli-pager"], "aws/ec2_status")
            if isinstance(result, dict):
                snapshot["ec2"] = {"region": REGION, "statuses": [
                    {"instance_id": item.get("InstanceId"), "state": item.get("InstanceState", {}).get("Name"),
                     "instance_status": item.get("InstanceStatus", {}).get("Status"),
                     "system_status": item.get("SystemStatus", {}).get("Status")}
                    for item in result.get("InstanceStatuses", []) if item.get("InstanceId") in instance_ids]}
        name = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ.json")
        atomic_json(self.evidence / "snapshots" / name, snapshot)
        atomic_json(self.state_path, self.state)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--evidence", required=True, type=Path)
    parser.add_argument("--interval", type=float, default=10)
    parser.add_argument("--aws-status", action="store_true")
    parser.add_argument("--once", action="store_true")
    args = parser.parse_args(argv)
    if not math.isfinite(args.interval) or args.interval <= 0:
        parser.error("interval must be positive and finite")
    try:
        collector = Collector(args.evidence)
        while True:
            collector.poll(args.aws_status)
            if args.once:
                break
            time.sleep(args.interval)
    except KeyboardInterrupt:
        return 0
    except (OSError, ValueError) as exc:
        parser.exit(2, "collection failed: " + type(exc).__name__ + "\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
