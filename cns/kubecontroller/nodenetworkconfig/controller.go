package nodenetworkconfig

import (
	"context"

	"github.com/Azure/azure-container-networking/crd/nodenetworkconfig/api/v1alpha"
	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type nncCreator interface {
	Create(context.Context, types.NamespacedName) (*v1alpha.NodeNetworkConfig, error)
	SetOwnerRef(context.Context, types.NamespacedName, metav1.Object) (*v1alpha.NodeNetworkConfig, error)
}

// Create stubs a NodeNetworkConfig for the passed Node. It is not an error if
// one already exists. After the NodeNetworkConfig is stubbed, the Node is set
// as the Owner so that Kubernetes will GC it in step with the Node lifecycle.
func Create(ctx context.Context, cli nncCreator, node *v1.Node) error {
	// get our Node so that we can set the OwnerRef on the NNC
	key := types.NamespacedName{Name: node.Name, Namespace: "kube-system"}
	_, err := cli.Create(ctx, key)
	if err != nil {
		if !apierrors.IsAlreadyExists(err) {
			// return any error which is *not* an AlreadyExists error
			return errors.Wrapf(err, "failed to create NodeNetworkConfig %s", key)
		}
		// ignore otherwise
	}
	// set owner ref
	if _, err = cli.SetOwnerRef(ctx, key, node); err != nil {
		return errors.Wrapf(err, "failed to set ownerref for NodeNetworkConfig %s", key)
	}
	return nil
}
