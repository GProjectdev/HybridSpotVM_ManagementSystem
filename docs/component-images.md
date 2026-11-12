# v6.0 component images

The actual-presentation architecture replaces the old deployment names.
See [migration gates](v6-migration.md) before switching an existing installation.

| Build COMPONENT | Location | Responsibility |
| --- | --- | --- |
| public-cloud-vm-collector | Management | Availability input and risk profile |
| spot-vm-management-controller | Management | VM configuration and lifecycle |
| checkpoint-recovery-coordinator | Management | Checkpoint and restore requests |
| training-lifecycle-controller | Management | Workload placement and runtime registration |
| training-runtime-controller | Member | Training progress and iteration telemetry |
| spot-watcher | Member node | Interruption notice reporting |
| placement-webhook | Management | Supporting placement admission |

Image convention: jeongseungjun/hybrid-spot-vm-system:COMPONENT-v6.0.
The base Dockerfile requires COMPONENT; images have fixed entrypoints, no mode.

Build all images using Buildah on Linux:

```bash
bash scripts/build-v6.sh
# After testing the built images and authenticating to the registry:
PUSH_IMAGES=1 bash scripts/build-v6.sh
python scripts/install-system.py management --image-repository jeongseungjun/hybrid-spot-vm-system --tag v6.0 --out management-v6.json
python scripts/install-system.py aws-member --image-repository jeongseungjun/hybrid-spot-vm-system --tag v6.0 --out aws-member-v6.json
```

Rendering does not apply manifests. Stop old controller Pods before applying
renamed deployments. CR wire kinds remain compatible; NodeRequest is not yet a
replacement CRD for TrainingPolicy.

Management deployments still share the existing Karmada kubeconfig and RBAC.
Component separation is not a security isolation claim. Existing supporting
provisioner, storage and checkpoint/restore images have their own release files.
