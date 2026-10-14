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

const (
	tankProps    = `"props":{"StorDriver/StorPoolName":"stornas-tank/thin"}`
	poolHealthy  = `{"storage_pool_name":"stornas","provider_kind":"LVM_THIN","total_capacity":1048576,` + tankProps + `}`
	poolNoCap    = `{"storage_pool_name":"stornas","provider_kind":"LVM_THIN",` + tankProps + `}`
	poolSatError = `{"storage_pool_name":"stornas","provider_kind":"LVM_THIN",` + tankProps + `,` +
		`"reports":[{"ret_code":-4611686018427387904,"message":"thin pool not found"}]}`
	poolOtherVG = `{"storage_pool_name":"stornas","provider_kind":"LVM_THIN","total_capacity":1048576,` +
		`"props":{"StorDriver/StorPoolName":"stornas-other/thin"}}`
)

func newServer(t *testing.T, getStatus int, pool string, created *map[string]any) *httptest.Server {
	t.Helper()
	return newServerConn(t, "ONLINE", getStatus, pool, created)
}

func newServerConn(t *testing.T, conn string, getStatus int, pool string, created *map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/nodes/node-a", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"node-a","type":"SATELLITE","connection_status":"` + conn + `"}`))
	})
	mux.HandleFunc("GET /v1/nodes/node-a/storage-pools/stornas", func(w http.ResponseWriter, _ *http.Request) {
		if getStatus == http.StatusOK {
			_, _ = w.Write([]byte(pool))
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
	srv := newServer(t, http.StatusNotFound, "", &created)
	defer srv.Close()

	r, err := NewRegistrar(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	// A fresh create is not usable until a later pass sees capacity.
	if err := r.EnsurePool(context.Background(), "node-a", "stornas", "stornas-tank"); err == nil {
		t.Fatal("want not-ready error after create")
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
	srv := newServer(t, http.StatusOK, poolHealthy, nil)
	defer srv.Close()

	r, err := NewRegistrar(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.EnsurePool(context.Background(), "node-a", "stornas", "stornas-tank"); err != nil {
		t.Fatal(err)
	}
}

func TestEnsurePoolSurfacesControllerErrors(t *testing.T) {
	srv := newServer(t, http.StatusInternalServerError, "", nil)
	defer srv.Close()

	r, err := NewRegistrar(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.EnsurePool(context.Background(), "node-a", "stornas", "stornas-tank"); err == nil {
		t.Fatal("want error")
	}
}

func TestEnsurePoolWaitsForSatellite(t *testing.T) {
	srv := newServerConn(t, "OFFLINE", http.StatusNotFound, "", nil)
	defer srv.Close()

	r, err := NewRegistrar(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.EnsurePool(context.Background(), "node-a", "stornas", "stornas-tank"); err == nil {
		t.Fatal("want error while the satellite is disconnected")
	}
}

func TestEnsurePoolRecreatesUnusablePools(t *testing.T) {
	for name, pool := range map[string]string{
		"error report":                       poolSatError,
		"no capacity on an online satellite": poolNoCap,
	} {
		t.Run(name, func(t *testing.T) {
			srv := newServer(t, http.StatusOK, pool, nil)
			defer srv.Close()
			deleted := false
			srv.Config.Handler.(*http.ServeMux).HandleFunc("DELETE /v1/nodes/node-a/storage-pools/stornas",
				func(w http.ResponseWriter, _ *http.Request) {
					deleted = true
					_, _ = w.Write([]byte(`[]`))
				})

			r, err := NewRegistrar(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.EnsurePool(context.Background(), "node-a", "stornas", "stornas-tank"); err == nil {
				t.Fatal("want error for an unusable pool")
			}
			if !deleted {
				t.Fatal("unusable pool was not dropped for recreation")
			}
		})
	}
}

// Two pools of one tier on a node would share the LINSTOR name; the one
// that does not own the registration must neither claim nor drop it.
func TestForeignRegistrationIsLeftAlone(t *testing.T) {
	srv := newServer(t, http.StatusOK, poolOtherVG, nil)
	defer srv.Close()
	srv.Config.Handler.(*http.ServeMux).HandleFunc("DELETE /v1/nodes/node-a/storage-pools/stornas",
		func(w http.ResponseWriter, _ *http.Request) {
			t.Error("foreign registration deleted")
			_, _ = w.Write([]byte(`[]`))
		})

	r, err := NewRegistrar(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.EnsurePool(context.Background(), "node-a", "stornas", "stornas-tank"); err == nil {
		t.Fatal("want error for a registration held by another VG")
	}
	if err := r.DeletePool(context.Background(), "node-a", "stornas", "stornas-tank"); err != nil {
		t.Fatal(err)
	}
}
