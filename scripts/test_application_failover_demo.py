"""Offline contract tests; every kubectl subprocess is mocked."""
import contextlib
import copy
import hashlib
import io
import json
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

import application_failover_demo as demo


def obj(name, identity, **extra):
    return {"metadata": {"name": name, "namespace": demo.NS, "uid": identity, "generation": 1,
                         "resourceVersion": "7"}, **extra}


class DemoTests(unittest.TestCase):
    def setUp(self):
        self.sts = obj(demo.APP, "world-uid", spec={"replicas": 0})
        self.ref = demo.workload_ref(self.sts)
        self.policy = obj(demo.APP, "policy-uid", spec={
            "workloadRef": self.ref, "sourceCluster": "onpre1", "targetWorkers": 1,
            "runtimeRef": {"name": "actual-runtime"},
            "expectedWorldSize": 1, "capacity": {"mode": "Existing", "aws": {"karmadaCluster": "aws"}}})
        self.snapshot = {"rank": 0, "name": demo.APP + "-0", "uid": "pod-uid", "nodeName": "source"}
        self.runtime = obj("actual-runtime", "runtime-uid", spec={
            "workloadRef": self.ref, "sourceCluster": "onpre1", "container": "trainer", "port": 8298},
            status={"clusters": [{"clusterName": "onpre1", "status": {
                "observedGeneration": 1, "sourceWorldUID": "world-uid", "sourcePods": [self.snapshot]}}]})
        self.runtime["metadata"]["labels"] = {demo.GROUP + "policy-uid": "policy-uid"}
        sha = "a" * 64
        self.cp = obj("cp", "cp-uid", spec={"workloadRef": self.ref}, status={"clusters": [{
            "clusterName": "onpre1", "phase": "Completed", "observedGeneration": 1,
            "completionTime": "2026-10-24T00:00:00Z", "pods": [{
                "podName": demo.APP + "-0", "podUID": "old-checkpoint-pod", "nodeName": "source",
                "phase": "Resumed", "checkpointFiles": [{"containerName": "trainer", "filePath": "/archive",
                    "sha256": sha, "durableRef": "file-store:" + demo.NS + "/sha256/" + sha,
                    "exportedAt": "2026-10-24T00:00:00Z"}]}]}]})
        self.cp["metadata"].update(labels={demo.GROUP + "policy-uid": "policy-uid"},
                                   annotations={demo.GROUP + "checkpoint-id": "round-1"})
        self.binding = obj("actual-binding", "binding-uid", spec={
            "resource": dict(self.ref, namespace=demo.NS), "clusters": [{"name": "onpre1", "replicas": 1}]})
        self.source_node = obj("source", "source-node-uid", spec={"unschedulable": True})
        self.target_node = obj("target", "target-node-uid", spec={}, status={
            "conditions": [{"type": "Ready", "status": "True"}]})
        self.responses = {
            ("karmada", "statefulsets.apps", demo.APP): self.sts,
            ("karmada", demo.POLICY, demo.APP): self.policy,
            ("karmada", demo.POLICY, None): {"items": []},
            ("karmada", demo.RUNTIME, None): {"items": [self.runtime]},
            ("karmada", demo.CHECKPOINT, None): {"items": [self.cp]},
            ("karmada", demo.BINDING, None): {"items": [self.binding]},
            ("source", "pods", demo.APP + "-0"): obj(demo.APP + "-0", "pod-uid", spec={"nodeName": "source"}),
            ("source", "nodes", "source"): self.source_node,
            ("target", "nodes", "target"): self.target_node,
        }
        self.calls = []
        self.created = []

    def kubectl(self, argv, **kwargs):
        self.calls.append(argv)
        if "--context" in argv:
            context = argv[argv.index("--context") + 1]
        else:
            config = argv[argv.index("--kubeconfig") + 1]
            context = {"karmada.conf": "karmada", "onpre1.conf": "source", "aws.conf": "target"}[config]
        start = argv.index("--request-timeout=30s") + 1
        action = argv[start]
        self.assertNotIn("delete", argv)
        self.assertNotIn("config", argv)
        if action == "get":
            resource = argv[start + 1]
            name = argv[start + 2] if not argv[start + 2].startswith("-") else None
            response = self.responses[(context, resource, name)]
        elif action == "create":
            response = json.loads(kwargs["input"])
            self.created.append(copy.deepcopy(response))
            response["metadata"]["uid"] = "created-uid"
        elif action == "annotate":
            response = self.binding
        else:
            self.fail("unexpected kubectl action " + action)
        return subprocess.CompletedProcess(argv, 0, json.dumps(response), "")

    def run_cli(self, command, *flags, default_context=True, environ=None):
        out, err = io.StringIO(), io.StringIO()
        prefix = [command, "--karmada-context", "karmada"] if default_context else [command]
        with patch.object(demo.subprocess, "run", side_effect=self.kubectl), \
                patch.dict(demo.os.environ, environ or {}, clear=True):
            with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
                status = demo.main([*prefix, *flags])
        return status, out.getvalue(), err.getvalue()

    def trigger(self):
        return self.run_cli("trigger", "--source-context", "source", "--confirm-workload",
                            demo.NS + "/" + demo.APP)

    def test_enroll_resolves_live_uids_and_renders_without_writes(self):
        status, output, _ = self.run_cli("enroll", "--target-context", "target", "--target-node", "target")
        self.assertEqual(status, 0)
        document = json.loads(output)["items"][0]
        self.assertEqual(document["metadata"]["annotations"],
                         {demo.GROUP + "periodic-checkpoint": "disabled"})
        spec = document["spec"]
        self.assertEqual(spec["workloadRef"]["uid"], "world-uid")
        self.assertEqual(spec["capacity"], {"mode": "Existing", "aws": {"karmadaCluster": "aws"},
            "existingNodes": [{"rank": 0, "nodeName": "target", "nodeUID": "target-node-uid"}]})
        self.assertTrue(spec["checkpoint"]["resume"])
        self.assertEqual(spec["checkpoint"]["minIntervalSeconds"], 600)
        self.assertEqual(spec["checkpoint"]["maxIntervalSeconds"], 600)
        self.assertNotIn("riskProfileRef", spec)
        self.assertFalse(self.created)

    def test_separate_kubeconfig_flags_without_contexts(self):
        status, _, err = self.run_cli("enroll", "--kubeconfig", "karmada.conf",
            "--target-kubeconfig", "aws.conf", "--target-node", "target", "--apply",
            default_context=False)
        self.assertEqual(status, 0, err)
        status, _, err = self.run_cli("checkpoint", "--kubeconfig", "karmada.conf",
            "--name", "manual", "--apply", default_context=False)
        self.assertEqual(status, 0, err)
        status, _, err = self.run_cli("trigger", "--kubeconfig", "karmada.conf",
            "--source-kubeconfig", "onpre1.conf", "--confirm-workload",
            demo.NS + "/" + demo.APP, default_context=False)
        self.assertEqual(status, 0, err)
        for call in self.calls:
            self.assertNotIn("--context", call)
            config = call[call.index("--kubeconfig") + 1]
            resource = call[call.index("--request-timeout=30s") + 2]
            if resource == "nodes":
                expected = "aws.conf" if "target" in call else "onpre1.conf"
            elif resource == "pods":
                expected = "onpre1.conf"
            else:
                expected = "karmada.conf"
            self.assertEqual(config, expected)

    def test_kubeconfig_environment_defaults(self):
        env = {"KARMADA_KUBECONFIG": "karmada.conf", "ONPRE1_KUBECONFIG": "onpre1.conf",
               "AWS_KUBECONFIG": "aws.conf"}
        commands = [
            ("enroll", "--target-node", "target"),
            ("checkpoint", "--name", "manual"),
            ("trigger", "--confirm-workload", demo.NS + "/" + demo.APP),
        ]
        for command in commands:
            status, _, err = self.run_cli(*command, default_context=False, environ=env)
            self.assertEqual(status, 0, err)
        self.assertTrue(all("--context" not in call for call in self.calls))
        self.assertEqual({call[call.index("--kubeconfig") + 1] for call in self.calls},
                         set(env.values()))

    def test_flags_override_environment_and_context_is_explicit_only(self):
        env = {key: "unused.conf" for key in
               ("KARMADA_KUBECONFIG", "ONPRE1_KUBECONFIG", "AWS_KUBECONFIG")}
        status, _, err = self.run_cli("enroll", "--kubeconfig", "karmada.conf",
            "--target-kubeconfig", "aws.conf", "--target-context", "target",
            "--target-node", "target", environ=env)
        self.assertEqual(status, 0, err)
        status, _, err = self.run_cli("trigger", "--kubeconfig", "karmada.conf",
            "--source-kubeconfig", "onpre1.conf", "--source-context", "source",
            "--confirm-workload", demo.NS + "/" + demo.APP, environ=env)
        self.assertEqual(status, 0, err)
        for call in self.calls:
            self.assertNotIn("unused.conf", call)
            self.assertIn("--context", call)

    def test_initial_discovery_does_not_require_placement(self):
        self.policy["status"] = {"discovery": {
            "ready": True, "phase": "Stable", "observedGeneration": 1,
            "workloadUID": "world-uid", "runtimeRef": {"name": "actual-runtime"},
            "sourceCluster": "onpre1", "targetCluster": "onpre1",
            "bindingName": "actual-binding", "bindingUID": "binding-uid"}}
        self.assertEqual(self.run_cli("checkpoint", "--name", "initial")[0], 0)
        self.assertEqual(self.trigger()[0], 0)

    def test_missing_connection_refuses_before_any_kubectl_access(self):
        cases = [
            (("checkpoint", "--name", "cp"), "Karmada"),
            (("enroll", "--target-kubeconfig", "aws.conf", "--target-node", "target"), "Karmada"),
            (("enroll", "--kubeconfig", "karmada.conf", "--target-node", "target"), "target"),
            (("trigger", "--kubeconfig", "karmada.conf", "--confirm-workload",
              demo.NS + "/" + demo.APP), "source"),
        ]
        for flags, role in cases:
            with self.subTest(role=role, command=flags[0]):
                status, _, err = self.run_cli(*flags, default_context=False,
                                              environ={"KUBECONFIG": "ambient.conf"})
                self.assertEqual(status, 1)
                self.assertIn(role + " requires", err)
                self.assertEqual(self.calls, [])

    def test_explicit_contexts_work_without_kubeconfig(self):
        self.assertEqual(self.run_cli("enroll", "--target-context", "target",
                                     "--target-node", "target")[0], 0)
        self.assertEqual(self.run_cli("checkpoint", "--name", "cp")[0], 0)
        self.assertEqual(self.trigger()[0], 0)
        for call in self.calls:
            self.assertIn("--context", call)
            self.assertNotIn("--kubeconfig", call)

    def test_kubectl_rejects_ambient_only_direct_usage(self):
        with patch.object(demo.subprocess, "run") as mocked:
            with self.assertRaisesRegex(demo.DemoError, "explicit kubeconfig or context"):
                demo.Kubectl()
            mocked.assert_not_called()

    def test_verified_placement_selects_current_runtime(self):
        self.policy["status"] = {"placement": {
            "verified": True, "policyUID": "policy-uid", "workloadUID": "world-uid",
            "initialSourceCluster": "onpre1", "initialRuntimeName": "actual-runtime",
            "activeCluster": "aws", "runtimeRef": {"name": "target-runtime"}}}
        target = copy.deepcopy(self.runtime)
        target["metadata"]["name"] = "target-runtime"
        target["spec"]["sourceCluster"] = "aws"
        target["status"]["clusters"][0]["clusterName"] = "aws"
        self.responses[("karmada", demo.RUNTIME, None)]["items"].append(target)
        status, output, err = self.run_cli("checkpoint", "--name", "after-move")
        self.assertEqual(status, 0, err)
        self.assertEqual(json.loads(output)["items"][0]["spec"]["placement"]
                         ["clusterAffinity"]["clusterNames"], ["aws"])
        self.assertEqual(self.trigger()[0], 1)
        self.policy["status"]["placement"]["initialRuntimeName"] = "stale"
        status, output, err = self.run_cli("checkpoint", "--name", "initial")
        self.assertEqual(status, 0, err)
        self.assertEqual(json.loads(output)["items"][0]["spec"]["placement"]
                         ["clusterAffinity"]["clusterNames"], ["onpre1"])

    def test_enroll_apply_and_running_workload_refusal(self):
        self.assertEqual(self.run_cli("enroll", "--target-context", "target", "--target-node", "target",
                                     "--apply")[0], 0)
        self.assertEqual(len(self.created), 1)
        self.sts["spec"]["replicas"] = 1
        self.assertEqual(self.run_cli("enroll", "--target-context", "target", "--target-node", "target")[0], 1)

    def test_checkpoint_owned_placement_and_resume(self):
        status, output, _ = self.run_cli("checkpoint", "--name", "manual-1")
        self.assertEqual(status, 0)
        pp, cp = json.loads(output)["items"]
        self.assertEqual(pp["metadata"]["name"], "manual-1-placement")
        self.assertEqual(pp["spec"]["placement"]["clusterAffinity"]["clusterNames"], ["onpre1"])
        for item in (pp, cp):
            self.assertEqual(item["metadata"]["labels"][demo.GROUP + "policy-uid"], "policy-uid")
        self.assertTrue(cp["spec"]["resume"])
        self.assertEqual(cp["spec"]["workloadRef"]["uid"], "world-uid")
        self.assertEqual(self.run_cli("checkpoint", "--name", "manual-2", "--apply")[0], 0)
        self.assertEqual([x["kind"] for x in self.created], ["PropagationPolicy", "FluidCRMigration"])

    def test_trigger_uses_binding_uid_and_resource_version(self):
        status, _, err = self.trigger()
        self.assertEqual(status, 0, err)
        self.assertIn(demo.GROUP + "application-failover-trigger=binding-uid", self.calls[-1])
        self.assertIn("--resource-version=7", self.calls[-1])
        self.assertEqual(sum("annotate" in c for c in self.calls), 1)

    def test_confirmation_refuses_before_any_access(self):
        self.assertEqual(self.run_cli("trigger", "--source-context", "source",
                                     "--confirm-workload", demo.APP)[0], 1)
        self.assertEqual(self.calls, [])

    def test_trigger_refuses_missing_checkpoint_or_uncordoned_source(self):
        self.responses[("karmada", demo.CHECKPOINT, None)] = {"items": []}
        self.assertEqual(self.trigger()[0], 1)
        self.responses[("karmada", demo.CHECKPOINT, None)] = {"items": [self.cp]}
        self.source_node["spec"]["unschedulable"] = False
        self.assertEqual(self.trigger()[0], 1)
        self.assertFalse(any("annotate" in c for c in self.calls))

    def test_trigger_rejects_recreated_pod_and_binding(self):
        self.responses[("source", "pods", demo.APP + "-0")]["metadata"]["uid"] = "recreated"
        self.assertEqual(self.trigger()[0], 1)
        self.responses[("source", "pods", demo.APP + "-0")]["metadata"]["uid"] = "pod-uid"
        self.binding["spec"]["resource"]["uid"] = "other-world"
        self.assertEqual(self.trigger()[0], 1)
        self.assertFalse(any("annotate" in c for c in self.calls))

    def test_stale_runtime_and_policy_are_rejected(self):
        self.runtime["status"]["clusters"][0]["status"]["observedGeneration"] = 0
        self.assertEqual(self.run_cli("checkpoint", "--name", "cp")[0], 1)
        self.runtime["status"]["clusters"][0]["status"]["observedGeneration"] = 1
        self.policy["spec"]["workloadRef"] = dict(self.ref, uid="stale")
        self.assertEqual(self.trigger()[0], 1)

    def test_checkpoint_requires_durable_full_current_round(self):
        self.assertTrue(demo.ready_checkpoint(self.cp, self.policy, "onpre1", demo.NS))
        variants = []
        cp = copy.deepcopy(self.cp)
        cp["spec"]["partialCheckpoint"] = {"enabled": True}
        variants.append(cp)
        cp = copy.deepcopy(self.cp)
        cp["status"]["clusters"][0]["observedGeneration"] = 0
        variants.append(cp)
        for field, value in (("sha256", "invalid"), ("durableRef", "other"),
                             ("exportedAt", "bad"), ("checkpointID", "other-round")):
            cp = copy.deepcopy(self.cp)
            cp["status"]["clusters"][0]["pods"][0]["checkpointFiles"][0][field] = value
            variants.append(cp)
        for cp in variants:
            self.assertFalse(demo.ready_checkpoint(cp, self.policy, "onpre1", demo.NS))

    def test_kubectl_diagnostics_do_not_leak_credentials(self):
        with patch.object(demo.subprocess, "run", return_value=subprocess.CompletedProcess(
                [], 1, "SECRET", "SECRET")), self.assertRaises(demo.DemoError) as caught:
            demo.Kubectl("context").get("nodes")
        self.assertNotIn("SECRET", str(caught.exception))

    def test_help_is_offline(self):
        with patch.object(demo.subprocess, "run") as mocked:
            for args in (["--help"], ["enroll", "--help"], ["checkpoint", "--help"], ["trigger", "--help"]):
                with contextlib.redirect_stdout(io.StringIO()), self.assertRaises(SystemExit) as caught:
                    demo.main(args)
                self.assertEqual(caught.exception.code, 0)
            mocked.assert_not_called()


