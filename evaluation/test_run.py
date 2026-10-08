import copy
import importlib.util
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

HERE=Path(__file__).resolve().parent
spec=importlib.util.spec_from_file_location("eval_run",HERE/"run.py")
run=importlib.util.module_from_spec(spec);spec.loader.exec_module(run)

class SaveTests(unittest.TestCase):
    def test_reader_sees_complete_old_json_until_atomic_publication(self):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / "run.json"
            before = {"phase": "prepared", "run_id": "r18a01"}
            after = {"phase": "running", "run_id": "r18a01", "payload": "x" * 100000}
            run.save(path, before)
            replace = run.os.replace

            def publish(source, destination):
                self.assertEqual(Path(source).parent, path.parent)
                self.assertNotEqual(Path(source), path)
                self.assertEqual(run.read(path), before)
                self.assertEqual(run.read(source), after)
                replace(source, destination)
                self.assertEqual(run.read(path), after)

            with patch.object(run.os, "replace", side_effect=publish) as publication:
                run.save(path, after)
            publication.assert_called_once()
            self.assertEqual(list(Path(d).iterdir()), [path])

    def test_failed_publication_preserves_old_json_and_removes_temporary(self):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / "run.json"
            before = {"phase": "running"}
            run.save(path, before)
            with patch.object(run.os, "replace", side_effect=OSError("publication failed")):
                with self.assertRaisesRegex(OSError, "publication failed"):
                    run.save(path, {"phase": "completed"})
            self.assertEqual(run.read(path), before)
            self.assertEqual(list(Path(d).iterdir()), [path])

