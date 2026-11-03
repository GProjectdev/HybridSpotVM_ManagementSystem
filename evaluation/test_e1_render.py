import unittest
from e1_render import render_e1


class E1RenderTest(unittest.TestCase):
    def test_source_storage_and_cold_target(self):
        config = dict(namespace="fluidcr-realign-121040", model="resnet50", nfs_server="10.0.0.1",
                      nfs_dataset_path="/datasets/cifar10", training_image="test:fixed", storage_class="legacy-nfs-client",
                      goal_steps=20000, batch_size=32, max_seconds=7200, initial_risk=0.05, changed_risk=0.8,
                      region="ap-northeast-2", availability_zone="ap-northeast-2c", instance_type="g5.xlarge", seed=1)
        source = {"spec": {"capacity": {"aws": dict(ami="ami-test", subnetId="subnet-test", vpcId="vpc-test",
                                                    securityGroupIds=["sg-test"], credentialsRef={"name": "aws"})}}}
        run, storage, resources, policy = render_e1(config, source, "e1-test")
        self.assertEqual(run["experiment"], "e1")
        self.assertEqual(policy["spec"]["sourceCluster"], "onpre1")
        self.assertEqual(policy["spec"]["capacity"]["aws"]["karmadaCluster"], "aws")
        self.assertEqual(policy["spec"]["capacity"]["mode"], "Provisioned")
        self.assertEqual(policy["metadata"]["annotations"]["training.dcnlab.com/suspend"], "true")
        self.assertFalse(policy["spec"]["replacement"]["enabled"])
        self.assertEqual({r["kind"] for r in storage}, {"PersistentVolume", "PersistentVolumeClaim"})
        self.assertFalse(any(r["kind"] in ("PersistentVolume", "PersistentVolumeClaim", "NodeProvision") for r in resources))
        sts = next(r for r in resources if r["kind"] == "StatefulSet")
        self.assertEqual(sts["spec"]["replicas"], 0)
        placements = [r for r in resources if r["kind"] == "PropagationPolicy"]
        for placement in placements:
            selectors = placement["spec"]["resourceSelectors"]
            clusters = placement["spec"]["placement"]["clusterAffinity"]["clusterNames"]
            if any(s["kind"] == "StatefulSet" for s in selectors):
                self.assertEqual(len(selectors), 1)
                self.assertEqual(clusters, ["onpre1"])
            else:
                self.assertEqual(clusters, ["onpre1", "aws"])


if __name__ == "__main__":
    unittest.main()
