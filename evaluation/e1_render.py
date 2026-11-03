#!/usr/bin/env python3
"""Offline E1 manifests. Never creates cloud resources or calls kubectl."""
import argparse
import copy
from pathlib import Path

from run import read, render, save


def render_e1(config, source, run_id):
    config = copy.deepcopy(config)
    config.update(cluster="aws", scenario="constant", restore_audit=True)
    config.pop("risk_schedule", None)
    run, storage, resources, policy = render(config, source, run_id, "checkpoint", "F300")
    run.update(experiment="e1", arm="cross-cluster", source_cluster="onpre1", target_cluster="aws")
    policy["spec"]["sourceCluster"] = "onpre1"
    policy["spec"]["capacity"]["mode"] = "Provisioned"
    policy["spec"]["capacity"].pop("existingNodes", None)
    policy["spec"]["replacement"] = {"enabled": False}
    policy["spec"]["policy"].update(minOnDemand=0, fixedOnDemand=0)
    policy["metadata"]["annotations"]["training.dcnlab.com/periodic-checkpoint"] = "disabled"
    # Release suspension only after the driver observes pending move intent.
    placement = next(o for o in resources if o["kind"] == "PropagationPolicy")
    dependencies = copy.deepcopy(placement)
    dependencies["metadata"]["name"] = run["app"] + "-dependencies"
    selectors = placement["spec"]["resourceSelectors"]
    dependencies["spec"]["resourceSelectors"] = [s for s in selectors if s["kind"] != "StatefulSet"]
    dependencies["spec"]["placement"]["clusterAffinity"]["clusterNames"] = ["onpre1", "aws"]
    placement["spec"]["resourceSelectors"] = [s for s in selectors if s["kind"] == "StatefulSet"]
    placement["spec"]["placement"]["clusterAffinity"]["clusterNames"] = ["onpre1"]
    resources.append(dependencies)
    return run, storage, resources, policy


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", required=True, help="source storage_class must exist on onpre1")
    parser.add_argument("--policy-source", required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--out", required=True)
    args = parser.parse_args()
    out = Path(args.out)
    if out.exists():
        parser.error("output path already exists; use a fresh run ID")
    run, storage, resources, policy = render_e1(read(args.config), read(args.policy_source), args.run_id)
    out.mkdir(parents=True)
    save(out / "run.json", run)
    save(out / "onpre-storage.json", {"apiVersion": "v1", "kind": "List", "items": storage})
    save(out / "karmada.json", {"apiVersion": "v1", "kind": "List", "items": resources})
    save(out / "policy.unbound.json", policy)
    print("Rendered only. Follow docs/e1-shared-pvc-v5.0.3.md; do not use checkpoint.py start/prepare.")


if __name__ == "__main__":
    main()