class RenderTests(unittest.TestCase):
    def setUp(self):
        self.c=json.loads((HERE/"config.example.json").read_text())
        self.c.update(nfs_server="10.0.0.5",nfs_dataset_path="/exports/datasets/cifar10")
        self.source={"spec":{"capacity":{"aws":{"ami":"ami-abc","subnetId":"subnet-abc","vpcId":"vpc-abc","securityGroupIds":["sg-abc"],"credentialsRef":{"name":"aws"}}},
                             "policy":{"alpha":0.8,"minOnDemand":1,"fixedOnDemand":2},
                             "checkpoint":{"candidateIntervalSeconds":[300]}}}
    def render(self,exp="cost",arm="A"):
        with tempfile.TemporaryDirectory() as d:
            p=Path(d);(p/"resnet18").mkdir();(p/"resnet18"/"train_resnet18.py").write_text("pass")
            with patch.object(run,"HERE",p):
                return run.render(self.c,self.source,"r18a01",exp,arm)
    def test_cost_and_fixed_policy(self):
        for exp,arm,expected in [("cost","A",2),("cost","B",0)]+[("checkpoint",arm,1) for arm in ("F60","F300","F600","D")]:
            info,member,items,policy=self.render(exp,arm)
            self.assertEqual(policy["spec"]["policy"]["minOnDemand"],expected)
            self.assertEqual(policy["spec"]["policy"].get("fixedOnDemand"),1 if exp=="checkpoint" else None)
            self.assertNotIn("candidateIntervalSeconds",policy["spec"]["checkpoint"])
            self.assertEqual(policy["metadata"]["annotations"]["training.dcnlab.com/suspend"],"true")
            self.assertEqual(info["namespace"],run.NS)
            self.assertEqual(info["dataset"],"cifar10")
            self.assertEqual(policy["metadata"]["annotations"].get("training.dcnlab.com/planned-partial"),
                             "disabled" if exp=="checkpoint" else None)
    def test_readonly_data_fresh_checkpoint_uid_before_start(self):
        info,member,items,policy=self.render()
        self.assertTrue(member[0]["spec"]["nfs"]["readOnly"])
        self.assertEqual(member[0]["spec"]["persistentVolumeReclaimPolicy"],"Retain")
        self.assertNotEqual(member[1]["metadata"]["name"],member[2]["metadata"]["name"])
        sts=next(x for x in items if x["kind"]=="StatefulSet")
        self.assertEqual(sts["spec"]["replicas"],0)
        self.assertEqual(policy["spec"]["workloadRef"]["uid"],"BOUND_DURING_PREPARE")
        self.assertEqual(self.source["spec"]["policy"]["fixedOnDemand"],2)
    def test_invalid_input(self):
        for key,value in [("namespace","default"),("goal_steps",0),("initial_risk",float("nan")),("nfs_server","REPLACE_ME")]:
            c=copy.deepcopy(self.c);c[key]=value
            with self.assertRaises(ValueError):run.validate(c,"r18a","cost","A")
        with self.assertRaises(ValueError):run.validate(self.c,"../bad","cost","A")
        with self.assertRaises(ValueError):run.validate(self.c,"r18a","checkpoint","A")
    def test_event_filter(self):
        text='2026-01-01 EVAL_EVENT {"run_id":"r18a","event":"step","rank":0}\nEVAL_EVENT broken\nEVAL_EVENT {"run_id":"other"}'
        self.assertEqual(len(list(run.training_events(text,"r18a"))),1)

    def test_no_stale_measurements_copied(self):
        self.source["spec"]["checkpoint"].update(measuredCosts={"checkpointSeconds":100},paperProfile={"enabled":True})
        _,_,_,policy=self.render("checkpoint","D")
        self.assertNotIn("measuredCosts",policy["spec"]["checkpoint"])
        self.assertNotIn("paperProfile",policy["spec"]["checkpoint"])

    def test_completed_risk_change_requires_recovery(self):
        with tempfile.TemporaryDirectory() as d:
            info={"scenario":"risk-rise","experiment":"cost","arm":"B","policy_uid":"uid","changed_risk_consumed":True}
            with patch.object(run,"get",return_value={"items":[]}):
                with self.assertRaisesRegex(RuntimeError,"recovery/cleanup"):
                    run.verify_scenario(Path(d),info)
            with patch.object(run,"get",return_value={"items":[{"status":{"phase":"Completed"}}]}):
                run.verify_scenario(Path(d),info)

    def test_unconfirmed_interruption_is_not_success(self):
        with tempfile.TemporaryDirectory() as d:
            info={"scenario":"interruption"}
            with self.assertRaisesRegex(RuntimeError,"no recorded FIS"):
                run.verify_scenario(Path(d),info)

    def test_failed_replacement_is_still_active(self):
        failed={"metadata":{"name":"replacement"},"status":{"phase":"Failed"}}
        with patch.object(run,"get",side_effect=[{"items":[failed]},{"items":[]},{"items":[]}]):
            self.assertEqual(run.active_operations({"policy_uid":"uid"}),["spotreplacements/replacement"])

    def test_dynamic_bootstrap_and_unconsumed_risk_are_not_valid(self):
        with self.assertRaisesRegex(RuntimeError,"bootstrap-only"):
            run.verify_scenario(Path("."),{"arm":"D","scenario":"constant"})
        with self.assertRaisesRegex(RuntimeError,"not consumed"):
            run.verify_scenario(Path("."),{"arm":"F300","scenario":"risk-rise"})

    def test_cleanup_is_scoped_and_retryable_after_policy_deletion(self):
        with tempfile.TemporaryDirectory() as d:
            evidence=Path(d)
            run.save(evidence/"run.json",{"run_id":"test","policy":"eval-test","app":"eval-test"})
            run.save(evidence/"created.json",[{"cluster":"karmada","kind":"TrainingPolicy","name":"eval-test","uid":"old-uid","namespaced":True}])
            with patch.object(run,"kubectl",return_value="") as command, patch.object(run,"get",return_value={"items":[]}), patch.object(run,"patch_policy") as suspend:
                run.cleanup(evidence,"test")
                suspend.assert_not_called()
                self.assertFalse(any("delete" in call.args for call in command.call_args_list))
            with self.assertRaises(ValueError):run.cleanup(evidence,"wrong")


