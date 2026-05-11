package observer

import (
	"encoding/json"

	nncv1alpha "github.com/Azure/azure-container-networking/crd/nodenetworkconfig/api/v1alpha"
)

func nncFromJSON(raw []byte, out *nncv1alpha.NodeNetworkConfig) error {
	return json.Unmarshal(raw, out)
}
