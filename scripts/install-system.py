#!/usr/bin/env python3
"""Render staged installation manifests; explicit context required for apply."""
import argparse
import copy
import json
import os
from pathlib import Path
import subprocess
import yaml

ROOT = Path(__file__).resolve().parents[1]
STAGES = {
    "crds": ["config/crd/kustomization.yaml"],
    "karmada": ["config/karmada/kustomization.yaml"],
    "management": ["config/management/kustomization.yaml"],
    "member": ["config/member/namespace.yaml", "config/member/rbac.yaml", "config/member/runtime.yaml"],
    "aws-member": ["config/member/kustomization.yaml"],
    "placement": ["config/management/namespace.yaml", "config/management/placement-webhook.yaml"],
}


def load_documents(path):
    if not path.is_file():
        raise ValueError(f"Stage unavailable in this checkout: {path}")
    result = []
    for obj in yaml.safe_load_all(path.read_text(encoding="utf-8")):
        if not obj:
            continue
        if obj.get("kind") == "Kustomization":
            if set(obj) - {"apiVersion", "kind", "resources"}:
                raise ValueError("Unsupported transform; use kubectl kustomize and review manually")
            for resource in obj["resources"]:
                child = (path.parent / resource).resolve()
                if ROOT.resolve() not in child.parents:
                    raise ValueError("Only repository-local resources are supported")
                result.extend(load_documents(child))
        else:
            result.append(obj)
    return result


def render(documents, repository, tag, pull_secret=None):
    documents = copy.deepcopy(documents)
    for obj in documents:
        if obj.get("kind") not in ("Deployment", "DaemonSet"):
            continue
        if not repository or not tag or any(x in repository + tag for x in ("__", "<", ">", " ")):
            raise ValueError("Set --image-repository and --tag to real values")
        component = obj["metadata"]["name"]
        if not (ROOT / "cmd" / component / "main.go").is_file():
            raise ValueError(f"No entrypoint in this branch: {component}")
        pod = obj["spec"]["template"]["spec"]
        if len(pod["containers"]) != 1 or pod.get("initContainers"):
            raise ValueError("Unexpected container layout; review before installation")
        pod["containers"][0]["image"] = f"{repository}:{component}-{tag}"
        if pull_secret:
            pod["imagePullSecrets"] = [{"name": pull_secret}]
    return {"apiVersion": "v1", "kind": "List", "items": documents}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("stage", choices=STAGES)
    parser.add_argument("--image-repository", default=os.environ.get("IMAGE_REPOSITORY"))
    parser.add_argument("--tag", default=os.environ.get("IMAGE_TAG"))
    parser.add_argument("--pull-secret")
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--apply", action="store_true")
    parser.add_argument("--kubeconfig")
    parser.add_argument("--confirm-context")
    args = parser.parse_args()
    if args.apply and (not args.kubeconfig or not args.confirm_context):
        parser.error("--apply requires --kubeconfig and --confirm-context")
    documents = [obj for filename in STAGES[args.stage] for obj in load_documents(ROOT / filename)]
    result = render(documents, args.image_repository, args.tag, args.pull_secret)
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")
    print(f"Rendered {args.stage}: {args.out}")
    if not args.apply:
        print("No cluster changes. Inspect this file before applying.")
        return
    command = ["kubectl", "--kubeconfig", args.kubeconfig, "--context", args.confirm_context]
    current = subprocess.check_output(
        ["kubectl", "--kubeconfig", args.kubeconfig, "config", "current-context"], text=True).strip()
    if current != args.confirm_context:
        raise ValueError(f"Context mismatch: expected {args.confirm_context}, found {current}")
    subprocess.run(command + ["apply", "-f", str(args.out)], check=True)
    for obj in result["items"]:
        kind, name = obj["kind"], obj["metadata"]["name"]
        if kind == "CustomResourceDefinition":
            subprocess.run(command + ["wait", "--for=condition=Established",
                                      f"crd/{name}", "--timeout=120s"], check=True)
        elif kind in ("Deployment", "DaemonSet"):
            subprocess.run(command + ["-n", obj["metadata"]["namespace"], "rollout", "status",
                                      f"{kind.lower()}/{name}", "--timeout=300s"], check=True)
    print("Stage applied. Readiness does not prove end-to-end restore compatibility.")


if __name__ == "__main__":
    main()
