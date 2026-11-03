#!/usr/bin/env python3
"""GPT-2 E1 execution and evidence capture. Requires Python 3.10+ and kubectl."""
import argparse
import csv
from datetime import datetime
import hashlib
import json
import os
import re
from pathlib import Path
import subprocess
import tarfile
import time

from e1_render import render_e1
from run import NS, now, read, save
from collect import SAFE_FIELDS, SAFE_LABELS, SAFE_ANNOTATIONS, event_from_message

KEYS = {"onpre1": "ONPRE1_KUBECONFIG", "aws": "AWS_KUBECONFIG",
        "karmada": "KARMADA_KUBECONFIG", "mgmt": "MGMT_KUBECONFIG"}
KINDS = ("statefulsets", "pods", "persistentvolumeclaims", "events", "nodeprovisions",
         "trainingpolicies", "trainingruntimes", "fluidcrmigrations", "restorerequests",
         "restoreplans", "pvmetadata", "pvmigrations", "resourcebindings", "propagationpolicies")

EVIDENCE_FIELDS = SAFE_FIELDS | set("""
metadata items annotations labels deletionTimestamp resourceVersion ownerReferences
claimRef nfs server path readOnly volumeName storageClassName accessModes capacity allocatable
resources requests storage persistentVolumeReclaimPolicy mountOptions volumes pvcSpec pvSpec
pvcName pvcUID pvName standalone collectedAt workloadUID memberWorkloadUID podUID podName
durableRef filePath sourcePath targetPath containerName conditions type reason lastTransitionTime
containers initContainers containerStatuses initContainerStatuses image imageID restartCount
state running waiting terminated startedAt finishedAt exitCode containerID nodeInfo kubeletVersion
containerRuntimeVersion kernelVersion osImage addresses address unschedulable template selector
clusterAffinity clusterNames placement verified verifiedAt activeCluster policyUID checkpointRef
sourcePod sourcePodUID sourceNode targetPod archives sourceFence sourceFenced groupRecovery
fenceReceipt preparation sourceWorkloadUID targetWorkloadUID sourceCluster targetCluster
resourceBinding metadataRef bindingName bindingUID dispatchSuspended suspension dispatching
provisioner volumeBindingMode reclaimPolicy involvedObject eventTime firstTimestamp lastTimestamp count
planRef restorePlanRef checkpointUID sourceCheckpointUID targetPodUID targetNode uid generation
phase preparedAt restoredAt sourceReleasedAt observedAt role matchLabels workload manifests
""".split())


def evidence_resource(value):
    if isinstance(value, list):
        return [evidence_resource(v) for v in value]
    if not isinstance(value, dict):
        return value
    result = {}
    for k, v in value.items():
        if k in ("labels", "annotations"):
            allowed = SAFE_LABELS if k == "labels" else SAFE_ANNOTATIONS
            result[k] = {a: b for a, b in v.items() if a in allowed}
        elif k in ("capacity", "allocatable", "requests"):
            result[k] = {a: b for a, b in v.items() if a in ("cpu", "memory", "storage", "nvidia.com/gpu")}
        elif k in EVIDENCE_FIELDS:
            result[k] = evidence_resource(v)
    return result


def valid_time(value):
    try:
        return datetime.fromisoformat(value.replace("Z", "+00:00")).tzinfo is not None
    except (ValueError, AttributeError):
        return False


