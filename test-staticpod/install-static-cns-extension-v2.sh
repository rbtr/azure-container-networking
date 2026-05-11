#!/bin/bash
# Install the static-pod CNS test stack onto a VMSS via CustomScript extension.
# Drops three files on each VM in the VMSS as it provisions (and on existing
# VMs once `az vmss update-instances` is invoked):
#
#   1. /etc/kubernetes/manifests/azure-cns.yaml — the static pod manifest
#   2. /var/lib/azure-cns-config/cns_config.json — CNS config (replaces the
#      ConfigMap mount that mirror pods can't use)
#   3. /var/lib/azure-cns/kubeconfig.yaml — kubeconfig with the azure-cns SA
#      token (replaces the SA projected token that mirror pods can't have)
#
# Usage:
#   ./install-static-cns-extension-v2.sh <manifest.yaml> <cns_config.json> <kubeconfig.yaml> <vmss-name> <node-rg>

set -euo pipefail

if [ $# -ne 5 ]; then
    echo "usage: $0 <manifest.yaml> <cns_config.json> <kubeconfig.yaml> <vmss-name> <node-rg>"
    exit 2
fi
MANIFEST_PATH="$1"
CNS_CONFIG_PATH="$2"
KUBECONFIG_PATH="$3"
VMSS_NAME="$4"
NODE_RG="$5"

for f in "$MANIFEST_PATH" "$CNS_CONFIG_PATH" "$KUBECONFIG_PATH"; do
    [ -f "$f" ] || { echo "missing: $f"; exit 2; }
done

# Build the runner. Three heredocs; chmod after each. Idempotent.
RUNNER=$(mktemp)
trap "rm -f $RUNNER" EXIT
{
    echo '#!/bin/bash'
    echo 'set -eu'
    echo 'mkdir -p /etc/kubernetes/manifests /var/lib/azure-cns /var/lib/azure-cns-config'
    echo 'cat > /var/lib/azure-cns-config/cns_config.json <<'"'"'CFGEOF'"'"''
    cat "$CNS_CONFIG_PATH"
    echo 'CFGEOF'
    echo 'chmod 644 /var/lib/azure-cns-config/cns_config.json'
    echo 'cat > /var/lib/azure-cns/kubeconfig.yaml <<'"'"'KCFGEOF'"'"''
    cat "$KUBECONFIG_PATH"
    echo 'KCFGEOF'
    echo 'chmod 600 /var/lib/azure-cns/kubeconfig.yaml'
    # The manifest must be written LAST so kubelet's fsnotify watcher only
    # fires once kubeconfig + cns_config.json are in place.
    echo 'cat > /etc/kubernetes/manifests/azure-cns.yaml <<'"'"'YAMLEOF'"'"''
    cat "$MANIFEST_PATH"
    echo 'YAMLEOF'
    echo 'chmod 600 /etc/kubernetes/manifests/azure-cns.yaml'
    echo 'echo "wrote azure-cns static pod stack"'
} > "$RUNNER"

SCRIPT_B64=$(base64 -w0 < "$RUNNER")
SETTINGS=$(jq -n --arg s "$SCRIPT_B64" '{script:$s}')

echo "Installing CustomScript extension on VMSS $VMSS_NAME (RG $NODE_RG)..."
az vmss extension set \
    --resource-group "$NODE_RG" \
    --vmss-name "$VMSS_NAME" \
    --publisher Microsoft.Azure.Extensions \
    --name CustomScript --version 2.1 \
    --settings "$SETTINGS" \
    --output none
echo "Triggering update on existing instances..."
az vmss update-instances -g "$NODE_RG" --name "$VMSS_NAME" --instance-ids '*' --no-wait --output none
echo "Done. (CSE will run on next provisioning event for new VMs; updated existing VMs.)"
