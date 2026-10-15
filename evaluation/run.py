#!/usr/bin/env python3
"""Evaluation preparation and bounded execution. Requires Python 3.10+, kubectl."""
import argparse
import copy
import datetime as dt
import json
import hashlib
import math
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time
from urllib.parse import quote

HERE = Path(__file__).resolve().parent
NS = "fluidcr-realign-121040"
LABEL = "training.dcnlab.com/evaluation-run"
POLICY_UID = "training.dcnlab.com/policy-uid"
ABORT_BACKUP_DIR = "failed-abort-backups"
ABORT_OPERATION_KINDS = (
    "spotreplacements",
    "spotrecoveries",
    "restorerequests",
    "fluidcrmigrations",
)

def now():
    return dt.datetime.now(dt.timezone.utc).isoformat()

def read(path):
    return json.loads(Path(path).read_text(encoding="utf-8"))

def save(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    payload = json.dumps(value, indent=2, ensure_ascii=False) + "\n"
    temporary = None
    try:
        # Publish on the same filesystem so readers see either complete version.
        with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=path.parent,
                                         prefix="." + path.name + ".", suffix=".tmp",
                                         delete=False) as out:
            temporary = Path(out.name)
            out.write(payload)
            out.flush()
            os.fsync(out.fileno())
        os.replace(temporary, path)
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)

def kubectl(cluster, *args, obj=None):
    key = {"karmada": "KARMADA_KUBECONFIG", "aws": "AWS_KUBECONFIG", "mgmt": "MGMT_KUBECONFIG"}[cluster]
    if not os.environ.get(key):
        raise ValueError(key + " is required")
    cmd = ["kubectl", "--kubeconfig", os.environ[key], "--request-timeout=30s"]
    result = subprocess.run(cmd + list(args), input=json.dumps(obj) if obj is not None else None,
                            text=True, capture_output=True, timeout=45)
    if result.returncode:
        raise RuntimeError("kubectl " + " ".join(args[:3]) + ": " + result.stderr[-2000:])
    return result.stdout

def get(cluster, kind, name=None, selector=None):
    args = ["-n", NS, "get", kind]
    if name:
        args += [name]
    if selector:
        args += ["-l", selector]
    return json.loads(kubectl(cluster, *args, "-o", "json"))

def meta(name, run_id, namespaced=True):
    value = {"name": name, "labels": {LABEL: run_id}}
    if namespaced:
        value["namespace"] = NS
    return value

def resource(api, kind, name, run_id, spec=None, namespaced=True):
    obj = {"apiVersion": api, "kind": kind, "metadata": meta(name, run_id, namespaced)}
    if spec is not None:
        obj["spec"] = spec
    return obj

def validate(c, run_id, experiment, arm):
    model = c.get("model", "resnet18")
    if model not in ("resnet18", "resnet50", "gpt2"):
        raise ValueError("model must be resnet18, resnet50, or gpt2")
    if "learning_rate" in c and (type(c["learning_rate"]) not in (int, float)
            or not math.isfinite(c["learning_rate"]) or c["learning_rate"] <= 0):
        raise ValueError("learning_rate must be finite and positive")
    if model == "gpt2":
        path = c.get("nfs_model_path", "")
        if not isinstance(path, str) or not path.startswith("/") or ".." in Path(path).parts or "REPLACE" in path:
            raise ValueError("gpt2 requires an absolute nfs_model_path")
        length = c.get("sequence_length", 512)
        if type(length) is not int or not 2 <= length <= 1024:
            raise ValueError("sequence_length must be between 2 and 1024")
    if c.get("namespace") != NS:
        raise ValueError("namespace must remain " + NS)
    if not re.fullmatch(r"[a-z][a-z0-9-]{0,15}[a-z0-9]|[a-z]", run_id):
        raise ValueError("run-id: 1-17 lowercase letters/digits/hyphens, start with letter")
    if arm not in ({"A", "B"} if experiment == "cost" else {"F60", "F300", "F600", "D"}):
        raise ValueError("arm does not belong to experiment")
    for key in ("nfs_server", "nfs_dataset_path", "training_image", "storage_class"):
        if not c.get(key) or "REPLACE" in c[key]:
            raise ValueError("set " + key + " in config")
    if not c["nfs_dataset_path"].startswith("/") or ".." in Path(c["nfs_dataset_path"]).parts:
        raise ValueError("nfs_dataset_path must be an absolute dataset-only path")
    for key in ("goal_steps", "batch_size", "max_seconds"):
        if type(c.get(key)) is not int or c[key] <= 0:
            raise ValueError(key + " must be a positive integer")
    for key in ("initial_risk", "changed_risk"):
        if not isinstance(c.get(key), (float, int)) or not math.isfinite(c[key]) or c[key] < 0:
            raise ValueError(key + " must be finite and nonnegative")
    if c.get("scenario") not in ("constant", "risk-rise", "risk-schedule", "interruption"):
        raise ValueError("scenario must be constant, risk-rise, risk-schedule, or interruption")
    if c["scenario"] == "risk-schedule":
        schedule = c.get("risk_schedule")
        if not isinstance(schedule, list) or not schedule:
            raise ValueError("risk_schedule must be a nonempty list")
        previous = 0
        for entry in schedule:
            if not isinstance(entry, dict):
                raise ValueError("risk_schedule entries must be objects")
            offset, value = entry.get("after_seconds"), entry.get("lambda_per_hour")
            if type(offset) is not int or not previous < offset < c["max_seconds"]:
                raise ValueError("risk_schedule offsets must increase and precede timeout")
            if type(value) not in (int, float) or not math.isfinite(value) or value < 0:
                raise ValueError("risk_schedule values must be finite and nonnegative")
            previous = offset
    if experiment == "cost" and arm == "A" and c["scenario"] == "interruption":
        raise ValueError("All On-Demand has no Spot target; use a constant control run for interruption comparisons")
    if c["scenario"] == "risk-rise" and not 0 < c.get("risk_change_after_seconds", 0) < c["max_seconds"]:
        raise ValueError("risk change must be positive and before timeout")
    if c["region"] != "ap-northeast-2" or c["availability_zone"] != "ap-northeast-2c":
        raise ValueError("this evaluation is scoped to ap-northeast-2c")