def durable_world(obj, uids, checkpoint_id):
    meta = obj.get("metadata", {})
    status = obj.get("status", {})
    pods = status.get("pods", [])
    return (meta.get("uid") and not meta.get("deletionTimestamp") and meta.get("generation", 0) > 0
            and status.get("observedGeneration") == meta["generation"] and valid_time(status.get("completionTime"))
            and status.get("phase") == "Completed" and len(pods) == 2 and len(set(uids)) == 2
            and {p.get("podUID") for p in pods} == set(uids)
            and len({p.get("podName") for p in pods}) == 2
            and all(p.get("podName") and p.get("nodeName") and p.get("phase") in ("Resumed", "ContainerCheckpointed")
                and p.get("checkpointID") == checkpoint_id and len(p.get("checkpointFiles", [])) == 1 and any(
                f.get("containerName") == "trainer" and f.get("checkpointID") == checkpoint_id
                and f.get("durableRef") == "file-store:" + NS + "/sha256/" + f.get("sha256", "")
                and f.get("filePath") and valid_time(f.get("exportedAt"))
                and re.fullmatch(r"[0-9a-f]{64}", f.get("sha256", ""))
                for f in p.get("checkpointFiles", [])) for p in pods))


def events_from(text, run_id):
    for line in text.splitlines():
        if "EVAL_EVENT " not in line:
            continue
        try:
            event = json.loads(line.split("EVAL_EVENT ", 1)[1])
            if not isinstance(event, dict) or event.get("run_id") != run_id or event.get("world_size") != 2:
                continue
            safe = event_from_message("EVAL_EVENT " + json.dumps(event))
        except (ValueError, TypeError):
            continue
        if safe:
            yield dict(safe, world_size=2)


def source_ready(metadata, runtimes, workload_uid, uids, claims):
    volume_ok = False
    for item in metadata:
        spec, status = item.get("spec", {}), item.get("status", {})
        if spec.get("workloadRef", {}).get("uid") != workload_uid or spec.get("sourceCluster") != "onpre1":
            continue
        snapshots = [status] + [s for s in status.get("clusters", []) if s.get("clusterName") == "onpre1"]
        for s in snapshots:
            volumes = {v.get("pvcName"): v for v in s.get("volumes", [])}
            if s.get("ready") and set(claims) <= set(volumes) and all(
                    volumes[n].get("pvcUID") and volumes[n].get("pvcSpec")
                    and volumes[n].get("pvSpec", {}).get("nfs", {}).get("server") == "10.50.1.31"
                    for n in claims):
                volume_ok = True
    for item in runtimes:
        for cluster in item.get("status", {}).get("clusters", []):
            s = cluster.get("status", {})
            if (cluster.get("clusterName") == "onpre1" and s.get("workloadUID") == workload_uid
                    and s.get("worldSize") == 2 and s.get("readyRanks") == 2
                    and {p.get("uid") for p in s.get("pods", [])} == set(uids)):
                return volume_ok
    return False


def restored_progress(events, resume_steps):
    restores = [e for e in events if e["event"] == "restored" and e["global_step"] > 0 and e.get("attempt_id")]
    if not restores:
        return None
    restore = restores[-1]
    events = [e for e in events if e.get("attempt_id") == restore["attempt_id"]]
    audits = [e for e in events if e["event"] == "restore_audit" and e.get("passed") is True
              and e["global_step"] == restore["global_step"]
              and all(e.get("checks", {}).get(k) is True for k in ("model", "optimizer", "rng", "step"))]
    updates = [e for e in events if e["event"] == "update_audit" and e.get("passed") is True
               and all(e.get(k) is True for k in ("gradients_finite", "gradients_nonzero", "parameters_changed"))]
    expected = set(range(restore["global_step"] + 1, restore["global_step"] + 4))
    if (audits and expected <= {e["global_step"] for e in updates}
            and any(e["event"] == "step" and e["global_step"] >= restore["global_step"] + resume_steps for e in events)):
        return restore["global_step"]
    return None


