package watchpods

import (
	"context"
	"fmt"
	"os"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// getNodeName checks the environment variables for the NODENAME and returns it or an error if unset.
func getNodeName() (string, error) {
	nodeName := os.Getenv("NODENAME")
	if nodeName == "" {
		return "", errors.New("NODENAME environment variable not set")
	}
	return nodeName, nil
}

func Execute() error {
	cfg := ctrl.GetConfigOrDie()
	nodeName, err := getNodeName()
	if err != nil {
		return errors.Wrap(err, "failed to get NodeName")
	}
	manager, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:             scheme.Scheme,
		MetricsBindAddress: "0",
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Pod{}: {
					Field: fields.SelectorFromSet(fields.Set{"spec.nodeName": nodeName}),
				},
			},
		},
	})
	if err != nil {
		return errors.Wrap(err, "failed to create manager")
	}
	if err := manager.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, "spec.nodeName", client.IndexerFunc(func(o client.Object) []string {
		return []string{o.(*corev1.Pod).Spec.NodeName}
	})); err != nil {
		return errors.Wrap(err, "failed to index pod:spec.nodeName")
	}
	p := New(nodeName)
	p.ReconcileFuncs = append(p.ReconcileFuncs, p.PodNotifierFunc(func(p []corev1.Pod) []corev1.Pod { return p }, p))
	fmt.Println("starting podwatcher")
	if err := p.SetupWithManager(manager); err != nil {
		return errors.Wrap(err, "failed to setup podwatcher")
	}
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	return manager.Start(ctrl.SetupSignalHandler())
}