def render(c, source, run_id, experiment, arm):
    validate(c, run_id, experiment, arm)
    model = c.get("model", "resnet18")
    data_mount = "/datasets/wikitext103-tokenized" if model == "gpt2" else "/datasets/cifar10"
    entrypoint = "train_gpt2.py" if model == "gpt2" else "train_resnet18.py"
    app = "eval-" + run_id
    labels = {"app": app, LABEL: run_id}
    policy_spec = copy.deepcopy(source["spec"])
    aws = policy_spec.get("capacity", {}).get("aws", {})
    if "REPLACE" in json.dumps(aws):
        raise ValueError("replace AWS template placeholders before render")
    for key in ("ami", "subnetId", "vpcId", "securityGroupIds", "credentialsRef"):
        if not aws.get(key):
            raise ValueError("source TrainingPolicy requires capacity.aws." + key)
    aws.update(region=c["region"], availabilityZone=c["availability_zone"],
               instanceType=c["instance_type"], karmadaCluster=c["cluster"],
               hardwareType="gpu", nodeLabel="gpu")
    policy_spec.update(workloadRef={"apiVersion": "apps/v1", "kind": "StatefulSet", "name": app, "uid": "BOUND_DURING_PREPARE"},
                       sourceCluster=c["cluster"], expectedWorldSize=2, targetWorkers=2,
                       runtimeRef={"name": app + "-runtime"}, riskProfileRef={"name": app + "-risk"},
                       replacement={"enabled": True})
    policy_spec["policy"] = copy.deepcopy(source["spec"].get("policy", {}))
    policy_spec["policy"].pop("fixedOnDemand", None)
    policy_spec["policy"]["minOnDemand"] = (2 if arm == "A" else 0) if experiment == "cost" else 1
    if experiment == "checkpoint":
        policy_spec["policy"]["fixedOnDemand"] = 1
    dynamic = arm == "D" or (experiment == "cost" and arm == "B")
    interval = 300 if experiment == "cost" or dynamic else int(arm[1:])
    checkpoint = copy.deepcopy(source["spec"].get("checkpoint", {}))
    checkpoint.pop("candidateIntervalSeconds", None)
    checkpoint.pop("measuredCosts", None)
    checkpoint.pop("paperProfile", None)
    checkpoint.update(minIntervalSeconds=60 if dynamic else interval,
                      maxIntervalSeconds=3600 if dynamic else interval,
                      riskBands=[{"maxLambdaPerHour": 1.0, "intervalSeconds": interval}], resume=True)
    policy_spec["checkpoint"] = checkpoint
    policy = resource("training.dcnlab.com/v1alpha1", "TrainingPolicy", app, run_id, policy_spec)
    policy["metadata"]["annotations"] = {"training.dcnlab.com/suspend": "true"}
    if experiment == "cost" and arm == "A":
        policy["metadata"]["annotations"]["training.dcnlab.com/periodic-checkpoint"] = "disabled"
    if experiment == "checkpoint":
        policy["metadata"]["annotations"]["training.dcnlab.com/planned-partial"] = "disabled"
    risk = resource("training.dcnlab.com/v1alpha1", "SpotRiskProfile", app+"-risk", run_id,
                    {"provider": "aws", "region": c["region"], "availabilityZone": c["availability_zone"],
                     "instanceType": c["instance_type"], "staticLambdaPerHour": c["initial_risk"],
                     "pollSeconds": 30, "maxAgeSeconds": 3600})
    data_name = app + "-data"
    pv = resource("v1", "PersistentVolume", data_name, run_id,
                  {"capacity": {"storage": "1Gi"}, "accessModes": ["ReadOnlyMany"],
                   "persistentVolumeReclaimPolicy": "Retain", "storageClassName": "",
                   "claimRef": {"namespace": NS, "name": data_name},
                   "mountOptions": ["ro"], "nfs": {"server": c["nfs_server"], "path": c["nfs_dataset_path"], "readOnly": True}}, False)
    data_pvc = resource("v1", "PersistentVolumeClaim", data_name, run_id,
                        {"accessModes": ["ReadOnlyMany"], "storageClassName": "", "volumeName": data_name,
                         "resources": {"requests": {"storage": "1Gi"}}})
    ckpt = resource("v1", "PersistentVolumeClaim", app+"-checkpoint", run_id,
                    {"accessModes": ["ReadWriteMany"], "storageClassName": c["storage_class"],
                     "resources": {"requests": {"storage": c.get("checkpoint_size", "10Gi")}}})
    env = [{"name": k, "value": str(v)} for k, v in {
        "WORLD_SIZE": 2, "LOCAL_RANK": 0, "MASTER_ADDR": app+"-0."+app+"."+NS+".svc.cluster.local",
        "MASTER_PORT": 29500, "NCCL_SOCKET_IFNAME": "eth0",
        "FLUIDCR_CHECKPOINT_PATH": "/checkpoint/$(POD_NAME)/latest.pt",
        "FLUIDCR_REGISTRY_DIR": "/tmp/fluidcr-workers", "FLUIDCR_DISTRIBUTED": 1,
        "EVAL_RUN_ID": run_id, "EVAL_GOAL_STEPS": c["goal_steps"], "EVAL_BATCH_SIZE": c["batch_size"],
        "EVAL_SEED": c["seed"], "EVAL_DATA_ROOT": data_mount,
        "EVAL_MODEL": model}.items()]
    if "learning_rate" in c:
        env.append({"name": "EVAL_LEARNING_RATE", "value": str(c["learning_rate"])})
    if model == "gpt2":
        env.extend({"name": k, "value": str(v)} for k, v in {
            "EVAL_MODEL_ROOT": "/models/gpt2", "EVAL_SEQUENCE_LENGTH": c.get("sequence_length", 512),
            "HF_HUB_OFFLINE": "1", "HF_DATASETS_OFFLINE": "1",
            "TRANSFORMERS_OFFLINE": "1", "TOKENIZERS_PARALLELISM": "false"}.items())
    env = [{"name":"POD_NAME","valueFrom":{"fieldRef":{"fieldPath":"metadata.name"}}},
           {"name":"RANK","valueFrom":{"fieldRef":{"fieldPath":"metadata.labels['apps.kubernetes.io/pod-index']"}}}] + env
    container = {"name": "trainer", "image": c["training_image"], "imagePullPolicy": "Always",
                 "command": ["python", "-u", "/workspace/" + entrypoint], "env": env,
                 "ports": [{"name":"control","containerPort":8298},{"name":"rendezvous","containerPort":29500}],
                 "resources": {"requests": {"cpu":"2","memory":"6Gi","nvidia.com/gpu":"1"},
                               "limits": {"cpu":"4","memory":"12Gi","nvidia.com/gpu":"1"}},
                 "readinessProbe": {"tcpSocket":{"port":"control"},"initialDelaySeconds":30},
                 "volumeMounts": [{"name":"training-script","mountPath":"/workspace","readOnly":True},
                                  {"name":"dataset","mountPath":data_mount,"readOnly":True},
                                  {"name":"shm","mountPath":"/dev/shm"}]}
    sts = resource("apps/v1","StatefulSet",app,run_id,
                   {"serviceName":app,"replicas":0,"podManagementPolicy":"Parallel","updateStrategy":{"type":"OnDelete"},
                    "selector":{"matchLabels":{"app":app}},
                    "template":{"metadata":{"labels":labels,"annotations":{
                        "fluidcr.dcnlab.com/inject":"true","fluidcr.dcnlab.com/container":"trainer",
                        "fluidcr.dcnlab.com/checkpoint-claim":app+"-checkpoint"}},
                        "spec":{"automountServiceAccountToken":False,
                                "nodeSelector":{"ml.dcn.ssu.ac.kr/provider":"AWS"},
                                "affinity":{"podAntiAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":[{
                                    "labelSelector":{"matchLabels":{"app":app}},"topologyKey":"kubernetes.io/hostname"}]}},
                                "containers":[container],
                                "volumes":[{"name":"training-script","configMap":{"name":app+"-script"}},
                                           {"name":"dataset","persistentVolumeClaim":{"claimName":data_name,"readOnly":True}},
                                           {"name":"shm","emptyDir":{"medium":"Memory","sizeLimit":"1Gi"}}]}}})
    sts["metadata"]["labels"]["migration.dcnlab.com/pv-metadata"] = "enabled"
    member = [pv, data_pvc, ckpt]
    if model == "gpt2":
        model_name = app + "-model"
        model_pv, model_pvc = copy.deepcopy(pv), copy.deepcopy(data_pvc)
        model_pv["metadata"] = meta(model_name, run_id, False)
        model_pv["spec"]["claimRef"]["name"] = model_name
        model_pv["spec"]["nfs"]["path"] = c["nfs_model_path"]
        model_pvc["metadata"] = meta(model_name, run_id)
        model_pvc["spec"]["volumeName"] = model_name
        member.extend([model_pv, model_pvc])
        container["volumeMounts"].append({"name": "model", "mountPath": "/models/gpt2", "readOnly": True})
        sts["spec"]["template"]["spec"]["volumes"].append({"name": "model", "persistentVolumeClaim": {
            "claimName": model_name, "readOnly": True}})
    service = resource("v1","Service",app,run_id,
                       {"clusterIP":"None","publishNotReadyAddresses":True,"selector":{"app":app},
                        "ports":[{"name":"control","port":8298},{"name":"rendezvous","port":29500}]})
    script = resource("v1","ConfigMap",app+"-script",run_id)
    script["data"] = {p.name: p.read_text(encoding="utf-8") for p in (HERE/"resnet18").glob("*.py") if not p.name.startswith("test")}
    if model == "gpt2":
        script["data"].update({p.name: p.read_text(encoding="utf-8") for p in (HERE/"gpt2").glob("*.py")
                               if not p.name.startswith("test")})
    if entrypoint not in script["data"]:
        raise ValueError("missing training entrypoint")
    placement = resource("policy.karmada.io/v1alpha1","PropagationPolicy",app+"-placement",run_id,
                         {"resourceSelectors":[{"apiVersion":o["apiVersion"],"kind":o["kind"],"name":o["metadata"]["name"]} for o in [service,script,sts]],
                          "placement":{"clusterAffinity":{"clusterNames":[c["cluster"]]}}})
    run = dict(c, run_id=run_id, app=app, policy=app, risk=app+"-risk", experiment=experiment, arm=arm,
               model=model, dataset="wikitext-103-raw-v1" if model == "gpt2" else "cifar10",
               world_size=2, created_at=now(), phase="rendered")
    run["learning_rate"] = c.get("learning_rate", 5e-5 if model == "gpt2" else 0.1)
    run["sequence_length"] = c.get("sequence_length", 512) if model == "gpt2" else None
    run["training_code_sha256"] = hashlib.sha256(
        json.dumps(script["data"], sort_keys=True).encode("utf-8")).hexdigest()
    if experiment == "cost" and arm == "A":
        run["scenario"] = "constant"
    return run, member, [service,script,sts,risk,placement], policy