def report(out):
    """Lossless analysis inputs; no inferred normal-case corrections or success claim."""
    out = Path(out)
    fields = ["timestamp", "cluster", "rank", "global_step", "event", "attempt_id", "pod_uid", "node", "observed_at"]
    path = out / "training-events.jsonl"
    with (out / "progress.csv").open("w", newline="", encoding="utf-8-sig") as f:
        writer = csv.DictWriter(f, fieldnames=fields, extrasaction="ignore")
        writer.writeheader()
        seen = set()
        if path.exists():
            for line in path.read_text(encoding="utf-8").splitlines():
                event = json.loads(line)
                key = tuple(event.get(k) for k in fields if k != "observed_at")
                if key not in seen:
                    writer.writerow(event)
                    seen.add(key)
    path = out / "timeline.jsonl"
    timeline = [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines()] if path.exists() else []
    with (out / "timeline.csv").open("w", newline="", encoding="utf-8-sig") as f:
        writer = csv.writer(f)
        writer.writerow(["timestamp", "event", "details"])
        for e in timeline:
            writer.writerow([e["timestamp"], e["event"], json.dumps(e)])
    save(out / "analysis-notes.json", {
        "result": "requires_review", "clock": "UTC; controller timestamps and observer timestamps are distinct",
        "scope": "2-rank GPT-2 onpre1 -> AWS On-Demand; shared NFS rebinding, not data copy",
        "review": ["same checkpoint across both ranks", "restore/update audits", "source fencing",
                   "PV/PVC UID and NFS backing", "automatic cold capacity", "failures and interventions"],
        "limitations": "No abnormal gaps removed. No cost advantage or statistical claim from E1."
    })


