package controller

import (
	"github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
)

// +kubebuilder:rbac:groups=curity.io,resources=identityservers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=curity.io,resources=identityservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=curity.io,resources=identityservers/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services;configmaps;secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

type Controller struct {
	client kubernetes.Interface
}

func NewController(client kubernetes.Interface) *Controller {
	return &Controller{
		client: client,
	}
}

func (c *Controller) Run(stopCh <-chan struct{}) {
	logrus.Info("starting curity-operator controller")
	<-stopCh
	logrus.Info("shutting down controller")
}