def create(cluster, obj, evidence, ledger):
    result = json.loads(kubectl(cluster, "create", "-f", "-", "-o", "json", obj=obj))
    ledger.append({"cluster":cluster,"apiVersion":obj["apiVersion"],"kind":obj["kind"],
                   "name":obj["metadata"]["name"],"uid":result["metadata"]["uid"],
                   "namespaced":"namespace" in obj["metadata"]})
    save(evidence/"created.json",ledger)
    return result

def owned(run, cluster, kind, name, uid=None):
    obj = get(cluster,kind,name)
    if obj["metadata"].get("labels",{}).get(LABEL) != run["run_id"] or (uid and obj["metadata"]["uid"] != uid):
        raise ValueError("ownership/UID mismatch for "+kind+"/"+name)
    return obj

def guard_idle():
    for item in get("karmada", "statefulsets")["items"]:
        if item.get("spec", {}).get("replicas", 1) > 0:
            raise ValueError("active StatefulSet exists: " + item["metadata"]["name"])
    for item in get("karmada","trainingpolicies")["items"]:
        if item["metadata"].get("annotations",{}).get("training.dcnlab.com/suspend") != "true":
            raise ValueError("active TrainingPolicy exists: "+item["metadata"]["name"]+"; finish/suspend it before evaluation")
    # Isolation also avoids using existing GPU capacity and contaminating provisioning costs.
    if get("aws","nodeprovisions")["items"]:
        raise ValueError("existing NodeProvisions in namespace; finish and clean the previous run first")

def prepare(evidence):
    run = read(evidence/"run.json")
    if run["phase"] != "rendered" or (evidence/"created.json").exists():
        raise ValueError("prepare is create-only; inspect partial creation instead of rerunning")
    guard_idle()
    if run["experiment"] == "checkpoint":
        crd = json.loads(kubectl("karmada","get","crd","trainingpolicies.training.dcnlab.com","-o","json"))
        props = next(v for v in crd["spec"]["versions"] if v["name"]=="v1alpha1")["schema"]["openAPIV3Schema"]["properties"]
        if "fixedOnDemand" not in props["spec"]["properties"]["policy"]["properties"]:
            raise ValueError("install fixedOnDemand CRD + updated controllers before checkpoint experiment")
    ledger = []
    for obj in read(evidence/"member.yaml")["items"]:
        create("aws",obj,evidence,ledger)
    uid = None
    for obj in read(evidence/"karmada.yaml")["items"]:
        result = create("karmada",obj,evidence,ledger)
        if obj["kind"] == "StatefulSet":
            uid = result["metadata"]["uid"]
            patch = {"metadata":{"labels":{"training.dcnlab.com/workload-uid":uid}},
                     "spec":{"template":{"metadata":{"labels":{"training.dcnlab.com/workload-uid":uid}}}}}
            kubectl("karmada","-n",NS,"patch","statefulset",run["app"],"--type=merge","-p",json.dumps(patch))
    policy = read(evidence/"policy.yaml")
    policy["spec"]["workloadRef"]["uid"] = uid
    result = create("karmada",policy,evidence,ledger)
    run.update(workload_uid=uid, policy_uid=result["metadata"]["uid"],phase="prepared")
    save(evidence/"run.json",run)

def event(evidence, event_name, **fields):
    with (evidence/"runner-events.jsonl").open("a",encoding="utf-8") as out:
        out.write(json.dumps(dict(timestamp=now(),event=event_name,**fields))+"\n")

def patch_policy(run, suspended=True):
    owned(run,"karmada","trainingpolicy",run["policy"],run["policy_uid"])
    kubectl("karmada","-n",NS,"patch","trainingpolicy",run["policy"],"--type=merge","-p",
            json.dumps({"metadata":{"annotations":{"training.dcnlab.com/suspend":"true" if suspended else None}}}))

def stop(evidence):
    run = read(evidence/"run.json")
    patch_policy(run)
    wait_quiescent(run)
    owned(run,"karmada","statefulset",run["app"],run["workload_uid"])
    kubectl("karmada","-n",NS,"scale","statefulset",run["app"],"--replicas=0")
    event(evidence,"workload_stopped",warning="VMs still bill until scoped cleanup and EC2 termination")