class Experiment:
    def __init__(self, directory):
        self.out = Path(directory).resolve()
        self.run = read(self.out / "run.json")
        if self.run.get("experiment") != "e1" or self.run.get("model") != "gpt2":
            raise ValueError("Only a GPT-2 E1 evidence directory is supported")
        self.app = self.run["app"]
        self.seen = set()
        self.observed = []
        self.last_snapshot = 0
        self.log_since = {}

    def event(self, event, **fields):
        if "detail" in fields:
            private = self.out / "private"
            private.mkdir(exist_ok=True)
            with (private / "errors.jsonl").open("a", encoding="utf-8") as f:
                f.write(json.dumps(dict(timestamp=now(), event=event, **fields)) + "\n")
            fields["detail"] = "Raw diagnostics saved under private/ (not packaged)"
        record = dict(timestamp=now(), event=event, **fields)
        with (self.out / "timeline.jsonl").open("a", encoding="utf-8") as f:
            f.write(json.dumps(record) + "\n")
        print(json.dumps(record), flush=True)

    def k(self, cluster, *args, obj=None):
        key = KEYS[cluster]
        if not os.environ.get(key):
            raise RuntimeError(key + " is required")
        result = subprocess.run(["kubectl", "--kubeconfig", os.environ[key], "--request-timeout=10s", *args],
                                input=json.dumps(obj) if obj is not None else None,
                                capture_output=True, text=True, timeout=20)
        if result.returncode:
            raise RuntimeError(cluster + " " + " ".join(args[:5]) + ": " + result.stderr[-1500:])
        return result.stdout

    def get(self, cluster, kind, name=None, namespace=NS):
        args = (["-n", namespace] if namespace else []) + ["get", kind]
        if name:
            args += [name]
        return json.loads(self.k(cluster, *args, "-o", "json"))

    def create(self, cluster, obj):
        self.event("create_requested", cluster=cluster, kind=obj["kind"], name=obj["metadata"]["name"])
        self.k(cluster, "create", "-f", "-", obj=obj)

    def patch(self, kind, name, patch):
        self.k("karmada", "-n", NS, "patch", kind, name, "--type=merge", "-p", json.dumps(patch))

    def pods(self, cluster):
        return [p for p in self.get(cluster, "pods")["items"]
                if p["metadata"].get("labels", {}).get("app") == self.app]

    def logs(self):
        for cluster in ("onpre1", "aws"):
            try:
                pods = self.pods(cluster)
            except Exception as exc:
                self.event("collection_error", cluster=cluster, detail=str(exc))
                continue
            for pod in pods:
                meta = pod["metadata"]
                key = cluster + "-" + meta["uid"]
                try:
                    args = ["-n", NS, "logs", meta["name"], "-c", "trainer", "--timestamps=true"]
                    if key in self.log_since:
                        args += ["--since-time=" + self.log_since[key]]
                    text = self.k(cluster, *args)
                    folder = self.out / "private" / "trainer-logs"
                    folder.mkdir(parents=True, exist_ok=True)
                    with (folder / (key + ".log")).open("a", encoding="utf-8") as f:
                        f.write(text)
                    for e in events_from(text, self.run["run_id"]):
                        identity = (key, json.dumps(e, sort_keys=True))
                        if identity in self.seen:
                            continue
                        self.seen.add(identity)
                        e = dict(e, cluster=cluster, pod_uid=meta["uid"], pod_name=meta["name"],
                                 node=pod["spec"].get("nodeName"), observed_at=now())
                        self.observed.append(e)
                        with (self.out / "training-events.jsonl").open("a", encoding="utf-8") as f:
                            f.write(json.dumps(e) + "\n")
                    lines = text.splitlines()
                    if lines and " " in lines[-1]:
                        self.log_since[key] = lines[-1].split(" ", 1)[0]
                except Exception as exc:
                    self.event("log_error", cluster=cluster, pod=meta["name"], detail=str(exc))

    def snapshot(self, label):
        folder = self.out / "snapshots" / (str(time.time_ns()) + "-" + label)
        folder.mkdir(parents=True)
        for cluster in ("onpre1", "aws", "karmada"):
            for kind in (*KINDS, "persistentvolumes", "nodes"):
                if cluster != "karmada" and kind in ("resourcebindings", "propagationpolicies"):
                    continue
                try:
                    value = self.get(cluster, kind, namespace=None if kind in ("persistentvolumes", "nodes") else NS)
                    save(folder / (cluster + "-" + kind + ".json"), evidence_resource(value))
                except Exception as exc:
                    save(folder / (cluster + "-" + kind + ".error.json"), {"error": type(exc).__name__})
                    self.event("collection_error", detail=str(exc))
            if cluster == "karmada":
                for target in ("onpre1", "aws"):
                    try:
                        works = self.get(cluster, "works", namespace="karmada-es-" + target)
                        works["items"] = [w for w in works["items"] if self.app in json.dumps(w)
                                          and not any(m.get("kind") == "Secret" for m in w.get("spec", {}).get("workload", {}).get("manifests", []))]
                        save(folder / ("works-" + target + ".json"), evidence_resource(works))
                    except Exception as exc:
                        self.event("collection_error", detail=str(exc))
        self.last_snapshot = time.monotonic()

    def controllers(self):
        folder = self.out / "controllers"
        folder.mkdir(exist_ok=True)
        for cluster in KEYS:
            for ns in ("hybridspot-system", "stateful-migration-system", "pv-migration-system",
                       "remote-cluster-provisioner-system", "fluidcr-system"):
                try:
                    deployments = self.get(cluster, "deployments", namespace=ns)
                    save(folder / (cluster + "-" + ns + "-deployments.json"), evidence_resource(deployments))
                    save(folder / (cluster + "-" + ns + "-pods.json"), evidence_resource(self.get(cluster, "pods", namespace=ns)))
                    for item in deployments["items"]:
                        name = item["metadata"]["name"]
                        text = self.k(cluster, "-n", ns, "logs", "deployment/" + name,
                                      "--all-containers=true", "--timestamps=true", "--since-time=" + self.run["created_at"])
                        private = self.out / "private" / "controllers"
                        private.mkdir(parents=True, exist_ok=True)
                        (private / (cluster + "-" + name + ".log")).write_text(text, encoding="utf-8")
                except Exception as exc:
                    self.event("collection_error", cluster=cluster, namespace=ns, detail=str(exc))

    def wait(self, name, predicate, seconds):
        self.event(name + "_waiting", timeout_seconds=seconds)
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            self.logs()
            result = predicate()
            if result:
                self.event(name + "_observed")
                return result
            if time.monotonic() - self.last_snapshot > 60:
                self.snapshot(name)
            time.sleep(5)
        raise TimeoutError(name + " timed out; resources retained, no automatic retry")

    def preflight(self):
        for key in KEYS.values():
            if not os.environ.get(key):
                raise RuntimeError(key + " is required")
        for cluster in ("onpre1", "aws", "karmada"):
            self.get(cluster, "namespace", NS, namespace=None)
            for kind in ("statefulsets", "trainingpolicies", "restorerequests", "pvmigrations"):
                if self.get(cluster, kind)["items"]:
                    raise RuntimeError(cluster + ": existing " + kind + " in experiment namespace; inspect first")
            for item in self.get(cluster, "persistentvolumeclaims")["items"]:
                if item["metadata"]["name"].startswith(self.app + "-"):
                    raise RuntimeError("Fresh claim names required")
        save(self.out / "source-storageclass.json", self.get("onpre1", "storageclass", self.run["storage_class"], namespace=None))
        nodes = self.get("onpre1", "nodes", namespace=None)["items"]
        ready = [n for n in nodes if not n.get("spec", {}).get("unschedulable")
                 and n["metadata"].get("labels", {}).get("ml.dcn.ssu.ac.kr/provider") == "AWS"
                 and int(n.get("status", {}).get("allocatable", {}).get("nvidia.com/gpu", "0")) >= 1
                 and any(c["type"] == "Ready" and c["status"] == "True" for c in n["status"].get("conditions", []))]
        if len(ready) < 2:
            raise RuntimeError("Two Ready, schedulable source GPU workers required")
        self.event("preflight_passed")

    def progress(self, cluster, minimum):
        pods = self.pods(cluster)
        if len(pods) != 2 or len({p["spec"].get("nodeName") for p in pods}) != 2:
            return False
        uids = {p["metadata"]["uid"] for p in pods}
        steps = [e for e in self.observed if e["cluster"] == cluster and e["pod_uid"] in uids
                 and e["event"] == "step" and e["global_step"] >= minimum]
        for rank in (0, 1):
            expected = next((p for p in pods if p["metadata"]["name"] == self.app + "-" + str(rank)), None)
            if not expected or not any(e["rank"] == rank and e["pod_uid"] == expected["metadata"]["uid"] for e in steps):
                return False
        return pods

    def execute(self, args):
        with (self.out / "execution-started.json").open("x", encoding="utf-8") as f:
            json.dump({"timestamp": now()}, f)
        self.preflight()
        self.snapshot("baseline")
        self.controllers()
        for file, cluster in (("onpre-storage.json", "onpre1"), ("karmada.json", "karmada")):
            self.k(cluster, "create", "--dry-run=server", "-f", str(self.out / file))
        for file, cluster in (("onpre-storage.json", "onpre1"), ("karmada.json", "karmada")):
            for obj in read(self.out / file)["items"]:
                self.create(cluster, obj)
        uid = self.get("karmada", "statefulset", self.app)["metadata"]["uid"]
        label = {"training.dcnlab.com/workload-uid": uid}
        self.patch("statefulset", self.app, {"metadata": {"labels": label}, "spec": {"template": {"metadata": {"labels": label}}}})
        policy = read(self.out / "policy.unbound.json")
        policy["spec"]["workloadRef"]["uid"] = uid
        save(self.out / "policy.json", policy)
        self.create("karmada", policy)
        claims = [o["metadata"]["name"] for o in read(self.out / "onpre-storage.json")["items"] if o["kind"] == "PersistentVolumeClaim"]
        self.wait("source_storage", lambda: all(self.get("onpre1", "pvc", n).get("status", {}).get("phase") == "Bound" for n in claims), 300)
        if self.get("aws", "nodeprovisions")["items"] or any(p["metadata"]["name"] in claims for p in self.get("aws", "pvc")["items"]):
            raise RuntimeError("Target is not cold; refusing to start")
        self.event("source_start_requested")
        self.patch("statefulset", self.app, {"spec": {"replicas": 2}})
        pods = self.wait("source_progress", lambda: self.progress("onpre1", args.warmup_steps), args.source_timeout)
        uids = {p["metadata"]["uid"] for p in pods}
        save(self.out / "source-pods.json", evidence_resource(pods))
        self.wait("source_metadata", lambda: source_ready(
            self.get("karmada", "pvmetadata")["items"], self.get("karmada", "trainingruntimes")["items"],
            uid, uids, claims), 300)
        self.snapshot("before-checkpoint")
        cp = self.app + "-e1-checkpoint"
        policy = self.get("karmada", "trainingpolicy", self.app)
        labels = {"training.dcnlab.com/managed-by": "hybridspotvm-system", "training.dcnlab.com/policy": self.app,
                  "training.dcnlab.com/policy-uid": policy["metadata"]["uid"], "training.dcnlab.com/role": "checkpoint"}
        checkpoint = {"apiVersion": "fluidcr.dcnlab.com/v1alpha1", "kind": "FluidCRMigration",
                      "metadata": {"name": cp, "namespace": NS, "labels": labels,
                                   "annotations": {"training.dcnlab.com/checkpoint-id": cp}},
                      "spec": {"workloadRef": policy["spec"]["workloadRef"], "resume": True, "container": "trainer",
                               "ctrlPort": 8298, "appCheckpointTimeoutSeconds": 300, "kubeletTimeoutSeconds": 300}}
        pp = {"apiVersion": "policy.karmada.io/v1alpha1", "kind": "PropagationPolicy",
              "metadata": {"name": cp + "-placement", "namespace": NS}, "spec": {
                  "resourceSelectors": [{"apiVersion": checkpoint["apiVersion"], "kind": checkpoint["kind"], "name": cp}],
                  "placement": {"clusterAffinity": {"clusterNames": ["onpre1"]}}}}
        self.create("karmada", pp)
        self.create("karmada", checkpoint)
        def exported():
            member = next((o for o in self.get("onpre1", "fluidcrmigrations")["items"]
                           if o["metadata"]["name"] == cp and durable_world(o, uids, cp)), None)
            if not member:
                return False
            reflected = self.get("karmada", "fluidcrmigration", cp)
            reports = [s for s in reflected.get("status", {}).get("clusters", []) if s.get("clusterName") == "onpre1"]
            if len(reports) != 1:
                return False
            view = dict(reflected, status=reports[0])
            if not durable_world(view, uids, cp):
                return False
            save(self.out / "checkpoint-reflected.json", evidence_resource(reflected))
            return member
        result = self.wait("checkpoint_export", exported, args.checkpoint_timeout)
        save(self.out / "checkpoint-exported.json", evidence_resource(result))
        self.snapshot("before-move")
        self.event("move_requested")
        self.patch("propagationpolicy", self.app + "-placement", {"spec": {"placement": {"clusterAffinity": {"clusterNames": ["aws"]}}}})
        def pending_move():
            for binding in self.get("karmada", "resourcebindings")["items"]:
                resource = binding.get("spec", {}).get("resource", {})
                if resource.get("uid") != uid or resource.get("kind") != "StatefulSet":
                    continue
                pending = binding.get("metadata", {}).get("annotations", {}).get("training.dcnlab.com/pending-placement", "[]")
                if json.loads(pending) == [{"name": "aws", "replicas": 2}]:
                    return True
            return False
        self.wait("pending_move", pending_move, 300)
        self.patch("trainingpolicy", self.app, {"metadata": {"annotations": {"training.dcnlab.com/suspend": "false"}}})
        self.event("policy_resumed_for_move")
        def resumed():
            target = self.progress("aws", 1)
            if not target:
                return False
            target_uids = {p["metadata"]["uid"] for p in target}
            if target_uids & uids:
                return False
            restored_steps = []
            for rank in (0, 1):
                pod = next(p for p in target if p["metadata"]["name"] == self.app + "-" + str(rank))
                events = [e for e in self.observed if e["cluster"] == "aws" and e["pod_uid"] == pod["metadata"]["uid"] and e["rank"] == rank]
                step = restored_progress(events, args.resume_steps)
                if step is None:
                    return False
                restored_steps.append(step)
            placement = self.get("karmada", "trainingpolicy", self.app).get("status", {}).get("placement", {})
            return (len(set(restored_steps)) == 1 and not self.pods("onpre1")
                    and placement.get("verified") is True and placement.get("activeCluster") == "aws")
        self.wait("target_progress_candidate", resumed, args.move_timeout)
        self.event("execution_finished", result="candidate_requires_evidence_review",
                   note="Verify same checkpoint, volume bindings, source fencing and restore receipts offline; resources retained")


