package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/epheo/stornas/internal/clusterstate"
	"github.com/epheo/stornas/internal/eventbus"
	"github.com/epheo/stornas/internal/stream"
)

var version = "dev"

func main() {
	addr := flag.String("addr", envOr("STORNAS_ADDR", ":8080"), "listen address")
	webDir := flag.String("web", envOr("STORNAS_WEB", "web/build"), "built SPA directory")
	flag.Parse()

	cfg, err := kubeConfig()
	if err != nil {
		log.Fatalf("kubernetes config: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Fatal(err)
	}

	ctx := ctrl.SetupSignalHandler()
	bus := eventbus.New()
	state := clusterstate.New(cs, dyn, bus)
	state.Run(ctx)

	kinds := []eventbus.Kind{eventbus.PoolChanged, eventbus.NodeChanged, eventbus.VolumeChanged, eventbus.ShareChanged}
	wake, cancel := bus.Subscribe(kinds...)
	defer cancel()
	hub := stream.NewHub(state.Snapshot, wake, func() uint64 { return bus.Version(kinds...) })
	go hub.Run(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /api/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"version": version, "healthy": state.Healthy()})
	})
	mux.HandleFunc("GET /api/v1/state", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, state.Snapshot())
	})
	mux.Handle("GET /api/v1/stream", hub)
	mux.Handle("GET /", spaHandler(*webDir))

	srv := &http.Server{Addr: *addr, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(nil) //nolint:staticcheck // shutdown at process exit; no drain deadline needed
	}()

	log.Printf("stornas %s listening on %s", version, *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// kubeConfig prefers in-cluster (the normal deployment), falling back to
// kubeconfig loading rules for development outside a pod.
func kubeConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// spaHandler serves the built SPA: real files as-is, everything else the
// index so client-side routes deep-link.
func spaHandler(dir string) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(dir, filepath.Clean("/"+strings.TrimPrefix(r.URL.Path, "/")))
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			fs.ServeHTTP(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(dir, "index.html"))
	})
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
