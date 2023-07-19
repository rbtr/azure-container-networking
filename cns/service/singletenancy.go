package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/cnireconciler"
	"github.com/Azure/azure-container-networking/cns/configuration"
	"github.com/Azure/azure-container-networking/cns/ipampool"
	cssctrl "github.com/Azure/azure-container-networking/cns/kubecontroller/clustersubnetstate"
	nncctrl "github.com/Azure/azure-container-networking/cns/kubecontroller/nodenetworkconfig"
	"github.com/Azure/azure-container-networking/cns/logger"
	"github.com/Azure/azure-container-networking/cns/restserver"
	cnstypes "github.com/Azure/azure-container-networking/cns/types"
	"github.com/Azure/azure-container-networking/crd"
	"github.com/Azure/azure-container-networking/crd/clustersubnetstate/api/v1alpha1"
	"github.com/Azure/azure-container-networking/crd/nodenetworkconfig"
	"github.com/Azure/azure-container-networking/crd/nodenetworkconfig/api/v1alpha"
	"github.com/Azure/azure-container-networking/log"
	"github.com/Azure/azure-container-networking/store"
	"github.com/avast/retry-go/v3"
	"github.com/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	kuberuntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

type nodeNetworkConfigGetter interface {
	Get(context.Context) (*v1alpha.NodeNetworkConfig, error)
}

type ncStateReconciler interface {
	ReconcileNCState(ncRequest *cns.CreateNetworkContainerRequest, podInfoByIP map[string]cns.PodInfo, nnc *v1alpha.NodeNetworkConfig) cnstypes.ResponseCode
}

// TODO(rbtr) where should this live??
// reconcileInitialCNSState initializes cns by passing pods and a CreateNetworkContainerRequest
func reconcileInitialCNSState(ctx context.Context, cli nodeNetworkConfigGetter, ncReconciler ncStateReconciler, podInfoByIPProvider cns.PodInfoByIPProvider) error {
	// Get nnc using direct client
	nnc, err := cli.Get(ctx)
	if err != nil {
		if crd.IsNotDefined(err) {
			return errors.Wrap(err, "failed to init CNS state: NNC CRD is not defined")
		}
		if apierrors.IsNotFound(err) {
			return errors.Wrap(err, "failed to init CNS state: NNC not found")
		}
		return errors.Wrap(err, "failed to init CNS state: failed to get NNC CRD")
	}

	logger.Printf("Retrieved NNC: %+v", nnc)

	// If there are no NCs, we can't initialize our state and we should fail out.
	if len(nnc.Status.NetworkContainers) == 0 {
		return errors.New("failed to init CNS state: no NCs found in NNC CRD")
	}

	// For each NC, we need to create a CreateNetworkContainerRequest and use it to rebuild our state.
	for i := range nnc.Status.NetworkContainers {
		var ncRequest *cns.CreateNetworkContainerRequest
		var err error

		switch nnc.Status.NetworkContainers[i].AssignmentMode { //nolint:exhaustive // skipping dynamic case
		case v1alpha.Static:
			ncRequest, err = nncctrl.CreateNCRequestFromStaticNC(nnc.Status.NetworkContainers[i])
		default: // For backward compatibility, default will be treated as Dynamic too.
			ncRequest, err = nncctrl.CreateNCRequestFromDynamicNC(nnc.Status.NetworkContainers[i])
		}

		if err != nil {
			return errors.Wrapf(err, "failed to convert NNC status to network container request, "+
				"assignmentMode: %s", nnc.Status.NetworkContainers[i].AssignmentMode)
		}
		// Get previous PodInfo state from podInfoByIPProvider
		podInfoByIP, err := podInfoByIPProvider.PodInfoByIP()
		if err != nil {
			return errors.Wrap(err, "provider failed to provide PodInfoByIP")
		}

		// Call cnsclient init cns passing those two things.
		if err := restserver.ResponseCodeToError(ncReconciler.ReconcileNCState(ncRequest, podInfoByIP, nnc)); err != nil {
			return errors.Wrap(err, "failed to reconcile NC state")
		}
	}
	return nil
}

