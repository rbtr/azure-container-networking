package kube

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
)

type podLister interface {
	List(context.Context, metav1.ListOptions) (*corev1.PodList, error)
}

func GetPodsByNode(ctx context.Context, podcli podLister, node string) (*corev1.PodList, error) {
	nodeSelector := fields.SelectorFromSet(fields.Set{"spec.nodeName": node})
	return podcli.List(ctx, metav1.ListOptions{FieldSelector: nodeSelector.String()}) //nolint:wrapcheck // test file
}

func FilterHostnet(pods []corev1.Pod) []corev1.Pod {
	out := []corev1.Pod{}
	for i := range pods {
		if !pods[i].Spec.HostNetwork {
			out = append(out, pods[i])
		}
	}
	return out
}
