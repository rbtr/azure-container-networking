package kube

import (
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
)

func GetClientOrDie() *kubernetes.Clientset {
	kubeconf := ctrl.GetConfigOrDie()
	return kubernetes.NewForConfigOrDie(kubeconf)
}