def package(out):
    out = Path(out).resolve()
    report(out)
    files = sorted(p for p in out.rglob("*") if p.is_file() and not p.is_symlink()
                   and "private" not in p.relative_to(out).parts and p.name != "sha256.json")
    save(out / "sha256.json", {str(p.relative_to(out)): hashlib.sha256(p.read_bytes()).hexdigest() for p in files})
    archive = out.parent / (out.name + "-" + str(time.time_ns()) + ".tar.gz")
    with tarfile.open(archive, "w:gz") as tar:
        for p in [*files, out / "sha256.json"]:
            tar.add(p, arcname=str(Path(out.name) / p.relative_to(out)), recursive=False)
    print("DOWNLOAD: " + str(archive), flush=True)
    return archive


def build_parser():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    render = sub.add_parser("render")
    render.add_argument("--config", default="evaluation/config.gpt2.ck10k-risk60-fis80.json")
    render.add_argument("--policy-source", default="evaluation/policy-source.json")
    render.add_argument("--run-id", required=True)
    render.add_argument("--out", required=True)
    for name in ("execute", "collect", "pack"):
        p = sub.add_parser(name)
        p.add_argument("--evidence", required=True)
        if name == "execute":
            p.add_argument("--confirm", required=True, help="run ID; authorizes two AWS Spot workers")
            p.add_argument("--warmup-steps", type=int, default=10)
            p.add_argument("--resume-steps", type=int, default=50)
            p.add_argument("--source-timeout", type=int, default=2400)
            p.add_argument("--checkpoint-timeout", type=int, default=1200)
            p.add_argument("--move-timeout", type=int, default=3600)
    return parser


