package controller

import (
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

func TestNewController(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // TODO: migrate to fake.NewClientset when applyconfig is available
	c := NewController(client)

	if c == nil {
		t.Fatal("expected controller to be non-nil")
	}
	if c.client == nil {
		t.Fatal("expected client to be set")
	}
}

func TestControllerRunStopsOnSignal(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // TODO: migrate to fake.NewClientset when applyconfig is available
	c := NewController(client)

	stopCh := make(chan struct{})
	done := make(chan struct{})

	go func() {
		c.Run(stopCh)
		close(done)
	}()

	close(stopCh)
	<-done
}
