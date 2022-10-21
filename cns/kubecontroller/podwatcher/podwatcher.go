package podwatcher

import (
	"context"

	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type podcli interface {
	List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error
}

type podWatcher struct {
	cli     podcli
	sink    chan<- int
	listOpt client.ListOption
}

func New(nodename string, podcounter chan<- int) *podWatcher { //nolint:revive // private struct to force constructor
	return &podWatcher{
		sink:    podcounter,
		listOpt: &client.ListOptions{FieldSelector: fields.SelectorFromSet(fields.Set{"spec.nodeName": nodename})},
	}
}

func (p *podWatcher) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	podList := &v1.PodList{}
	if err := p.cli.List(ctx, podList, p.listOpt); err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to list pods")
	}
	p.sink <- len(podList.Items)
	return reconcile.Result{}, nil
}

// SetupWithManager Sets up the reconciler with a new manager, filtering using NodeNetworkConfigFilter on nodeName.
func (p *podWatcher) SetupWithManager(mgr ctrl.Manager) error {
	p.cli = mgr.GetClient()
	err := ctrl.NewControllerManagedBy(mgr).
		For(&v1.Pod{}).
		WithEventFilter(predicate.Funcs{
			// ignore Status only changes - they don't update the generation
			UpdateFunc: func(ue event.UpdateEvent) bool {
				return ue.ObjectOld.GetGeneration() != ue.ObjectNew.GetGeneration()
			},
		}).
		Complete(p)
	if err != nil {
		return errors.Wrap(err, "failed to set up pod watcher with manager")
	}
	return nil
}