def main():
    parser = build_parser()
    args = parser.parse_args()
    if args.command == "render":
        config = read(args.config)
        if config.get("model") != "gpt2":
            parser.error("E1 driver requires the reviewed GPT-2 config")
        config.update(storage_class="e1-aws-nfs", goal_steps=10000, nfs_server="10.50.1.31")
        run, storage, resources, policy = render_e1(config, read(args.policy_source), args.run_id)
        out = Path(args.out)
        out.mkdir(parents=True, exist_ok=False)
        for name, value in (("run.json", run), ("onpre-storage.json", {"apiVersion": "v1", "kind": "List", "items": storage}),
                            ("karmada.json", {"apiVersion": "v1", "kind": "List", "items": resources}), ("policy.unbound.json", policy)):
            save(out / name, value)
        print("Rendered. Inspect manifests before execute. No cluster changes.")
        return
    if args.command == "pack":
        package(args.evidence)
        return
    experiment = Experiment(args.evidence)
    if args.command == "execute":
        if args.confirm != experiment.run["run_id"]:
            parser.error("--confirm must equal run ID")
        if min(args.warmup_steps, args.resume_steps, args.source_timeout, args.checkpoint_timeout, args.move_timeout) <= 0:
            parser.error("steps and timeouts must be positive")
    try:
        if args.command == "execute":
            experiment.execute(args)
    except (Exception, KeyboardInterrupt) as exc:
        experiment.event("execution_failed", detail=str(exc) or "KeyboardInterrupt", resources="retained")
        raise
    finally:
        experiment.logs()
        experiment.snapshot("final")
        experiment.controllers()
        package(experiment.out)


if __name__ == "__main__":
    main()
