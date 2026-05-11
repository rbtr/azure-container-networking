#!/bin/bash
# Helper that builds a VMSS CustomScript extension settings JSON for the
# given static-pod manifest. Invoked once per arm (B-pre, B-pull) to
# (re)install the extension on the cluster's nodepool VMSS.
#
# Usage:
#   ./install-static-cns-extension.sh <manifest.yaml> <vmss-name> <node-rg>
#
# The extension drops the static pod manifest at the standard kubelet
# manifest path on each new VM as it provisions.

set -euo pipefail

if [ $# -ne 3 ]; then
    echo "usage: $0 <static-pod-manifest.yaml> <vmss-name> <node-resource-group>"
    exit 2
fi
MANIFEST_PATH="$1"
VMSS_NAME="$2"
NODE_RG="$3"

if [ ! -f "$MANIFEST_PATH" ]; then
    echo "manifest not found: $MANIFEST_PATH"
    exit 2
fi

# Build the runner script that actually executes on each VM. The static-pod
# manifest is heredoc'd into kubelet's watched directory.
RUNNER=$(mktemp)
trap "rm -f $RUNNER" EXIT
{
    echo '#!/bin/bash'
    echo 'set -eu'
    echo 'mkdir -p /etc/kubernetes/manifests'
    echo 'cat > /etc/kubernetes/manifests/azure-cns.yaml <<'"'"'YAMLEOF'"'"''
    cat "$MANIFEST_PATH"
    echo 'YAMLEOF'
    echo 'chmod 600 /etc/kubernetes/manifests/azure-cns.yaml'
    echo 'echo "wrote azure-cns static pod manifest"'
} > "$RUNNER"

# Encode and apply.
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
echo "Done."
