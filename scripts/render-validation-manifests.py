#!/usr/bin/env python3
"""Render controller manifests locally; never contacts a cluster or applies objects."""
import argparse
import json
import os
import sys

import yaml


def render(documents, images, cluster=None, control_plane=False):
    result = []
    for obj in documents:
        if obj is None:
            continue
        if obj.get("kind") not in ("Deployment", "DaemonSet"):
            result.append(obj)
            continue
        pod = obj["spec"]["template"]["spec"]
        for container in pod.get("containers", []) + pod.get("initContainers", []):
            old = container.get("image", "")
            if old in images:
                container["image"] = images[old]
            args = container.get("args", [])
            container["args"] = [
                "--cluster-name=" + cluster if cluster and a.startswith("--cluster-name=")
                else "--payload-image=" + images["payload"] if a.startswith("--payload-image=")
                else a for a in args
            ]
        if obj["metadata"]["name"] == "stateful-artifact":
            pod.setdefault("nodeSelector", {})["migration.dcnlab.com/artifact-node"] = "true"
        if control_plane and obj["kind"] == "Deployment":
            tolerations = pod.setdefault("tolerations", [])
            for key in ("node-role.kubernetes.io/control-plane", "node-role.kubernetes.io/master"):
                item = {"key": key, "operator": "Exists", "effect": "NoSchedule"}
                if item not in tolerations:
                    tolerations.append(item)
        result.append(obj)
    return {"apiVersion": "v1", "kind": "List", "items": result}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cluster")
    parser.add_argument("--control-plane-tolerations", action="store_true")
    args = parser.parse_args()
    names = {
        "ghcr.io/gprojectdev/hybridspot-management:dev": "SYSTEM_IMAGE",
        "ghcr.io/gprojectdev/pv-migration-system:dev": "PV_IMAGE",
        "ghcr.io/gprojectdev/stateful-migration-system:dev": "STATEFUL_IMAGE",
        "docker.io/lehuannhatrang/fluidcr-webhook:v0.2": "INJECTOR_IMAGE",
        "payload": "PAYLOAD_IMAGE",
    }
    images = {old: os.environ[var] for old, var in names.items()}
    if any(not value or "__REPLACE" in value for value in images.values()):
        parser.error("all image variables must be filled before rendering")
    json.dump(render(yaml.safe_load_all(sys.stdin), images, args.cluster,
                     args.control_plane_tolerations), sys.stdout, indent=2)
    print()


if __name__ == "__main__":
    main()
