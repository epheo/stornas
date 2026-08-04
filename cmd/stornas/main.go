package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
)

var version = "dev"

func main() {
	addr := flag.String("addr", envOr("STORNAS_ADDR", ":8080"), "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /api/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"version": version})
	})

	log.Printf("stornas %s listening on %s", version, *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
