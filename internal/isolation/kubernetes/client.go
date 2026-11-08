// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"fmt"
	"os"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Connect returns a client for the cluster: from kubeconfig if given (local
// testing), otherwise the Pod's own service account (a worker in the
// cluster).
func Connect(kubeconfig string) (kubernetes.Interface, *rest.Config, error) {
	var cfg *rest.Config
	var err error
	if kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, nil, fmt.Errorf("kubernetes: %w", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("kubernetes: %w", err)
	}
	return client, cfg, nil
}

// apiServerAddr is the API server's address as Pods see it.
func apiServerAddr() (host, port string) {
	return os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
}
