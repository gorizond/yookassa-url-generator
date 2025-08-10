package main

import (
	"flag"

	"github.com/gorizond/payment-url-generator/controllers"
	"github.com/rancher/wrangler/v3/pkg/kubeconfig"
	"github.com/rancher/wrangler/v3/pkg/signals"
	"github.com/rancher/wrangler/v3/pkg/start"
	"k8s.io/client-go/rest"
)

func main() {
	ctx := signals.SetupSignalContext()

	var kubeconfigPath string
	flag.StringVar(&kubeconfigPath, "kubeconfig", "", "Path to kubeconfig file")
	flag.Parse()

	var config *rest.Config
	var err error

	if kubeconfigPath != "" {
		config, err = kubeconfig.GetNonInteractiveClientConfig(kubeconfigPath).ClientConfig()
		if err != nil {
			panic(err)
		}
	} else {
		config, err = rest.InClusterConfig()
		if err != nil {
			panic(err)
		}
	}

	// Initialize controllers
	controllers.InitPaymentURLController(ctx, config)

	// Start the controllers
	if err := start.All(ctx, 50); err != nil {
		panic(err)
	}

	<-ctx.Done()
}
