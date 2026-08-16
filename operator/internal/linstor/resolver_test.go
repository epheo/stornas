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
	"net/http"
	"net/http/httptest"
	"testing"
)

func viewServer(t *testing.T, body string) *Registrar {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/view/resources", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	r, err := NewRegistrar(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestResolvePlacementPrefersPrimary(t *testing.T) {
	body := `[
	  {"name":"pvc-1","node_name":"node-a","state":{"in_use":false},"volumes":[{"device_path":"/dev/drbd1000"}]},
	  {"name":"pvc-1","node_name":"node-b","state":{"in_use":true},"volumes":[{"device_path":"/dev/drbd1000"}]},
	  {"name":"pvc-2","node_name":"node-a","state":{"in_use":true},"volumes":[{"device_path":"/dev/drbd1001"}]}
	]`
	node, dev, replicas, err := viewServer(t, body).ResolvePlacement(context.Background(), "pvc-1", "node-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if node != "node-b" || dev != "/dev/drbd1000" {
		t.Fatalf("placement = %s %s", node, dev)
	}
	if replicas != 2 {
		t.Fatalf("replicas = %d", replicas)
	}
}

func TestResolvePlacementSkipsDiskless(t *testing.T) {
	body := `[
	  {"name":"pvc-1","node_name":"node-c","flags":["DISKLESS"],"volumes":[{"device_path":"/dev/drbd1000"}]},
	  {"name":"pvc-1","node_name":"node-a","state":{"in_use":false},"volumes":[{"device_path":"/dev/drbd1000"}]}
	]`
	node, _, replicas, err := viewServer(t, body).ResolvePlacement(context.Background(), "pvc-1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if node != "node-a" {
		t.Fatalf("node = %s", node)
	}
	if replicas != 1 {
		t.Fatalf("replicas = %d", replicas)
	}
}

func TestResolvePlacementNoReplica(t *testing.T) {
	if _, _, _, err := viewServer(t, `[]`).ResolvePlacement(context.Background(), "pvc-1", "", nil); err == nil {
		t.Fatal("want error")
	}
}

// Failover: a primary on an avoided node must not win; the surviving
// diskful replica takes over.
func TestResolvePlacementAvoidsDeadPrimary(t *testing.T) {
	body := `[
	  {"name":"pvc-1","node_name":"node-a","state":{"in_use":true},"volumes":[{"device_path":"/dev/drbd1000"}]},
	  {"name":"pvc-1","node_name":"node-b","state":{"in_use":false},"volumes":[{"device_path":"/dev/drbd1000"}]}
	]`
	node, _, replicas, err := viewServer(t, body).ResolvePlacement(context.Background(), "pvc-1", "node-a", map[string]bool{"node-a": true})
	if err != nil {
		t.Fatal(err)
	}
	if node != "node-b" {
		t.Fatalf("node = %s", node)
	}
	if replicas != 2 {
		t.Fatalf("replicas = %d", replicas)
	}
}

// No primary and no preference signal: placement sticks to the current
// node instead of flapping to whichever replica lists first.
func TestResolvePlacementSticksToCurrent(t *testing.T) {
	body := `[
	  {"name":"pvc-1","node_name":"node-a","state":{"in_use":false},"volumes":[{"device_path":"/dev/drbd1000"}]},
	  {"name":"pvc-1","node_name":"node-b","state":{"in_use":false},"volumes":[{"device_path":"/dev/drbd1000"}]}
	]`
	node, _, _, err := viewServer(t, body).ResolvePlacement(context.Background(), "pvc-1", "node-b", nil)
	if err != nil {
		t.Fatal(err)
	}
	if node != "node-b" {
		t.Fatalf("node = %s", node)
	}
}

func TestResolvePlacementAllReplicasAvoided(t *testing.T) {
	body := `[
	  {"name":"pvc-1","node_name":"node-a","state":{"in_use":false},"volumes":[{"device_path":"/dev/drbd1000"}]}
	]`
	avoid := map[string]bool{"node-a": true}
	if _, _, _, err := viewServer(t, body).ResolvePlacement(context.Background(), "pvc-1", "", avoid); err == nil {
		t.Fatal("want error when every replica is unhealthy")
	}
}

// One reconcile burst (a target's LUNs, a sweep) must cost one view
// fetch, not one per placement.
func TestResolvePlacementReusesFreshView(t *testing.T) {
	fetches := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/view/resources", func(w http.ResponseWriter, _ *http.Request) {
		fetches++
		_, _ = w.Write([]byte(`[
		  {"name":"pvc-1","node_name":"node-a","state":{"in_use":true},"volumes":[{"device_path":"/dev/drbd1000"}]}
		]`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	r, err := NewRegistrar(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, _, _, err := r.ResolvePlacement(context.Background(), "pvc-1", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	if fetches != 1 {
		t.Fatalf("fetches = %d, want 1", fetches)
	}
}