def policy_uid_owned(obj, run, kind):
    name = obj.get("metadata", {}).get("name", "")
    labels = obj.get("metadata", {}).get("labels", {})
    if labels.get(POLICY_UID) != run["policy_uid"]:
        raise ValueError(kind+"/"+name+" policy UID mismatch; refusing abort")
    spec_uid = obj.get("spec", {}).get("policyRef", {}).get("uid")
    if spec_uid and spec_uid != run["policy_uid"]:
        raise ValueError(kind+"/"+name+" spec policy UID mismatch; refusing abort")

def workload_uid_owned(obj, run, kind):
    name = obj.get("metadata", {}).get("name", "")
    spec_uid = obj.get("spec", {}).get("workloadRef", {}).get("uid")
    if spec_uid != run.get("workload_uid"):
        raise ValueError(kind+"/"+name+" workload UID mismatch; refusing abort")
    labels = obj.get("metadata", {}).get("labels", {})
    if labels.get(POLICY_UID) not in (None, "", run["policy_uid"]):
        raise ValueError(kind+"/"+name+" policy UID mismatch; refusing abort")
    for status in obj.get("status", {}).get("clusters", []):
        status_uid = status.get("workloadUID") or status.get("workloadRef", {}).get("uid")
        if status_uid and status_uid != run.get("workload_uid"):
            raise ValueError(kind+"/"+name+" status workload UID mismatch; refusing abort")

def request_uids(objects):
    return {obj["metadata"].get("uid") for _, kind, obj in objects if kind == "restorerequests" and obj["metadata"].get("uid")}

def owner_ref_uids(obj):
    return {owner.get("uid") for owner in obj.get("metadata", {}).get("ownerReferences", []) if owner.get("uid")}

def restore_plan_owned(obj, run, request_ids, kind):
    name = obj.get("metadata", {}).get("name", "")
    spec = obj.get("spec", {})
    if spec.get("workloadRef", {}).get("uid") != run.get("workload_uid"):
        raise ValueError(kind+"/"+name+" workload UID mismatch; refusing abort")
    if spec.get("requestUID") not in request_ids:
        raise ValueError(kind+"/"+name+" request UID mismatch; refusing abort")

def restore_plans_for_requests(cluster, run, request_ids):
    if not request_ids:
        return []
    matches = []
    for obj in get(cluster, "restoreplans")["items"]:
        if obj.get("spec", {}).get("requestUID") in request_ids:
            restore_plan_owned(obj, run, request_ids, "restoreplans")
            matches.append(obj)
    return matches

ALLOWED_RESOURCE_SELECTOR_FIELDS = {"apiVersion", "kind", "namespace", "name"}

def checked_resource_selectors(obj):
    name = obj.get("metadata", {}).get("name", "")
    checked = []
    for item in obj.get("spec", {}).get("resourceSelectors", []):
        unsupported = sorted(set(item) - ALLOWED_RESOURCE_SELECTOR_FIELDS)
        if unsupported:
            raise ValueError("propagationpolicies/"+name+" uses unsupported resource selector fields: "+", ".join(unsupported))
        if not item.get("apiVersion") or not item.get("kind") or not item.get("name"):
            raise ValueError("propagationpolicies/"+name+" must use explicit apiVersion/kind/name selectors")
        checked.append(item)
    return checked

def selector_targets(obj):
    return {(item.get("apiVersion"), item.get("kind"), item.get("namespace") or NS, item.get("name"))
            for item in checked_resource_selectors(obj)}

def selector_scope(obj):
    return sorted(({key: item[key] for key in ("apiVersion", "kind", "namespace", "name") if key in item}
                   for item in checked_resource_selectors(obj)),
                  key=lambda item: (item.get("apiVersion", ""), item.get("kind", ""),
                                    item.get("namespace", ""), item.get("name", "")))

def placement_scope(obj):
    metadata = obj.get("metadata", {})
    labels = metadata.get("labels", {})
    return {"name": metadata.get("name"), "uid": metadata.get("uid"),
            "policy_uid": labels.get(POLICY_UID, ""),
            "owner_uids": sorted(owner_ref_uids(obj)),
            "resource_selectors": selector_scope(obj)}

def persisted_placement_scopes(run):
    scopes = []
    seen = set()
    for scope in run.get("failed_abort_placements", []):
        name = scope.get("name")
        if name and name not in seen:
            scopes.append(scope)
            seen.add(name)
    for name in run.get("failed_abort_placement_names", []):
        if name and name not in seen:
            scopes.append({"name": name})
            seen.add(name)
    return scopes

def placement_matches_scope(obj, scope, run, request_ids):
    if not scope.get("name") or obj.get("metadata", {}).get("name") != scope.get("name"):
        return False
    if not scope.get("uid") or not scope.get("resource_selectors"):
        return False
    if selector_scope(obj) != scope["resource_selectors"]:
        return False
    labels = obj.get("metadata", {}).get("labels", {})
    if labels.get(POLICY_UID) not in (None, "", run["policy_uid"]):
        return False
    if labels.get(POLICY_UID) == run["policy_uid"]:
        return True
    return bool(owner_ref_uids(obj) & request_ids & set(scope.get("owner_uids", [])))

def propagation_policy_for_objects(obj, selected, request_ids, run):
    labels = obj.get("metadata", {}).get("labels", {})
    try:
        targets = selector_targets(obj)
    except ValueError:
        if labels.get(POLICY_UID) == run["policy_uid"] or bool(owner_ref_uids(obj) & request_ids):
            raise
        return False
    if not targets:
        return False
    selected_keys = {(target.get("apiVersion"), target.get("kind"),
                      target.get("metadata", {}).get("namespace") or NS,
                      target.get("metadata", {}).get("name")) for target in selected}
    if not targets.issubset(selected_keys):
        return False
    if labels.get(POLICY_UID) not in (None, "", run["policy_uid"]):
        return False
    return labels.get(POLICY_UID) == run["policy_uid"] or bool(owner_ref_uids(obj) & request_ids)

def propagation_policies_for_objects(run, selected, request_ids):
    if not selected:
        return []
    matches = []
    for obj in get("karmada", "propagationpolicies")["items"]:
        if propagation_policy_for_objects(obj, selected, request_ids, run):
            matches.append(obj)
    return matches

def persisted_abort_placements(run, request_ids):
    matches = []
    for scope in persisted_placement_scopes(run):
        name = scope.get("name")
        if not name:
            continue
        try:
            obj = get("karmada", "propagationpolicies", name)
        except RuntimeError as exc:
            if "NotFound" in str(exc) or "not found" in str(exc):
                continue
            raise
        if not obj or not obj.get("metadata", {}).get("uid"):
            continue
        if not placement_matches_scope(obj, scope, run, request_ids):
            raise ValueError("propagationpolicies/"+name+" does not match validated abort scope; refusing abort")
        matches.append(obj)
    return matches

