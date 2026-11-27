package main

import (
	"log"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"

	"github.com/epheo/stornas/internal/agent"
	"github.com/epheo/stornas/internal/lvm"
	"github.com/epheo/stornas/internal/mdraid"
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
	ns := os.Getenv("POD_NAMESPACE")
	if ns == "" {
		ns = "stornas-system"
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  scheme,
		Metrics: server.Options{BindAddress: "0"},
		// The agent reads ConfigMaps only in its own namespace and only its
		// own Node: its login banner needs nothing else.
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&corev1.ConfigMap{}: {Namespaces: map[string]cache.Config{ns: {}}},
			&corev1.Node{}:      {Field: fields.OneTermEqualSelector("metadata.name", node)},
		}},
	})
	if err != nil {
		log.Fatal(err)
	}

	host := agent.HostRunner{}
	smart := agent.NewSmartStore()
	md := mdraid.NewWithRunner(host)
	r := &agent.PoolReconciler{Client: mgr.GetClient(), Node: node, LVM: lvm.NewWithRunner(host), MD: md, Smart: smart}
	if err := r.SetupWithManager(mgr); err != nil {
		log.Fatal(err)
	}
	shares := &agent.ShareAgentReconciler{
		Client: mgr.GetClient(),
		Node:   node,
		Shares: &agent.ShareManager{Run: host, Node: node},
	}
	if err := shares.SetupWithManager(mgr); err != nil {
		log.Fatal(err)
	}
	users := &agent.UserAgentReconciler{Client: mgr.GetClient(), Secrets: mgr.GetAPIReader(), Run: host}
	if err := users.SetupWithManager(mgr); err != nil {
		log.Fatal(err)
	}
	inv := &agent.InventoryPublisher{Client: mgr.GetClient(), Node: node, Run: host, MD: md, Smart: smart,
		PlanPath: "/etc/amorce/data-disks.json"}
	if err := mgr.Add(inv); err != nil {
		log.Fatal(err)
	}
	targets := &agent.TargetAgentReconciler{
		Client:  mgr.GetClient(),
		Secrets: mgr.GetAPIReader(),
		Node:    node,
		LIO:     &agent.LIOManager{Run: host},
	}
	if err := targets.SetupWithManager(mgr); err != nil {
		log.Fatal(err)
	}

	banner := &agent.BannerReconciler{Client: mgr.GetClient(), Secrets: mgr.GetAPIReader(), Run: host, Namespace: ns, Node: node}
	if err := banner.SetupWithManager(mgr); err != nil {
		log.Fatal(err)
	}

	log.Printf("stornas-agent %s on node %s", version, node)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Fatal(err)
	}
}
