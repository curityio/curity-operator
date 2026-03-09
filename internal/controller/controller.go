package controller

import (
	"github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
)

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