def collect_failed_abort_objects(run, previous_request_ids=None):
    operations = []
    for kind in ABORT_OPERATION_KINDS:
        for obj in get("karmada", kind, selector=POLICY_UID+"="+run["policy_uid"])["items"]:
            policy_uid_owned(obj, run, kind)
            operations.append(("karmada", kind, obj))
    request_ids = set(previous_request_ids or []) | request_uids(operations)
    plans = [("karmada", "restoreplans", obj) for obj in restore_plans_for_requests("karmada", run, request_ids)]
    selected = [obj for _, _, obj in operations + plans]
    placement_by_name = {obj["metadata"]["name"]: obj
                         for obj in propagation_policies_for_objects(run, selected, request_ids)}
    for obj in persisted_abort_placements(run, request_ids):
        placement_by_name.setdefault(obj["metadata"]["name"], obj)
    placements = [("karmada", "propagationpolicies", obj) for _, obj in sorted(placement_by_name.items())]
    objects = operations + plans
    for obj in get("aws", "fluidcrmigrations")["items"]:
        if obj.get("spec", {}).get("workloadRef", {}).get("uid") != run.get("workload_uid"):
            continue
        workload_uid_owned(obj, run, "fluidcrmigrations")
        objects.append(("aws", "fluidcrmigrations", obj))
    for obj in restore_plans_for_requests("aws", run, request_ids):
        objects.append(("aws", "restoreplans", obj))
    objects.extend(placements)
    return objects

def member_abort_migrations(run):
    matches = []
    for obj in get("aws", "fluidcrmigrations")["items"]:
        if obj.get("spec", {}).get("workloadRef", {}).get("uid") == run.get("workload_uid"):
            workload_uid_owned(obj, run, "fluidcrmigrations")
            matches.append(obj)
    return matches

def abort_restore_plan_matches(run, request_ids):
    matches = []
    for cluster in ("karmada", "aws"):
        for obj in restore_plans_for_requests(cluster, run, request_ids):
            matches.append((cluster, "restoreplans", obj))
    return matches

def failed_abort_remaining(run, include_placements=True):
    request_ids = set(run.get("failed_abort_request_uids", []))
    remaining = []
    for kind in ABORT_OPERATION_KINDS:
        for obj in get("karmada", kind, selector=POLICY_UID+"="+run["policy_uid"])["items"]:
            policy_uid_owned(obj, run, kind)
            remaining.append("karmada/"+kind+"/"+obj["metadata"]["name"])
    for obj in member_abort_migrations(run):
        remaining.append("aws/fluidcrmigrations/"+obj["metadata"]["name"])
    for cluster, kind, obj in abort_restore_plan_matches(run, request_ids):
        remaining.append(cluster+"/"+kind+"/"+obj["metadata"]["name"])
    if include_placements:
        for obj in persisted_abort_placements(run, request_ids):
            remaining.append("karmada/propagationpolicies/"+obj["metadata"]["name"])
    return remaining

def wait_failed_abort_removed(run, timeout=180, interval=5, include_placements=True):
    deadline = time.monotonic() + timeout
    while True:
        remaining = failed_abort_remaining(run, include_placements=include_placements)
        if not remaining:
            return
        if time.monotonic() >= deadline:
            raise RuntimeError("aborted recovery resources still exist; refusing scale/cleanup: " + ", ".join(remaining))
        time.sleep(interval)

def recover_failed_abort_placement_scopes(evidence, run):
    names = set(run.get("failed_abort_placement_names", []))
    scoped = {scope.get("name") for scope in run.get("failed_abort_placements", [])
              if scope.get("name") and scope.get("uid") and scope.get("resource_selectors")}
    missing = names - scoped
    if not missing:
        return False
    recovered = {scope.get("name"): scope for scope in run.get("failed_abort_placements", []) if scope.get("name")}
    directory = evidence/ABORT_BACKUP_DIR
    for path in sorted(directory.glob("*.json")) if directory.exists() else []:
        backup = read(path)
        if backup.get("run_id") != run["run_id"] or backup.get("policy_uid") != run["policy_uid"]:
            continue
        for item in backup.get("objects", []):
            if item.get("kind") != "propagationpolicies":
                continue
            obj = item.get("object", {})
            name = obj.get("metadata", {}).get("name")
            if name in missing:
                recovered[name] = placement_scope(obj)
    updated = recovered.keys() >= names and recovered != {scope.get("name"): scope for scope in run.get("failed_abort_placements", []) if scope.get("name")}
    if updated:
        run["failed_abort_placements"] = [recovered[name] for name in sorted(recovered)]
    return updated

