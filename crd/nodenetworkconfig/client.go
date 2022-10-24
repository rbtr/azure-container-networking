package nodenetworkconfig

import (
	"context"
	"reflect"

	"github.com/Azure/azure-container-networking/crd"
	"github.com/Azure/azure-container-networking/crd/nodenetworkconfig/api/v1alpha"
	"github.com/pkg/errors"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	typedv1 "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Scheme is a runtime scheme containing the client-go scheme and the NodeNetworkConfig scheme.
var Scheme = runtime.NewScheme()

func init() {
	_ = scheme.AddToScheme(Scheme)
	_ = v1alpha.AddToScheme(Scheme)
}

// Installer provides methods to manage the lifecycle of the NodeNetworkConfig resource definition.
type Installer struct {
	cli typedv1.CustomResourceDefinitionInterface
}

func NewInstaller(c *rest.Config) (*Installer, error) {
	cli, err := crd.NewCRDClientFromConfig(c)
	if err != nil {
		return nil, errors.Wrap(err, "failed to init crd client")
	}
	return &Installer{
		cli: cli,
	}, nil
}

func (i *Installer) create(ctx context.Context, res *v1.CustomResourceDefinition) (*v1.CustomResourceDefinition, error) {
	res, err := i.cli.Create(ctx, res, metav1.CreateOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "failed to create nnc crd")
	}
	return res, nil
}

// Install installs the embedded NodeNetworkConfig CRD definition in the cluster.
func (i *Installer) Install(ctx context.Context) (*v1.CustomResourceDefinition, error) {
	nnc, err := GetNodeNetworkConfigs()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get embedded nnc crd")
	}
	return i.create(ctx, nnc)
}

// InstallOrUpdate installs the embedded NodeNetworkConfig CRD definition in the cluster or updates it if present.
func (i *Installer) InstallOrUpdate(ctx context.Context) (*v1.CustomResourceDefinition, error) {
	nnc, err := GetNodeNetworkConfigs()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get embedded nnc crd")
	}
	current, err := i.create(ctx, nnc)
	if !apierrors.IsAlreadyExists(err) {
		return current, err
	}
	if current == nil {
		current, err = i.cli.Get(ctx, nnc.Name, metav1.GetOptions{})
		if err != nil {
			return nil, errors.Wrap(err, "failed to get existing nnc crd")
		}
	}
	if !reflect.DeepEqual(nnc.Spec.Versions, current.Spec.Versions) {
		nnc.SetResourceVersion(current.GetResourceVersion())
		previous := *current
		current, err = i.cli.Update(ctx, nnc, metav1.UpdateOptions{})
		if err != nil {
			return &previous, errors.Wrap(err, "failed to update existing nnc crd")
		}
	}
	return current, nil
}

// Client provides methods to interact with instances of the NodeNetworkConfig custom resource.
type Client struct {
	cli      client.Client
	identity client.FieldOwner
}

// NewClient creates a new NodeNetworkConfig client around the passed ctrlcli.Client.
func NewClient(cli client.Client, identity string) *Client {
	return &Client{
		cli:      cli,
		identity: client.FieldOwner(identity),
	}
}

// Create makes a new NodeNetworkConfig identified by the NamespacedName.
func (c *Client) Create(ctx context.Context, key types.NamespacedName) (*v1alpha.NodeNetworkConfig, error) {
	obj := skel(key)
	err := c.cli.Create(ctx, obj, c.identity)
	return obj, errors.Wrapf(err, "failed to create nnc %v", key)
}

// Get returns the NodeNetworkConfig identified by the NamespacedName.
func (c *Client) Get(ctx context.Context, key types.NamespacedName) (*v1alpha.NodeNetworkConfig, error) {
	nodeNetworkConfig := &v1alpha.NodeNetworkConfig{}
	err := c.cli.Get(ctx, key, nodeNetworkConfig)
	return nodeNetworkConfig, errors.Wrapf(err, "failed to get nnc %v", key)
}

// Patch performs a server-side patch of the passed NodeNetworkConfigSpec to the NodeNetworkConfig specified by the NamespacedName.
func (c *Client) Patch(ctx context.Context, key types.NamespacedName, spec *v1alpha.NodeNetworkConfigSpec) (*v1alpha.NodeNetworkConfig, error) {
	obj := skel(key)
	obj.Spec = *spec
	if err := c.cli.Patch(ctx, obj, client.Apply, client.ForceOwnership, c.identity); err != nil {
		return nil, errors.Wrap(err, "failed to patch nnc")
	}
	return obj, nil
}

func (c *Client) PatchStatus(ctx context.Context, key types.NamespacedName, status *v1alpha.NodeNetworkConfigStatus) (*v1alpha.NodeNetworkConfig, error) {
	obj := skel(key)
	obj.Status = *status
	if err := c.cli.Status().Patch(ctx, obj, client.Apply, client.ForceOwnership, c.identity); err != nil {
		return nil, errors.Wrap(err, "failed to patch nnc status")
	}
	return obj, nil
}

// Update does a fetch, deepcopy, and update of the NodeNetworkConfig with the passed spec.
// Deprecated: Update is deprecated and usage should migrate to PatchSpec.
func (c *Client) Update(ctx context.Context, key types.NamespacedName, spec *v1alpha.NodeNetworkConfigSpec) (*v1alpha.NodeNetworkConfig, error) {
	nnc, err := c.Get(ctx, key)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get nnc")
	}
	spec.DeepCopyInto(&nnc.Spec)
	if err := c.cli.Update(ctx, nnc); err != nil {
		return nil, errors.Wrap(err, "failed to update nnc")
	}
	return nnc, nil
}

// SetOwnerRef sets the owner of the NodeNetworkConfig to the given object, using HTTP Patch.
func (c *Client) SetOwnerRef(ctx context.Context, key types.NamespacedName, owner metav1.Object) (*v1alpha.NodeNetworkConfig, error) {
	obj := skel(key)
	if err := ctrlutil.SetControllerReference(owner, obj, Scheme); err != nil {
		return nil, errors.Wrapf(err, "failed to set controller reference for nnc")
	}
	if err := c.cli.Patch(ctx, obj, client.Apply, client.ForceOwnership, c.identity); err != nil {
		return nil, errors.Wrapf(err, "failed to patch nnc")
	}
	return obj, nil
}

func skel(key types.NamespacedName) *v1alpha.NodeNetworkConfig {
	return &v1alpha.NodeNetworkConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha.GroupVersion.String(),
			Kind:       "NodeNetworkConfig",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
		},
	}
}
