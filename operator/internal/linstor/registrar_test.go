/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package linstor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newServer(t *testing.T, getStatus int, created *map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/nodes/node-a/storage-pools/stornas", func(w http.ResponseWriter, _ *http.Request) {
		if getStatus == http.StatusOK {
			_, _ = w.Write([]byte(`{"storage_pool_name":"stornas","provider_kind":"LVM_THIN"}`))
			return
		}
		w.WriteHeader(getStatus)
	})
	mux.HandleFunc("POST /v1/nodes/node-a/storage-pools", func(w http.ResponseWriter, r *http.Request) {
		if created == nil {
			t.Error("unexpected create call")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(created); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`[]`))
	})
	return httptest.NewServer(mux)
}

func TestEnsurePoolCreatesWhenAbsent(t *testing.T) {
	var created map[string]any
	srv := newServer(t, http.StatusNotFound, &created)
	defer srv.Close()

	r, err := NewRegistrar(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.EnsurePool(context.Background(), "node-a", "stornas-tank"); err != nil {
		t.Fatal(err)
	}

	if created["storage_pool_name"] != "stornas" || created["provider_kind"] != "LVM_THIN" {
		t.Fatalf("created = %v", created)
	}
	props, _ := created["props"].(map[string]any)
	if props["StorDriver/LvmVg"] != "stornas-tank" || props["StorDriver/ThinPool"] != "thin" {
		t.Fatalf("props = %v", props)
	}
}

func TestEnsurePoolIdempotent(t *testing.T) {
	srv := newServer(t, http.StatusOK, nil)
	defer srv.Close()

	r, err := NewRegistrar(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.EnsurePool(context.Background(), "node-a", "stornas-tank"); err != nil {
		t.Fatal(err)
	}
}

func TestEnsurePoolSurfacesControllerErrors(t *testing.T) {
	srv := newServer(t, http.StatusInternalServerError, nil)
	defer srv.Close()

	r, err := NewRegistrar(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.EnsurePool(context.Background(), "node-a", "stornas-tank"); err == nil {
		t.Fatal("want error")
	}
}