def backup_failed_abort(evidence, run, objects, request_ids):
    directory = evidence/ABORT_BACKUP_DIR
    directory.mkdir(parents=True, exist_ok=True)
    path = directory/(dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")+".json")
    backup = {
        "run_id": run["run_id"],
        "policy_uid": run["policy_uid"],
        "workload_uid": run.get("workload_uid"),
        "request_uids": sorted(request_ids),
        "created_at": now(),
        "objects": [{"cluster": cluster, "kind": kind, "object": obj} for cluster, kind, obj in objects],
    }
    save(path, backup)
    return path

def delete_url(obj, plural):
    api = obj.get("apiVersion", "")
    name = quote(obj["metadata"]["name"], safe="")
    namespace = quote(obj.get("metadata", {}).get("namespace") or NS, safe="")
    if "/" in api:
        group, version = api.split("/", 1)
        return "/apis/"+quote(group, safe="")+"/"+quote(version, safe="")+"/namespaces/"+namespace+"/"+plural+"/"+name
    return "/api/"+quote(api, safe="")+"/namespaces/"+namespace+"/"+plural+"/"+name

def delete_checked(cluster, kind, obj):
    try:
        latest = get(cluster, kind, obj["metadata"]["name"])
    except RuntimeError as exc:
        if "NotFound" in str(exc) or "not found" in str(exc):
            return False
        raise
    if not latest or not latest.get("metadata", {}).get("uid"):
        return False
    if latest.get("metadata", {}).get("uid") != obj.get("metadata", {}).get("uid"):
        raise ValueError(kind+"/"+obj["metadata"]["name"]+" UID changed before delete; refusing abort")
    options = {"apiVersion":"v1", "kind":"DeleteOptions", "preconditions":{"uid":latest["metadata"]["uid"]}}
    try:
        kubectl(cluster, "delete", "--raw="+delete_url(latest, kind), "--filename=-", obj=options)
    except RuntimeError as exc:
        if "NotFound" in str(exc) or "not found" in str(exc):
            return False
        raise
    return True

def abort_failed_run(evidence, confirm, wait_timeout=180, wait_interval=5):
    run = read(evidence/"run.json")
    if confirm != run["run_id"]:
        raise ValueError("--confirm must equal run_id; abort deletes failed-run recovery objects")
    if run.get("phase") not in ("failed", "aborted"):
        raise ValueError("abort requires failed or already aborted run phase")
    if recover_failed_abort_placement_scopes(evidence, run):
        save(evidence/"run.json", run)
    if run.get("phase") == "aborted":
        wait_failed_abort_removed(run, wait_timeout, wait_interval)
        return
    patch_policy(run)
    previous_request_ids = set(run.get("failed_abort_request_uids", []))
    objects = collect_failed_abort_objects(run, previous_request_ids)
    request_ids = previous_request_ids | request_uids(objects)
    backup = backup_failed_abort(evidence, run, objects, request_ids)
    run["failed_abort_request_uids"] = sorted(request_ids)
    placement_names = set(run.get("failed_abort_placement_names", []))
    placement_scopes = {scope.get("name"): scope for scope in run.get("failed_abort_placements", []) if scope.get("name")}
    for _, kind, obj in objects:
        if kind == "propagationpolicies":
            name = obj["metadata"]["name"]
            placement_names.add(name)
            placement_scopes[name] = placement_scope(obj)
    run["failed_abort_placement_names"] = sorted(placement_names)
    run["failed_abort_placements"] = [placement_scopes[name] for name in sorted(placement_scopes)]
    save(evidence/"run.json", run)
    producer_objects = [(cluster, kind, obj) for cluster, kind, obj in objects if kind != "propagationpolicies"]
    initial_placements = {obj["metadata"]["name"]: obj for _, kind, obj in objects if kind == "propagationpolicies"}
    for cluster, kind, obj in producer_objects:
        if cluster == "karmada":
            if kind == "restoreplans":
                restore_plan_owned(obj, run, request_ids, kind)
            else:
                policy_uid_owned(obj, run, kind)
        elif kind == "restoreplans":
            restore_plan_owned(obj, run, request_ids, kind)
        else:
            workload_uid_owned(obj, run, kind)
        event(evidence, "failed_abort_delete_requested", cluster=cluster, kind=kind,
              resource_name=obj["metadata"]["name"], uid=obj["metadata"].get("uid"))
        delete_checked(cluster, kind, obj)
    wait_failed_abort_removed(run, wait_timeout, wait_interval, include_placements=False)
    placement_objects = initial_placements
    for obj in persisted_abort_placements(run, request_ids):
        placement_objects[obj["metadata"]["name"]] = obj
    for obj in placement_objects.values():
        if obj["metadata"]["name"] not in run["failed_abort_placement_names"]:
            raise ValueError("propagationpolicies/"+obj["metadata"]["name"]+" not in persisted abort scope")
        event(evidence, "failed_abort_delete_requested", cluster="karmada", kind="propagationpolicies",
              resource_name=obj["metadata"]["name"], uid=obj["metadata"].get("uid"))
        delete_checked("karmada", "propagationpolicies", obj)
    wait_failed_abort_removed(run, wait_timeout, wait_interval)
    owned(run, "karmada", "statefulset", run["app"], run["workload_uid"])
    kubectl("karmada", "-n", NS, "scale", "statefulset", run["app"], "--replicas=0")
    run.update(phase="aborted", failed_abort_requested_at=now())
    save(evidence/"run.json", run)
    event(evidence, "failed_run_aborted", backup=str(backup.relative_to(evidence)),
          warning="failed recovery objects were deleted; no success status fabricated")

def active_operations(run):
    active = []
    for kind, terminal in (("spotreplacements", {"Completed", "Rejected"}),
                           ("spotrecoveries", {"Completed", "Rejected"}),
                           ("restorerequests", {"Verified", "Rejected"})):
        for obj in get("karmada", kind, selector=POLICY_UID+"="+run["policy_uid"])["items"]:
            if obj.get("status", {}).get("phase") not in terminal:
                active.append(kind+"/"+obj["metadata"]["name"])
    return active

def wait_quiescent(run):
    deadline = time.monotonic()+180
    while True:
        active = active_operations(run)
        for obj in get("aws", "fluidcrmigrations")["items"]:
            if obj.get("spec", {}).get("workloadRef", {}).get("uid") != run.get("workload_uid"):
                continue
            phase = obj.get("status", {}).get("phase")
            if "schedule" in obj.get("spec", {}):
                quiet = (not obj["spec"]["schedule"].get("enabled", True) and phase == "Paused"
                         and obj.get("status", {}).get("observedGeneration") == obj["metadata"].get("generation"))
            else:
                quiet = phase in ("Completed", "Failed")
            if not quiet:
                active.append("fluidcrmigration/"+obj["metadata"]["name"])
        if not active:
            return
        if time.monotonic() >= deadline:
            raise RuntimeError("suspended but operations are not quiescent; Pods/VMs retained: "+", ".join(active))
        time.sleep(5)

def observe_policy(evidence, run):
    obj = owned(run, "karmada", "trainingpolicy", run["policy"], run["policy_uid"])
    decision = obj.get("status", {}).get("policy", {})
    if run["experiment"] == "checkpoint" and decision.get("decisionStage"):
        if (decision.get("onDemandWorkers"), decision.get("spotWorkers")) != (1, 1):
            raise RuntimeError("fixed composition not honored; inspect controller image and policy")
    checkpoint = obj.get("status", {}).get("checkpoint", {})
    if run["experiment"] == "cost" and run["arm"] == "A":
        if checkpoint.get("reason") == "periodic_disabled":
            run["periodic_disabled_observed"] = True
        migrations = get("aws", "fluidcrmigrations")["items"]
        for migration in migrations:
            spec = migration.get("spec", {})
            if spec.get("workloadRef", {}).get("name") == run["app"]:
                raise RuntimeError("A baseline contains a checkpoint schedule/run; inspect coordinator image and policy")
    if checkpoint.get("intervalSource") == "checkpoint-cost-analytic":
        run["analytic_interval_observed"] = True
    if run["scenario"] == "risk-rise":
        risk = owned(run, "karmada", "spotriskprofile", run["risk"])
        status = risk.get("status", {})
        if (status.get("ready") and status.get("observedGeneration") == risk["metadata"]["generation"]
                and status.get("lambdaPerHour") == run["changed_risk"]
                and decision.get("lambdaPerHour") == run["changed_risk"]):
            run["changed_risk_consumed"] = True
    save(evidence/"policy-latest.json", obj)


def advance_risk_schedule(evidence, run, elapsed):
    entries = run.setdefault("risk_schedule_observations", [])
    if entries and not entries[-1].get("consumed_at"):
        current = entries[-1]
        risk = owned(run, "karmada", "spotriskprofile", run["risk"])
        status = risk.get("status", {})
        policy = read(evidence/"policy-latest.json")
        decision = policy.get("status", {}).get("policy", {})
        value = current["lambda_per_hour"]
        if (risk.get("spec", {}).get("staticLambdaPerHour") == value
                and status.get("ready")
                and status.get("observedGeneration") == risk["metadata"]["generation"]
                and status.get("lambdaPerHour") == value
                and decision.get("lambdaPerHour") == value):
            current["consumed_at"] = now()
            current["decision"] = decision
            event(evidence, "scheduled_risk_consumed", index=len(entries)-1, decision=decision)
    index = len(entries)
    if index < len(run["risk_schedule"]) and elapsed >= run["risk_schedule"][index]["after_seconds"]:
        if entries and not entries[-1].get("consumed_at"):
            raise RuntimeError("previous scheduled risk not consumed before next offset")
        entry = run["risk_schedule"][index]
        set_risk(evidence, entry["lambda_per_hour"])
        entries.append(dict(entry, requested_at=now(), actual_after_seconds=elapsed))
        event(evidence, "scheduled_risk_requested", index=index, **entries[-1])
    save(evidence/"run.json", run)

def set_risk(evidence, value):
    if not math.isfinite(value) or value < 0:
        raise ValueError("risk must be finite and nonnegative")
    run = read(evidence/"run.json")
    owned(run,"karmada","spotriskprofile",run["risk"])
    kubectl("karmada","-n",NS,"patch","spotriskprofile",run["risk"],"--type=merge","-p",
            json.dumps({"spec":{"staticLambdaPerHour":value}}))
    event(evidence,"risk_requested",value=value)

def training_events(text, run_id):
    for line in text.splitlines():
        if "EVAL_EVENT " not in line:
            continue
        try:
            value = json.loads(line.split("EVAL_EVENT ",1)[1])
            if value.get("run_id") == run_id:
                yield value
        except (ValueError,TypeError):
            continue

def verify_group_recovery(evidence, run):
    role = "training.dcnlab.com/role"
    requests = get("karmada", "restorerequests", selector=POLICY_UID+"="+run["policy_uid"]+","+role+"=group-restore")["items"]
    save(evidence/"recovery-final.json", requests)
    if not requests or any(
            obj.get("metadata", {}).get("labels", {}).get(POLICY_UID) != run["policy_uid"]
            or obj.get("metadata", {}).get("labels", {}).get(role) != "group-restore"
            or obj.get("status", {}).get("phase") != "Verified" for obj in requests):
        raise RuntimeError("group recovery requires nonempty policy-UID-owned group restores all Verified")
    snapshots = {cluster: get(cluster, "nodeprovisions")["items"] for cluster in ("karmada", "aws")}
    save(evidence/"group-nodeprovisions-final.json", snapshots)
    for obj in requests:
        annotations = obj["metadata"].get("annotations", {})
        name = annotations.get("training.dcnlab.com/group-old-nodeprovision")
        old_uid = annotations.get("training.dcnlab.com/group-old-nodeprovision-uid")
        if not name or not old_uid:
            raise RuntimeError("group restore missing unambiguous old NodeProvision identities")
        # Run-unique old names must disappear in both clusters; member UIDs differ.
        for cluster in ("karmada", "aws"):
            for node in snapshots[cluster]:
                metadata = node.get("metadata", {})
                if metadata.get("name") == name:
                    raise RuntimeError("group recovery/cleanup incomplete: old NodeProvision remains in "+cluster)

def verify_scenario(evidence, run):
    if run.get("experiment") == "cost" and run.get("arm") == "A" and not run.get("periodic_disabled_observed"):
        raise RuntimeError("A baseline disabled acknowledgement missing; inspect coordinator image")
    if run["scenario"] == "risk-schedule":
        observations = run.get("risk_schedule_observations", [])
        if len(observations) != len(run["risk_schedule"]) or not all(x.get("consumed_at") for x in observations):
            raise RuntimeError("risk schedule incomplete; increase goal_steps or inspect risk consumption")
        if active_operations(run):
            raise RuntimeError("risk schedule recovery/cleanup still active")
    if (run.get("arm") == "D" or (run.get("experiment") == "cost" and run.get("arm") == "B")) and not run.get("analytic_interval_observed"):
        raise RuntimeError("analytic checkpoint interval was never observed; bootstrap-only run is not dynamic-policy evidence")
    if run["scenario"] == "risk-rise" and not run.get("changed_risk_consumed"):
        raise RuntimeError("changed risk was not consumed by the policy before completion")
    if run["scenario"] == "interruption":
        if not (evidence/"fis-started.json").exists():
            raise RuntimeError("no recorded FIS interruption; invalid interruption run")
        import fis
        result = read(evidence/"fis-started.json")
        experiment_id = result.get("experiment", {}).get("id")
        if not experiment_id:
            raise RuntimeError("FIS start is unconfirmed; inspect AWS before retrying")
        status = fis.aws(run["region"], "fis", "get-experiment", "--id", experiment_id)
        save(evidence/"fis-final.json", status)
        if status["experiment"]["state"]["status"] != "completed":
            raise RuntimeError("FIS experiment has not completed successfully")
        target = read(evidence/"fis-target.json")
        ec2 = fis.aws(run["region"], "ec2", "describe-instances", "--instance-ids", target["instance_id"])
        save(evidence/"fis-instance-final.json", ec2)
        states = [i["State"]["Name"] for r in ec2["Reservations"] for i in r["Instances"]]
        if len(states) != 1 or states[0] not in ("terminated", "stopped"):
            raise RuntimeError("actual target interruption is not yet verified")
    if run["scenario"] == "interruption" or (run["scenario"] == "risk-rise" and run["experiment"] == "cost" and run["arm"] == "B"):
        if run.get("experiment") == "checkpoint":
            verify_group_recovery(evidence, run)
            return
        operations = []
        for kind in ("spotreplacements", "spotrecoveries"):
            operations.extend(get("karmada", kind, selector=POLICY_UID+"="+run["policy_uid"])["items"])
        save(evidence/"recovery-final.json", operations)
        if not operations or any(o.get("status", {}).get("phase") != "Completed" for o in operations):
            raise RuntimeError("scenario recovery/cleanup has not completed; exclude from successful runs")

def start(evidence):
    run = read(evidence/"run.json")
    if run["phase"] != "prepared":
        raise ValueError("start requires prepared run (no resume of finished measurements)")
    owned(run,"karmada","statefulset",run["app"],run["workload_uid"])
    collector = subprocess.Popen([sys.executable,str(HERE/"collect.py"),"--evidence",str(evidence)])
    run.update(phase="running",started_at=now())
    save(evidence/"run.json",run)
    event(evidence,"provisioning_started")
    seen, completed, learning_start, changed = set(), set(), None, False
    logged = set()
    deadline = time.monotonic()+run["max_seconds"]
    try:
        kubectl("karmada","-n",NS,"scale","statefulset",run["app"],"--replicas=2")
        patch_policy(run,False)
        while time.monotonic() < deadline:
            if collector.poll() is not None:
                raise RuntimeError("collector stopped unexpectedly")
            observe_policy(evidence, run)
            pods = get("aws","pods",selector="app="+run["app"])["items"]
            for pod in pods:
                if pod["status"].get("phase") != "Running":
                    continue
                try:
                    logs = kubectl("aws","-n",NS,"logs",pod["metadata"]["name"],"-c","trainer","--timestamps","--tail=200")
                except RuntimeError as exc:
                    event(evidence,"log_poll_error",message=str(exc))
                    continue
                for value in training_events(logs,run["run_id"]):
                    key = json.dumps(value, sort_keys=True)
                    if key not in logged:
                        with (evidence/"monitor-events.jsonl").open("a", encoding="utf-8") as out:
                            out.write(key+"\n")
                        logged.add(key)
                    rank = value.get("rank")
                    if value.get("event") in ("step","completed") and value.get("global_step",0)>0:
                        seen.add(rank)
                    if value.get("event") == "completed" and value.get("global_step",0)>=run["goal_steps"]:
                        completed.add(rank)
            if {0,1}.issubset(seen) and learning_start is None:
                learning_start = time.monotonic()
                event(evidence,"all_ranks_progress_observed")
            if run["scenario"]=="risk-rise" and learning_start is not None and not changed and time.monotonic()-learning_start>=run["risk_change_after_seconds"]:
                set_risk(evidence,run["changed_risk"])
                changed=True
            if {0,1}.issubset(completed):
                if run["scenario"]=="risk-rise" and not changed:
                    raise RuntimeError("goal completed before scheduled risk event; increase goal_steps")
                verify_scenario(evidence, run)
                run.update(phase="completed",completed_at=now())
                event(evidence,"all_ranks_completed")
                break
            if run["scenario"] == "risk-schedule" and learning_start is not None:
                advance_risk_schedule(evidence, run, time.monotonic()-learning_start)
            time.sleep(5)
        else:
            raise TimeoutError("max_seconds reached")
    except (Exception,KeyboardInterrupt) as exc:
        run.update(phase="failed",failure_reason=str(exc) or "interrupted",ended_at=now())
        event(evidence,"run_failed",message=run["failure_reason"])
        raise
    finally:
        save(evidence/"run.json",run)
        try:
            stop(evidence)
            time.sleep(2)
        finally:
            collector.terminate()
            try:
                collector.wait(timeout=40)
            except subprocess.TimeoutExpired:
                collector.kill()
                collector.wait()
            print("Collection ended. Check runner-events for successful stop; VMs/PVCs retained until scoped cleanup.",flush=True)

def cleanup(evidence, confirm):
    run = read(evidence/"run.json")
    if confirm != run["run_id"]:
        raise ValueError("--confirm must equal run_id; cleanup terminates this run's VMs")
    ledger = read(evidence/"created.json")
    policy_entries=[e for e in ledger if e["kind"]=="TrainingPolicy"]
    if policy_entries:
        run.setdefault("policy_uid",policy_entries[0]["uid"])
        current = kubectl("karmada", "-n", NS, "get", "trainingpolicy", run["policy"], "--ignore-not-found", "-o", "json")
        if current.strip():
            patch_policy(run)
        sts_entries = [e for e in ledger if e["kind"] == "StatefulSet"]
        if sts_entries:
            run.setdefault("workload_uid", sts_entries[0]["uid"])
        if run.get("phase") == "aborted":
            wait_failed_abort_removed(run)
        wait_quiescent(run)
    # Delete workload first and wait for Karmada's member deletion. Keep dataset and checkpoints.
    for item in sorted(ledger,key=lambda e: 0 if e["kind"]=="StatefulSet" else 1):
        if item["kind"] in ("PersistentVolume","PersistentVolumeClaim"):
            continue
        args=["get",item["kind"],item["name"],"--ignore-not-found","-o","json"]
        if item["namespaced"]:
            args=["-n",NS]+args
        raw=kubectl(item["cluster"],*args)
        if not raw.strip():
            continue
        obj=json.loads(raw)
        if obj["metadata"]["uid"] != item["uid"]:
            raise ValueError("recreated resource; refusing delete "+item["name"])
        kubectl(item["cluster"],"delete","-f","-","--wait=true","--timeout=20s",obj=obj)
    if policy_entries:
        for obj in get("karmada","nodeprovisions",selector=POLICY_UID+"="+run["policy_uid"])["items"]:
            event(evidence,"nodeprovision_delete_requested",name=obj["metadata"]["name"],status=obj.get("status",{}))
            kubectl("karmada","delete","-f","-","--wait=false",obj=obj)
        # These objects use labels rather than owner references; keep the UID boundary.
        for kind in ("fluidcrmigrations", "trainingruntimes", "restorerequests", "spotrecoveries", "spotreplacements", "propagationpolicies"):
            for obj in get("karmada", kind, selector=POLICY_UID+"="+run["policy_uid"])["items"]:
                if obj["metadata"].get("labels", {}).get(POLICY_UID) != run["policy_uid"]:
                    raise ValueError("child policy UID changed; refusing cleanup")
                kubectl("karmada", "delete", "-f", "-", "--wait=false", obj=obj)
    event(evidence,"cleanup_requested",note="PVC/PV retained; verify member finalizers and actual EC2 termination")
    print("Scoped deletion requested. No finalizers removed; PVC/PV retained. Verify EC2 termination.")

def main(experiment=None):
    parser=argparse.ArgumentParser(description=__doc__)
    sub=parser.add_subparsers(dest="command",required=True)
    p=sub.add_parser("capture-policy")
    p.add_argument("--name",default="trainer-realign")
    p.add_argument("--output",required=True,type=Path)
    p=sub.add_parser("render")
    p.add_argument("--config",required=True,type=Path)
    p.add_argument("--run-id",required=True)
    if experiment is None:
        p.add_argument("--experiment",choices=["cost","checkpoint"],required=True)
    else:
        p.set_defaults(experiment=experiment)
    arms = {"cost": ["A", "B"], "checkpoint": ["F60", "F300", "F600", "D"]}
    p.add_argument("--arm",required=True,choices=arms.get(experiment))
    p.add_argument("--out",required=True,type=Path)
    for name in ("prepare","start","stop","risk","cleanup","abort"):
        p=sub.add_parser(name)
        p.add_argument("--evidence",required=True,type=Path)
        if name=="risk":
            p.add_argument("--value",required=True,type=float)
        if name in ("cleanup", "abort"):
            p.add_argument("--confirm",required=True)
    args=parser.parse_args()
    if experiment is not None and hasattr(args, "evidence"):
        recorded = read(args.evidence / "run.json").get("experiment")
        if recorded != experiment:
            raise ValueError("evidence belongs to a different experiment: " + str(recorded))
    if args.command=="capture-policy":
        obj=get("karmada","trainingpolicy",args.name)
        save(args.output,{"apiVersion":obj["apiVersion"],"kind":obj["kind"],"spec":obj["spec"]})
    elif args.command=="render":
        c=read(args.config)
        source=read(args.config.resolve().parent/c["policy_source"])
        run,member,karmada,policy=render(c,source,args.run_id,args.experiment,args.arm)
        args.out.mkdir(parents=True,exist_ok=False)
        save(args.out/"run.json",run)
        save(args.out/"member.yaml",{"apiVersion":"v1","kind":"List","items":member})
        save(args.out/"karmada.yaml",{"apiVersion":"v1","kind":"List","items":karmada})
        save(args.out/"policy.yaml",policy)
        print("Rendered JSON-compatible YAML; inspect before prepare:",args.out)
    elif args.command=="risk":
        set_risk(args.evidence,args.value)
    elif args.command=="cleanup":
        cleanup(args.evidence,args.confirm)
    elif args.command=="abort":
        abort_failed_run(args.evidence,args.confirm)
    else:
        globals()[args.command](args.evidence)

if __name__=="__main__":
    try:
        main()
    except (ValueError,RuntimeError,TimeoutError,subprocess.SubprocessError) as exc:
        print("ERROR:",exc,file=sys.stderr)
        sys.exit(1)