class PackageTests(unittest.TestCase):
    def test_failover_is_an_explicit_never_300_merge_patch(self):
        path = Path(__file__).resolve().parents[1] / (
            "config/demo/application-failover/application-failover.patch.yaml")
        patch_text = path.read_text(encoding="utf-8")
        self.assertEqual(patch_text, (
            "# Merge patch, NOT an apply manifest. Arm only after restore orchestration is ready.\n"
            "spec:\n"
            "  propagateDeps: true\n"
            "  failover:\n"
            "    application:\n"
            "      decisionConditions:\n"
            "        tolerationSeconds: 300\n"
            "      purgeMode: Never\n"))

    def test_packaged_contract_and_script_digest(self):
        package = Path(__file__).resolve().parents[1] / "config/demo/application-failover"
        workload = (package / "05-karmada-workload.yaml").read_text(encoding="utf-8")
        self.assertIn("replicas: 0", workload)
        self.assertIn("FLUIDCR_CHECKPOINT_DIR, value: /checkpoint", workload)
        self.assertIn('WORLD_SIZE, value: "1"', workload)
        self.assertIn("EVAL_MODEL, value: resnet50", workload)
        self.assertIn("volumeClaimTemplates:", workload)
        self.assertNotIn("README.md", {p.name for p in package.iterdir()})
        storage = (package / "02-onpre1-storage.yaml").read_text(encoding="utf-8")
        self.assertIn("/mnt/nfs/datasets/cifar10", storage)
        self.assertIn("/mnt/nfs/gpu-fluidcr-demo/r50-fluidcr-demo", storage)
        cm = json.loads((package / "03-karmada-script.yaml").read_text(encoding="utf-8"))
        script = cm["data"]["train_resnet18.py"]
        self.assertEqual(hashlib.sha256(script.encode()).hexdigest(),
                         cm["metadata"]["annotations"]["demo.training-code-sha256"])
        compile(script, "train_resnet18.py", "exec")


if __name__ == "__main__":
    unittest.main()