class GroupRecoveryTests(unittest.TestCase):
    def setUp(self):
        self.info = {"scenario": "interruption", "experiment": "checkpoint", "arm": "F300",
                     "policy_uid": "policy-uid", "region": "ap-northeast-2"}
        self.request = {"metadata": {"name": "op-group-restore", "labels": {
            run.POLICY_UID: "policy-uid", "training.dcnlab.com/role": "group-restore"},
            "annotations": {"training.dcnlab.com/group-old-nodeprovision": "old-node",
                            "training.dcnlab.com/group-old-nodeprovision-uid": "karmada-old"}},
            "spec": {"groupRestore": {"sourcePods": [{"nodeProvisionRef": {
                "name": "old-node", "uid": "aws-old"}}]}}, "status": {"phase": "Verified"}}

    def verify(self, evidence, requests, nodes):
        run.save(evidence/"fis-started.json", {"experiment": {"id": "exp-1"}})
        run.save(evidence/"fis-target.json", {"instance_id": "i-old"})
        def aws(region, service, *args):
            return ({"experiment": {"state": {"status": "completed"}}} if service == "fis"
                    else {"Reservations": [{"Instances": [{"State": {"Name": "terminated"}}]}]})
        def get(cluster, kind, name=None, selector=None):
            if kind == "restorerequests":
                self.assertEqual(cluster, "karmada")
                self.assertEqual(selector, run.POLICY_UID+"=policy-uid,training.dcnlab.com/role=group-restore")
                return {"items": requests}
            self.assertEqual(kind, "nodeprovisions")
            self.assertIsNone(selector)
            return {"items": nodes[cluster]}
        with patch.dict(run.sys.modules, {"fis": SimpleNamespace(aws=aws)}), patch.object(run, "get", side_effect=get), patch.object(run, "kubectl") as command:
            try:
                run.verify_scenario(evidence, self.info)
            finally:
                command.assert_not_called()

    def test_group_success_old_name_absent_with_optional_new_name_replacement(self):
        for replaced in (False, True):
            with self.subTest(replaced=replaced), tempfile.TemporaryDirectory() as d:
                evidence = Path(d)
                nodes = {cluster: ([{"metadata": {"name": "replacement-node", "uid": cluster+"-new"}}]
                                   if replaced else []) for cluster in ("karmada", "aws")}
                self.verify(evidence, [self.request], nodes)
                self.assertEqual(run.read(evidence/"recovery-final.json"), [self.request])
                self.assertEqual(run.read(evidence/"group-nodeprovisions-final.json"), nodes)

    def test_group_still_old_refused_in_either_cluster_even_when_terminating(self):
        for cluster, uid in (("karmada", "karmada-old"), ("aws", "aws-old"),
                             ("karmada", "different-uid"), ("aws", "different-uid")):
            with self.subTest(cluster=cluster), tempfile.TemporaryDirectory() as d:
                nodes = {"karmada": [], "aws": []}
                nodes[cluster] = [{"metadata": {"name": "old-node", "uid": uid,
                                                "deletionTimestamp": "2026-10-08T00:00:00Z"}}]
                with self.assertRaisesRegex(RuntimeError, "old NodeProvision remains in "+cluster):
                    self.verify(Path(d), [self.request], nodes)
                self.assertEqual(run.read(Path(d)/"group-nodeprovisions-final.json"), nodes)

    def test_group_requires_nonempty_all_verified_owned_requests(self):
        for invalid in ("empty", "unverified", "foreign", "wrong-role"):
            with self.subTest(invalid=invalid), tempfile.TemporaryDirectory() as d:
                request = copy.deepcopy(self.request)
                if invalid == "unverified":
                    request["status"]["phase"] = "Restoring"
                elif invalid == "foreign":
                    request["metadata"]["labels"][run.POLICY_UID] = "other-policy"
                elif invalid == "wrong-role":
                    request["metadata"]["labels"]["training.dcnlab.com/role"] = "partial"
                requests = [] if invalid == "empty" else [self.request, request]
                with self.assertRaisesRegex(RuntimeError, "all Verified"):
                    self.verify(Path(d), requests, {"karmada": [], "aws": []})

    def test_group_missing_old_identity_fails_closed(self):
        request = copy.deepcopy(self.request)
        request["metadata"]["annotations"].pop("training.dcnlab.com/group-old-nodeprovision-uid")
        with tempfile.TemporaryDirectory() as d:
            with self.assertRaisesRegex(RuntimeError, "old NodeProvision identities"):
                self.verify(Path(d), [request], {"karmada": [], "aws": []})


if __name__=="__main__":unittest.main()