// initializeCRDState builds and starts the CRD controllers.
func initializeCRDState(ctx context.Context, httpRestService cns.HTTPService, cnsconfig *configuration.CNSConfig) error {
	// convert interface type to implementation type
	httpRestServiceImplementation, ok := httpRestService.(*restserver.HTTPRestService)
	if !ok {
		logger.Errorf("[Azure CNS] Failed to convert interface httpRestService to implementation: %v", httpRestService)
		return fmt.Errorf("[Azure CNS] Failed to convert interface httpRestService to implementation: %v",
			httpRestService)
	}

	// Set orchestrator type
	orchestrator := cns.SetOrchestratorTypeRequest{
		OrchestratorType: cns.KubernetesCRD,
	}
	httpRestServiceImplementation.SetNodeOrchestrator(&orchestrator)

	// build default clientset.
	kubeConfig, err := ctrl.GetConfig()
	if err != nil {
		logger.Errorf("[Azure CNS] Failed to get kubeconfig for request controller: %v", err)
		return errors.Wrap(err, "failed to get kubeconfig")
	}
	kubeConfig.UserAgent = fmt.Sprintf("azure-cns-%s", version)

	clientset, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return errors.Wrap(err, "failed to build clientset")
	}

	// get nodename for scoping kube requests to node.
	nodeName, err := configuration.NodeName()
	if err != nil {
		return errors.Wrap(err, "failed to get NodeName")
	}

	var podInfoByIPProvider cns.PodInfoByIPProvider
	switch {
	case cnsconfig.ManageEndpointState:
		logger.Printf("Initializing from self managed endpoint store")
		podInfoByIPProvider, err = cnireconciler.NewCNSPodInfoProvider(httpRestServiceImplementation.EndpointStateStore) // get reference to endpoint state store from rest server
		if err != nil {
			if errors.Is(err, store.ErrKeyNotFound) {
				logger.Printf("[Azure CNS] No endpoint state found, skipping initializing CNS state")
			} else {
				return errors.Wrap(err, "failed to create CNS PodInfoProvider")
			}
		}
	case cnsconfig.InitializeFromCNI:
		logger.Printf("Initializing from CNI")
		podInfoByIPProvider, err = cnireconciler.NewCNIPodInfoProvider()
		if err != nil {
			return errors.Wrap(err, "failed to create CNI PodInfoProvider")
		}
	default:
		logger.Printf("Initializing from Kubernetes")
		podInfoByIPProvider = cns.PodInfoByIPProviderFunc(func() (map[string]cns.PodInfo, error) {
			pods, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{ //nolint:govet // ignore err shadow
				FieldSelector: "spec.nodeName=" + nodeName,
			})
			if err != nil {
				return nil, errors.Wrap(err, "failed to list Pods for PodInfoProvider")
			}
			podInfo, err := cns.KubePodsToPodInfoByIP(pods.Items)
			if err != nil {
				return nil, errors.Wrap(err, "failed to convert Pods to PodInfoByIP")
			}
			return podInfo, nil
		})
	}
	// create scoped kube clients.
	directcli, err := client.New(kubeConfig, client.Options{Scheme: nodenetworkconfig.Scheme})
	if err != nil {
		return errors.Wrap(err, "failed to create ctrl client")
	}
	nnccli := nodenetworkconfig.NewClient(directcli)
	if err != nil {
		return errors.Wrap(err, "failed to create NNC client")
	}
	// TODO(rbtr): nodename and namespace should be in the cns config
	scopedcli := nncctrl.NewScopedClient(nnccli, types.NamespacedName{Namespace: "kube-system", Name: nodeName})

	clusterSubnetStateChan := make(chan v1alpha1.ClusterSubnetState)
	// initialize the ipam pool monitor
	poolOpts := ipampool.Options{
		RefreshDelay: poolIPAMRefreshRateInMilliseconds * time.Millisecond,
	}
	poolMonitor := ipampool.NewMonitor(httpRestServiceImplementation, scopedcli, clusterSubnetStateChan, &poolOpts)
	httpRestServiceImplementation.IPAMPoolMonitor = poolMonitor

	logger.Printf("Reconciling initial CNS state")
	// apiserver nnc might not be registered or api server might be down and crashloop backof puts us outside of 5-10 minutes we have for
	// aks addons to come up so retry a bit more aggresively here.
	// will retry 10 times maxing out at a minute taking about 8 minutes before it gives up.
	attempt := 0
	err = retry.Do(func() error {
		attempt++
		logger.Printf("reconciling initial CNS state attempt: %d", attempt)
		err = reconcileInitialCNSState(ctx, scopedcli, httpRestServiceImplementation, podInfoByIPProvider)
		if err != nil {
			logger.Errorf("failed to reconcile initial CNS state, attempt: %d err: %v", attempt, err)
		}
		return errors.Wrap(err, "failed to initialize CNS state")
	}, retry.Context(ctx), retry.Delay(initCNSInitalDelay), retry.MaxDelay(time.Minute))
	if err != nil {
		return err
	}
	logger.Printf("reconciled initial CNS state after %d attempts", attempt)

	// start the pool Monitor before the Reconciler, since it needs to be ready to receive an
	// NodeNetworkConfig update by the time the Reconciler tries to send it.
	go func() {
		logger.Printf("Starting IPAM Pool Monitor")
		if e := poolMonitor.Start(ctx); e != nil {
			logger.Errorf("[Azure CNS] Failed to start pool monitor with err: %v", e)
		}
	}()
	logger.Printf("initialized and started IPAM pool monitor")

	// the nodeScopedCache sets Selector options on the Manager cache which are used
	// to perform *server-side* filtering of the cached objects. This is very important
	// for high node/pod count clusters, as it keeps us from watching objects at the
	// whole cluster scope when we are only interested in the Node's scope.
	nodeScopedCache := cache.BuilderWithOptions(cache.Options{
		SelectorsByObject: cache.SelectorsByObject{
			&v1alpha.NodeNetworkConfig{}: {
				Field: fields.SelectorFromSet(fields.Set{"metadata.name": nodeName}),
			},
		},
	})

	crdSchemes := kuberuntime.NewScheme()
	if err = v1alpha.AddToScheme(crdSchemes); err != nil {
		return errors.Wrap(err, "failed to add nodenetworkconfig/v1alpha to scheme")
	}
	if err = v1alpha1.AddToScheme(crdSchemes); err != nil {
		return errors.Wrap(err, "failed to add clustersubnetstate/v1alpha1 to scheme")
	}
	manager, err := ctrl.NewManager(kubeConfig, ctrl.Options{
		Scheme:             crdSchemes,
		MetricsBindAddress: "0",
		Namespace:          "kube-system", // TODO(rbtr): namespace should be in the cns config
		NewCache:           nodeScopedCache,
	})
	if err != nil {
		return errors.Wrap(err, "failed to create manager")
	}

	// get our Node so that we can xref it against the NodeNetworkConfig's to make sure that the
	// NNC is not stale and represents the Node we're running on.
	node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return errors.Wrapf(err, "failed to get node %s", nodeName)
	}

	// get CNS Node IP to compare NC Node IP with this Node IP to ensure NCs were created for this node
	nodeIP := configuration.NodeIP()

	// NodeNetworkConfig reconciler
	nncReconciler := nncctrl.NewReconciler(httpRestServiceImplementation, poolMonitor, nodeIP)
	// pass Node to the Reconciler for Controller xref
	if err := nncReconciler.SetupWithManager(manager, node); err != nil { //nolint:govet // intentional shadow
		return errors.Wrapf(err, "failed to setup nnc reconciler with manager")
	}

	if cnsconfig.EnableSubnetScarcity {
		// ClusterSubnetState reconciler
		cssReconciler := cssctrl.New(clusterSubnetStateChan)
		if err := cssReconciler.SetupWithManager(manager); err != nil {
			return errors.Wrapf(err, "failed to setup css reconciler with manager")
		}
	}

	// adding some routes to the root service mux
	mux := httpRestServiceImplementation.Listener.GetMux()
	mux.Handle("/readyz", http.StripPrefix("/readyz", &healthz.Handler{}))
	if cnsconfig.EnablePprof {
		httpRestServiceImplementation.RegisterPProfEndpoints()
	}

	// Start the Manager which starts the reconcile loop.
	// The Reconciler will send an initial NodeNetworkConfig update to the PoolMonitor, starting the
	// Monitor's internal loop.
	go func() {
		logger.Printf("Starting controller-manager.")
		for {
			if err := manager.Start(ctx); err != nil {
				logger.Errorf("Failed to start controller-manager: %v", err)
				// retry to start the request controller
				// inc the managerStartFailures metric for failure tracking
				managerStartFailures.Inc()
			} else {
				logger.Printf("Stopped controller-manager.")
				return
			}
			time.Sleep(time.Second) // TODO(rbtr): make this exponential backoff
		}
	}()
	logger.Printf("Initialized controller-manager.")
	for {
		logger.Printf("Waiting for NodeNetworkConfig reconciler to start.")
		// wait for the Reconciler to run once on a NNC that was made for this Node.
		// the nncReadyCtx has a timeout of 15 minutes, after which we will consider
		// this false and the NNC Reconciler stuck/failed, log and retry.
		nncReadyCtx, _ := context.WithTimeout(ctx, 15*time.Minute) //nolint // it will time out and not leak
		if started, err := nncReconciler.Started(nncReadyCtx); !started {
			log.Errorf("NNC reconciler has not started, does the NNC exist? err: %v", err)
			nncReconcilerStartFailures.Inc()
			continue
		}
		logger.Printf("NodeNetworkConfig reconciler has started.")
		break
	}

	go func() {
		logger.Printf("Starting SyncHostNCVersion loop.")
		// Periodically poll vfp programmed NC version from NMAgent
		tickerChannel := time.Tick(time.Duration(cnsconfig.SyncHostNCVersionIntervalMs) * time.Millisecond)
		for {
			select {
			case <-tickerChannel:
				timedCtx, cancel := context.WithTimeout(ctx, time.Duration(cnsconfig.SyncHostNCVersionIntervalMs)*time.Millisecond)
				httpRestServiceImplementation.SyncHostNCVersion(timedCtx, cnsconfig.ChannelMode)
				cancel()
			case <-ctx.Done():
				logger.Printf("Stopping SyncHostNCVersion loop.")
				return
			}
		}
	}()
	logger.Printf("Initialized SyncHostNCVersion loop.")
	return nil
}
