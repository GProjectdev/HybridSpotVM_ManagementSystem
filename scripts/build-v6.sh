#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
repository="${IMAGE_REPOSITORY:-jeongseungjun/hybrid-spot-vm-system}"
tag="${IMAGE_TAG:-v6.0}"
command -v buildah >/dev/null
revision="$(git rev-parse HEAD)"
components=(
  public-cloud-vm-collector
  spot-vm-management-controller
  checkpoint-recovery-coordinator
  training-lifecycle-controller
  training-runtime-controller
  spot-watcher
  placement-webhook
)
for component in "${components[@]}"; do
  buildah bud --platform linux/amd64 --build-arg "COMPONENT=$component" \
    --label "org.opencontainers.image.revision=$revision" \
    --label "org.opencontainers.image.version=$tag" -t "$repository:$component-$tag" .
done
# Publish only after every component has built successfully.
if [[ "${PUSH_IMAGES:-0}" == 1 ]]; then
  for component in "${components[@]}"; do
    buildah push "$repository:$component-$tag" "docker://$repository:$component-$tag"
  done
fi
