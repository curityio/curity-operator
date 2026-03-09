package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/curityio/curity-operator/internal/controller"
	"github.com/curityio/curity-operator/internal/pkg/health"
	"github.com/curityio/curity-operator/internal/pkg/kube"
)

var healthAddr string

func newRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "curity-operator",
		Short: "Kubernetes operator for Curity Identity Server",
		RunE:  run,
	}

	cmd.Flags().StringVar(&healthAddr, "health-addr", ":8081", "address for health endpoints")

	return cmd
}

func run(cmd *cobra.Command, args []string) error {
	logrus.Info("starting curity-operator")

	client, err := kube.GetClient()
	if err != nil {
		return err
	}

	go health.Start(healthAddr)

	stopCh := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	ctrl := controller.NewController(client)
	go ctrl.Run(stopCh)

	<-sigCh
	logrus.Info("received shutdown signal")
	close(stopCh)

	return nil
}

func main() {
	if err := newRootCommand().Execute(); err != nil {
		logrus.Fatal(err)
	}
}
