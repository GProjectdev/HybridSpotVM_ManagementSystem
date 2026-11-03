import copy
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

from e1_experiment import Experiment, durable_world, events_from, package, source_ready, evidence_resource, restored_progress
from e1_render import render_e1
from run import read, save


class E1ExperimentTests(unittest.TestCase):
    def test_constrained_network_defaults(self):
        from e1_experiment import build_parser
        args = build_parser().parse_args(["execute", "--evidence", "test", "--confirm", "test"])
        self.assertEqual((args.warmup_steps, args.source_timeout, args.move_timeout), (10, 2400, 3600))
        self.assertEqual((args.resume_steps, args.checkpoint_timeout), (50, 1200))

    def checkpoint(self):
        return {"metadata": {"uid": "cp-uid", "generation": 1}, "status": {
            "phase": "Completed", "observedGeneration": 1, "completionTime": "2026-11-03T00:00:00Z", "pods": [
            {"podUID": uid, "podName": uid, "nodeName": uid, "phase": "Resumed", "checkpointID": "round", "checkpointFiles": [
                {"containerName": "trainer", "checkpointID": "round",
                 "durableRef": "file-store:fluidcr-realign-121040/sha256/" + "a" * 64,
                 "filePath": "/archive.tar", "exportedAt": "2026-11-03T00:00:00Z", "sha256": "a" * 64}]} for uid in ("u0", "u1")]}}

    def test_full_export_not_just_completed(self):
        cp = self.checkpoint()
        self.assertTrue(durable_world(cp, {"u0", "u1"}, "round"))
        for mutation in ("export", "uid", "round", "missing", "phase", "generation", "pod-phase", "digest", "export-time"):
            obj = copy.deepcopy(cp)
            if mutation == "export":
                del obj["status"]["pods"][1]["checkpointFiles"][0]["durableRef"]
            elif mutation == "uid":
                obj["status"]["pods"][1]["podUID"] = "stale"
            elif mutation == "round":
                obj["status"]["pods"][1]["checkpointID"] = "different"
            elif mutation == "phase":
                obj["status"]["phase"] = "Failed"
            elif mutation == "generation":
                obj["status"]["observedGeneration"] = 0
            elif mutation == "pod-phase":
                obj["status"]["pods"][1]["phase"] = "Failed"
            elif mutation == "digest":
                obj["status"]["pods"][1]["checkpointFiles"][0]["sha256"] = "z" * 64
            elif mutation == "export-time":
                obj["status"]["pods"][1]["checkpointFiles"][0].pop("exportedAt")
            else:
                obj["status"]["pods"].pop()
            self.assertFalse(durable_world(obj, {"u0", "u1"}, "round"), mutation)

    def test_event_scope(self):
        e = {"run_id": "e1-test", "world_size": 2, "rank": 1, "global_step": 100,
             "event": "step", "timestamp": "2026-11-03T00:00:00Z"}
        text = "bad\nEVAL_EVENT invalid\nEVAL_EVENT []\nEVAL_EVENT " + json.dumps(e)
        self.assertEqual(list(events_from(text, "e1-test")), [e])
        self.assertEqual(list(events_from(text, "other")), [])

    def test_restore_audit_cannot_mix_attempts_or_skip_updates(self):
        events = [{"event": "restored", "global_step": 100, "attempt_id": "a"},
                  {"event": "restore_audit", "global_step": 100, "attempt_id": "a", "passed": True,
                   "checks": dict(model=True, optimizer=True, rng=True, step=True)},
                  {"event": "step", "global_step": 150, "attempt_id": "a"}]
        events += [{"event": "update_audit", "global_step": step, "attempt_id": "a", "passed": True,
                    "gradients_finite": True, "gradients_nonzero": True, "parameters_changed": True} for step in (101, 102, 103)]
        self.assertEqual(restored_progress(events, 50), 100)
        mixed = copy.deepcopy(events)
        mixed[1]["attempt_id"] = "b"
        self.assertIsNone(restored_progress(mixed, 50))
        self.assertIsNone(restored_progress(events[:-1], 50))
        mixed = copy.deepcopy(events)
        mixed[1]["checks"]["model"] = False
        self.assertIsNone(restored_progress(mixed, 50))

    def test_metadata_requires_all_claims_and_live_runtime_uids(self):
        md = [{"spec": {"sourceCluster": "onpre1", "workloadRef": {"uid": "w"}},
               "status": {"ready": True, "volumes": [{"pvcName": n, "pvcUID": n + "uid", "pvcSpec": {"accessModes": ["RWX"]},
                   "pvSpec": {"nfs": {"server": "10.50.1.31"}}} for n in ("data", "model", "checkpoint")]}}]
        rt = [{"status": {"clusters": [{"clusterName": "onpre1", "status": {
            "workloadUID": "w", "worldSize": 2, "readyRanks": 2, "pods": [{"uid": "u0"}, {"uid": "u1"}]}}]}}]
        claims = ["data", "model", "checkpoint"]
        self.assertTrue(source_ready(md, rt, "w", {"u0", "u1"}, claims))
        self.assertFalse(source_ready(md, rt, "w", {"u0", "new"}, claims))
        self.assertFalse(source_ready(md, rt, "other", {"u0", "u1"}, claims))
        md[0]["status"]["volumes"].pop()
        self.assertFalse(source_ready(md, rt, "w", {"u0", "u1"}, claims))

    def fixture(self, directory):
        config = read(Path(__file__).parent / "config.gpt2.ck10k-risk60-fis80.json")
        config["storage_class"] = "e1-aws-nfs"
        source = {"spec": {"capacity": {"aws": {"ami": "ami-test", "subnetId": "subnet-test", "vpcId": "vpc-test",
            "securityGroupIds": ["sg-test"], "credentialsRef": {"name": "aws"}}}}}
        run, storage, resources, policy = render_e1(config, source, "e1-test")
        for name, value in (("run.json", run), ("onpre-storage.json", {"items": storage}),
                            ("karmada.json", {"items": resources}), ("policy.unbound.json", policy)):
            save(Path(directory) / name, value)
        return Experiment(directory), policy

    def test_gpt2_render_and_source_only_storage(self):
        with tempfile.TemporaryDirectory() as d:
            exp, policy = self.fixture(d)
            storage = read(Path(d) / "onpre-storage.json")["items"]
            self.assertEqual(sum(o["kind"] == "PersistentVolumeClaim" for o in storage), 3)
            self.assertEqual(exp.run["model"], "gpt2")
            self.assertEqual(policy["spec"]["policy"]["fixedOnDemand"], 0)
            self.assertEqual(policy["spec"]["policy"]["alpha"], 0.05)
            self.assertEqual(policy["spec"]["policy"]["minOnDemand"], 0)
            self.assertEqual(policy["metadata"]["annotations"]["training.dcnlab.com/suspend"], "true")

    def test_no_move_when_export_gate_times_out_and_no_rerun(self):
        with tempfile.TemporaryDirectory() as d:
            exp, policy = self.fixture(d)
            policy["metadata"]["uid"] = "policy-uid"
            pods = [{"metadata": {"uid": "u0"}}, {"metadata": {"uid": "u1"}}]
            def get(cluster, kind, name=None, **kwargs):
                if kind == "statefulset":
                    return {"metadata": {"uid": "workload"}}
                if kind == "trainingpolicy":
                    return policy
                return {"items": []}
            args = SimpleNamespace(warmup_steps=100, source_timeout=1, checkpoint_timeout=1, move_timeout=1, resume_steps=50)
            with patch.object(exp, "preflight"), patch.object(exp, "snapshot"), patch.object(exp, "controllers"), \
                    patch.object(exp, "k"), patch.object(exp, "create"), patch.object(exp, "get", side_effect=get), \
                    patch.object(exp, "patch") as mutate, \
                    patch.object(exp, "wait", side_effect=[True, pods, True, TimeoutError("export missing")]):
                with self.assertRaises(TimeoutError):
                    exp.execute(args)
                self.assertFalse(any(call.args[0] == "propagationpolicy" for call in mutate.call_args_list))
                with self.assertRaises(FileExistsError):
                    exp.execute(args)

    def test_move_releases_policy_only_after_pending_intent(self):
        for fail_pending in (False, True):
            with self.subTest(fail_pending=fail_pending), tempfile.TemporaryDirectory() as d:
                exp, policy = self.fixture(d)
                policy["metadata"]["uid"] = "policy-uid"
                pods = [{"metadata": {"uid": "u0"}}, {"metadata": {"uid": "u1"}}]
                def get(cluster, kind, name=None, **kwargs):
                    if kind == "statefulset":
                        return {"metadata": {"uid": "workload"}}
                    if kind == "trainingpolicy":
                        return policy
                    return {"items": []}
                args = SimpleNamespace(warmup_steps=100, source_timeout=1, checkpoint_timeout=1, move_timeout=1, resume_steps=50)
                sequence = []
                def wait(label, predicate, timeout):
                    sequence.append(label)
                    if label == "source_progress":
                        return pods
                    if label == "pending_move" and fail_pending:
                        raise TimeoutError("no pending placement")
                    return {}
                def mutate(kind, name, value):
                    if kind == "trainingpolicy":
                        self.assertEqual(value["metadata"]["annotations"]["training.dcnlab.com/suspend"], "false")
                        sequence.append("unsuspend")
                with patch.object(exp, "preflight"), patch.object(exp, "snapshot"), patch.object(exp, "controllers"), \
                        patch.object(exp, "k"), patch.object(exp, "create"), patch.object(exp, "get", side_effect=get), \
                        patch.object(exp, "patch", side_effect=mutate), patch.object(exp, "wait", side_effect=wait):
                    if fail_pending:
                        with self.assertRaises(TimeoutError):
                            exp.execute(args)
                        self.assertNotIn("unsuspend", sequence)
                    else:
                        exp.execute(args)
                        self.assertLess(sequence.index("pending_move"), sequence.index("unsuspend"))

    def test_snapshot_skips_karmada_only_kinds_on_members(self):
        with tempfile.TemporaryDirectory() as d:
            exp, _ = self.fixture(d)
            with patch.object(exp, "get", return_value={"items": []}) as get:
                exp.snapshot("test")
            for call in get.call_args_list:
                if call.args[0] != "karmada":
                    self.assertNotIn(call.args[1], ("resourcebindings", "propagationpolicies"))

    def test_package_contains_analysis_and_hashes(self):
        with tempfile.TemporaryDirectory() as d:
            out = Path(d) / "run"
            out.mkdir()
            save(out / "run.json", {"run_id": "test"})
            (out / "private").mkdir()
            (out / "private" / "raw.log").write_text("private diagnostic", encoding="utf-8")
            archive = package(out)
            with tarfile.open(archive) as tar:
                names = tar.getnames()
                self.assertIn("run/sha256.json", names)
                self.assertIn("run/progress.csv", names)
                self.assertIn("run/timeline.csv", names)
                self.assertEqual(len(names), len(set(names)))
                self.assertFalse(any("private" in name for name in names))
            self.assertEqual(read(out / "analysis-notes.json")["result"], "requires_review")

    def test_projection_excludes_credentials_and_keeps_volume_identity(self):
        obj = {"spec": {"containers": [{"image": "image:digest", "env": [{"name": "PASSWORD", "value": "secret"}],
                                        "args": ["--token=secret"]}],
                        "nfs": {"server": "10.50.1.31", "path": "/mnt/nfs"}, "claimRef": {"uid": "pvc-uid"}},
               "metadata": {"annotations": {"kubectl.kubernetes.io/last-applied-configuration": "secret"}}}
        result = evidence_resource(obj)
        self.assertNotIn("secret", json.dumps(result))
        self.assertEqual(result["spec"]["claimRef"]["uid"], "pvc-uid")


if __name__ == "__main__":
    unittest.main()
