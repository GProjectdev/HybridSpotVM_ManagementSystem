#!/usr/bin/env python3
"""Evaluation preparation and bounded execution. Requires Python 3.10+, kubectl."""
import argparse
import copy
import datetime as dt
import json
import math
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time

HERE = Path(__file__).resolve().parent
NS = "fluidcr-realign-121040"
LABEL = "training.dcnlab.com/evaluation-run"
POLICY_UID = "training.dcnlab.com/policy-uid"

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
    if c.get("scenario") not in ("constant", "risk-rise", "interruption"):
        raise ValueError("scenario must be constant, risk-rise, or interruption")
    if experiment == "cost" and arm == "A" and c["scenario"] == "interruption":
        raise ValueError("All On-Demand has no Spot target; use a constant control run for interruption comparisons")
    if c["scenario"] == "risk-rise" and not 0 < c.get("risk_change_after_seconds", 0) < c["max_seconds"]:
        raise ValueError("risk change must be positive and before timeout")
    if c["region"] != "ap-northeast-2" or c["availability_zone"] != "ap-northeast-2c":
        raise ValueError("this evaluation is scoped to ap-northeast-2c")

def render(c, source, run_id, experiment, arm):
    validate(c, run_id, experiment, arm)
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
    policy_spec["policy"]["minOnDemand"] = 2 if arm == "A" else 1
    if experiment == "checkpoint":
        policy_spec["policy"]["fixedOnDemand"] = 1
    interval = 300 if experiment == "cost" or arm == "D" else int(arm[1:])
    checkpoint = copy.deepcopy(source["spec"].get("checkpoint", {}))
    checkpoint.pop("candidateIntervalSeconds", None)
    checkpoint.pop("measuredCosts", None)
    checkpoint.pop("paperProfile", None)
    checkpoint.update(minIntervalSeconds=60 if arm == "D" else interval,
                      maxIntervalSeconds=600 if arm == "D" else interval,
                      riskBands=[{"maxLambdaPerHour": 1.0, "intervalSeconds": interval}], resume=True)
    policy_spec["checkpoint"] = checkpoint
    policy = resource("training.dcnlab.com/v1alpha1", "TrainingPolicy", app, run_id, policy_spec)
    policy["metadata"]["annotations"] = {"training.dcnlab.com/suspend": "true"}
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
        "EVAL_SEED": c["seed"], "EVAL_DATA_ROOT": "/datasets/cifar10"}.items()]
    env = [{"name":"POD_NAME","valueFrom":{"fieldRef":{"fieldPath":"metadata.name"}}},
           {"name":"RANK","valueFrom":{"fieldRef":{"fieldPath":"metadata.labels['apps.kubernetes.io/pod-index']"}}}] + env
    container = {"name": "trainer", "image": c["training_image"], "imagePullPolicy": "Always",
                 "command": ["python", "-u", "/workspace/train_resnet18.py"], "env": env,
                 "ports": [{"name":"control","containerPort":8298},{"name":"rendezvous","containerPort":29500}],
                 "resources": {"requests": {"cpu":"2","memory":"6Gi","nvidia.com/gpu":"1"},
                               "limits": {"cpu":"4","memory":"12Gi","nvidia.com/gpu":"1"}},
                 "readinessProbe": {"tcpSocket":{"port":"control"},"initialDelaySeconds":30},
                 "volumeMounts": [{"name":"training-script","mountPath":"/workspace","readOnly":True},
                                  {"name":"dataset","mountPath":"/datasets/cifar10","readOnly":True},
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
    service = resource("v1","Service",app,run_id,
                       {"clusterIP":"None","publishNotReadyAddresses":True,"selector":{"app":app},
                        "ports":[{"name":"control","port":8298},{"name":"rendezvous","port":29500}]})
    script = resource("v1","ConfigMap",app+"-script",run_id)
    script["data"] = {p.name: p.read_text(encoding="utf-8") for p in (HERE/"resnet18").glob("*.py") if not p.name.startswith("test")}
    if "train_resnet18.py" not in script["data"]:
        raise ValueError("missing training entrypoint")
    placement = resource("policy.karmada.io/v1alpha1","PropagationPolicy",app+"-placement",run_id,
                         {"resourceSelectors":[{"apiVersion":o["apiVersion"],"kind":o["kind"],"name":o["metadata"]["name"]} for o in [service,script,sts]],
                          "placement":{"clusterAffinity":{"clusterNames":[c["cluster"]]}}})
    run = dict(c, run_id=run_id, app=app, policy=app, risk=app+"-risk", experiment=experiment, arm=arm,
               model="resnet18", dataset="cifar10", world_size=2, created_at=now(), phase="rendered")
    return run, [pv,data_pvc,ckpt], [service,script,sts,risk,placement], policy

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

def event(evidence, name, **fields):
    with (evidence/"runner-events.jsonl").open("a",encoding="utf-8") as out:
        out.write(json.dumps(dict(timestamp=now(),event=name,**fields))+"\n")

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
    if run.get("arm") == "D" and not run.get("analytic_interval_observed"):
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

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    sub=parser.add_subparsers(dest="command",required=True)
    p=sub.add_parser("capture-policy")
    p.add_argument("--name",default="trainer-realign")
    p.add_argument("--output",required=True,type=Path)
    p=sub.add_parser("render")
    p.add_argument("--config",required=True,type=Path)
    p.add_argument("--run-id",required=True)
    p.add_argument("--experiment",choices=["cost","checkpoint"],required=True)
    p.add_argument("--arm",required=True)
    p.add_argument("--out",required=True,type=Path)
    for name in ("prepare","start","stop","risk","cleanup"):
        p=sub.add_parser(name)
        p.add_argument("--evidence",required=True,type=Path)
        if name=="risk":
            p.add_argument("--value",required=True,type=float)
        if name=="cleanup":
            p.add_argument("--confirm",required=True)
    args=parser.parse_args()
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
    else:
        globals()[args.command](args.evidence)

if __name__=="__main__":
    try:
        main()
    except (ValueError,RuntimeError,TimeoutError,subprocess.SubprocessError) as exc:
        print("ERROR:",exc,file=sys.stderr)
        sys.exit(1)
