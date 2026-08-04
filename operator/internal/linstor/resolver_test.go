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
	node, dev, err := viewServer(t, body).ResolvePlacement(context.Background(), "pvc-1")
	if err != nil {
		t.Fatal(err)
	}
	if node != "node-b" || dev != "/dev/drbd1000" {
		t.Fatalf("placement = %s %s", node, dev)
	}
}

func TestResolvePlacementSkipsDiskless(t *testing.T) {
	body := `[
	  {"name":"pvc-1","node_name":"node-c","flags":["DISKLESS"],"volumes":[{"device_path":"/dev/drbd1000"}]},
	  {"name":"pvc-1","node_name":"node-a","state":{"in_use":false},"volumes":[{"device_path":"/dev/drbd1000"}]}
	]`
	node, _, err := viewServer(t, body).ResolvePlacement(context.Background(), "pvc-1")
	if err != nil {
		t.Fatal(err)
	}
	if node != "node-a" {
		t.Fatalf("node = %s", node)
	}
}

func TestResolvePlacementNoReplica(t *testing.T) {
	if _, _, err := viewServer(t, `[]`).ResolvePlacement(context.Background(), "pvc-1"); err == nil {
		t.Fatal("want error")
	}
}
