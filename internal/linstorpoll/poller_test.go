package linstorpoll

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/epheo/stornas/internal/eventbus"
	"github.com/epheo/stornas/internal/model"
)

func TestPollAndDecorate(t *testing.T) {
	body := `[
	  {"name":"pvc-1","node_name":"node-a","state":{"in_use":true},"volumes":[{"state":{"disk_state":"UpToDate"}}]},
	  {"name":"pvc-1","node_name":"node-b","state":{"in_use":false},"volumes":[{"state":{"disk_state":"SyncTarget"}}]}
	]`
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/view/resources", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	bus := eventbus.New()
	wake, cancel := bus.Subscribe(eventbus.VolumeChanged)
	defer cancel()

	p, err := New(srv.URL, bus)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("first poll must publish VolumeChanged")
	}

	snap := model.Snapshot{Volumes: []model.Volume{
		{Name: "disk0", Resource: "pvc-1"},
		{Name: "other", Resource: "pvc-9"},
	}}
	p.Decorate(&snap)
	rep := snap.Volumes[0].Replication
	if rep == nil || len(rep.Replicas) != 2 || rep.Replicas[1].DiskState != "SyncTarget" || !rep.Replicas[0].InUse {
		t.Fatalf("replication = %+v", rep)
	}
	if snap.Volumes[1].Replication != nil {
		t.Fatal("unrelated volume decorated")
	}

	// Unchanged view publishes nothing.
	if err := p.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
		t.Fatal("unchanged poll published")
	default:
	}
}
