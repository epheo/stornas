package main

import (
	"log"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"

	"github.com/epheo/stornas/internal/agent"
	"github.com/epheo/stornas/internal/lvm"
)

var version = "dev"

func main() {
	node := os.Getenv("NODE_NAME")
	if node == "" {
		log.Fatal("NODE_NAME is required (set from spec.nodeName in the DaemonSet)")
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		log.Fatal(err)
	}
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		log.Fatal(err)
	}

	// One agent per node and each acts only on its own pools, so leader
	// election would serialize the whole fleet behind one host.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  scheme,
		Metrics: server.Options{BindAddress: "0"},
	})
	if err != nil {
		log.Fatal(err)
	}

	r := &agent.PoolReconciler{Client: mgr.GetClient(), Node: node, LVM: lvm.New()}
	if err := r.SetupWithManager(mgr); err != nil {
		log.Fatal(err)
	}

	log.Printf("stornas-agent %s on node %s", version, node)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Fatal(err)
	}
}
