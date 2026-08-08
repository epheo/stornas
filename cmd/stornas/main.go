package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/epheo/stornas/internal/api"
	"github.com/epheo/stornas/internal/auth"
	"github.com/epheo/stornas/internal/clusterstate"
	"github.com/epheo/stornas/internal/eventbus"
	"github.com/epheo/stornas/internal/linstorpoll"
	"github.com/epheo/stornas/internal/model"
	"github.com/epheo/stornas/internal/stream"
	"github.com/epheo/stornas/internal/tasks"
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

	src := &auth.KubeSource{Dyn: dyn, CS: cs, Namespace: envOr("STORNAS_NAMESPACE", "stornas-system")}
	if pw, err := src.Bootstrap(ctx); err != nil {
		log.Printf("auth bootstrap: %v (login unavailable until a LocalUser exists)", err)
	} else if pw != "" {
		// First boot only; afterwards the password lives in the
		// admin-password Secret.
		log.Printf("initial admin password: %s", pw)
	}
	sessions := auth.NewManager(src)

	bus := eventbus.New()
	state := clusterstate.New(cs, dyn, bus)
	state.Run(ctx)

	var poller *linstorpoll.Poller
	if u := os.Getenv("LINSTOR_URL"); u != "" {
		p, err := linstorpoll.New(u, bus)
		if err != nil {
			log.Fatalf("linstor poller: %v", err)
		}
		poller = p
		go poller.Run(ctx)
	}
	feed := tasks.New(bus)
	snapFn := func() model.Snapshot {
		snap := state.Snapshot()
		poller.Decorate(&snap)
		snap.Tasks = taskModels(feed)
		return snap
	}

	kinds := []eventbus.Kind{
		eventbus.PoolChanged, eventbus.NodeChanged, eventbus.VolumeChanged,
		eventbus.ShareChanged, eventbus.TargetChanged, eventbus.SnapshotChanged,
		eventbus.AlertChanged, eventbus.TaskChanged,
	}
	wake, cancel := bus.Subscribe(kinds...)
	defer cancel()
	hub := stream.NewHub(snapFn, wake, func() uint64 { return bus.Version(kinds...) })
	go hub.Run(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /api/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"version": version, "healthy": state.Healthy()})
	})
	mux.HandleFunc("POST /api/v1/login", sessions.Login)
	mux.HandleFunc("POST /api/v1/logout", sessions.Logout)
	mux.HandleFunc("GET /api/v1/session", sessions.Session)
	mux.HandleFunc("POST /api/v1/session/password", sessions.ChangePassword)
	mux.Handle("GET /api/v1/state", sessions.Require(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, snapFn())
	})))
	mux.Handle("GET /api/v1/stream", sessions.Require(hub))

	mutate := &api.API{Dyn: dyn, CS: cs, Namespace: src.Namespace, Tasks: feed, Linstor: poller.Client()}
	admin := func(h http.HandlerFunc) http.Handler { return sessions.RequireRole("admin", h) }
	mux.Handle("POST /api/v1/pools", admin(mutate.CreatePool))
	mux.Handle("POST /api/v1/pools/{name}/replace", admin(mutate.ReplacePoolDevice))
	mux.Handle("POST /api/v1/volumes", admin(mutate.CreateVolume))
	mux.Handle("DELETE /api/v1/volumes/{name}", admin(mutate.DeleteVolume))
	mux.Handle("POST /api/v1/volumes/{name}/resize", admin(mutate.ResizeVolume))
	mux.Handle("POST /api/v1/volumes/{name}/resolve-split", admin(mutate.ResolveSplitBrain))
	mux.Handle("POST /api/v1/shares", admin(mutate.CreateShare))
	mux.Handle("DELETE /api/v1/shares/{name}", admin(mutate.DeleteShare))
	mux.Handle("POST /api/v1/targets", admin(mutate.CreateTarget))
	mux.Handle("DELETE /api/v1/targets/{name}", admin(mutate.DeleteTarget))
	mux.Handle("POST /api/v1/snapshots", admin(mutate.CreateSnapshot))
	mux.Handle("DELETE /api/v1/snapshots/{name}", admin(mutate.DeleteSnapshot))
	mux.Handle("GET /api/v1/users", admin(mutate.ListUsers))
	mux.Handle("POST /api/v1/users", admin(mutate.CreateUser))
	mux.Handle("DELETE /api/v1/users/{name}", admin(mutate.DeleteUser))
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

// taskModels maps the feed into wire form here rather than in the tasks
// package, which stays a verbatim dotvirt copy (CLAUDE.md provenance).
// Ops() is newest-first and append-ordered, so the frame is deterministic
// and the hub's byte-level dedupe holds.
func taskModels(f *tasks.Feed) []model.Task {
	ops := f.Ops()
	out := make([]model.Task, 0, len(ops))
	for _, op := range ops {
		out = append(out, model.Task{
			Verb:   op.Verb,
			Object: op.Name,
			By:     op.By,
			OK:     op.OK,
			At:     op.At.UTC().Format(time.RFC3339),
		})
	}
	return out
}
