#!/usr/bin/env bash
set -euo pipefail
# Input must be a flattened, certificate-data-based administrator kubeconfig.
: "${KARMADA_KUBECONFIG:?set KARMADA_KUBECONFIG}"
: "${OUTPUT_KUBECONFIG:?set OUTPUT_KUBECONFIG to a private output path}"
umask 077
server=$(kubectl --kubeconfig="$KARMADA_KUBECONFIG" config view --minify --raw -o jsonpath='{.clusters[0].cluster.server}')
ca=$(kubectl --kubeconfig="$KARMADA_KUBECONFIG" config view --minify --flatten --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
test -n "$server"
test -n "$ca"
token=$(kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n hybridspot-system create token hybridspot-management --duration=24h)
cat > "$OUTPUT_KUBECONFIG" <<EOF
apiVersion: v1
kind: Config
clusters:
- name: karmada
  cluster:
    server: $server
    certificate-authority-data: $ca
users:
- name: hybridspot-management
  user:
    token: $token
contexts:
- name: karmada
  context:
    cluster: karmada
    user: hybridspot-management
current-context: karmada
EOF
echo "Wrote kubeconfig; rotate token before expiry (server may shorten requested lifetime)."
