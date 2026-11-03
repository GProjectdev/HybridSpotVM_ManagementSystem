# Compatibility Constraints

- Keep upstream Karmada unchanged. Resolve integration issues in our PV Migration and System controllers, not a Karmada fork or controller-manager image.
- Preserve UID, ownership, NFS, source fencing and restore verification. Never mark a failed migration Completed manually.
- Standalone claims use separate PV/PVC Works with reciprocal binding receipts; keep the restore gate compatible with PV-Migration-System.
- Use Buildah for builds/pushes and full kubectl commands without shortcut functions.
