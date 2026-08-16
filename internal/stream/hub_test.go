package stream

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/epheo/stornas/internal/eventbus"
	"github.com/epheo/stornas/internal/model"
)

func TestHubDeliversFirstFrameAndUpdates(t *testing.T) {
	var health atomic.Value
	health.Store("Online")
	snapshot := func() model.Snapshot {
		return model.Snapshot{Pools: []model.Pool{{Name: "tank", Health: health.Load().(string)}}}
	}

	bus := eventbus.New()
	wake, cancel := bus.Subscribe(eventbus.PoolChanged)
	defer cancel()
	hub := NewHub(snapshot, wake, func() uint64 { return bus.Version(eventbus.PoolChanged) })

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go hub.Run(ctx)

	srv := httptest.NewServer(hub)
	defer srv.Close()

	ws, _, err := websocket.DefaultDialer.Dial(strings.Replace(srv.URL, "http", "ws", 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Close() }()

	read := func() model.Snapshot {
		t.Helper()
		if err := ws.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		_, js, err := ws.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var f frame
		if err := json.Unmarshal(js, &f); err != nil {
			t.Fatal(err)
		}
		return f.Snapshot
	}

	// A fresh connection is served a frame without any bus activity.
	if got := read(); got.Pools[0].Health != "Online" {
		t.Fatalf("first frame = %+v", got)
	}

	health.Store("Degraded")
	bus.Publish(eventbus.PoolChanged)
	if got := read(); got.Pools[0].Health != "Degraded" {
		t.Fatalf("update frame = %+v", got)
	}
}

// An idle appliance wakes the bus every minute; with no browser open the
// hub must not pay for a snapshot build and marshal each time.
func TestHubSkipsBuildWithoutClients(t *testing.T) {
	var builds atomic.Int64
	snapshot := func() model.Snapshot {
		builds.Add(1)
		return model.Snapshot{}
	}

	bus := eventbus.New()
	wake, cancel := bus.Subscribe(eventbus.PoolChanged)
	defer cancel()
	hub := NewHub(snapshot, wake, func() uint64 { return bus.Version(eventbus.PoolChanged) })

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go hub.Run(ctx)

	for range 5 {
		bus.Publish(eventbus.PoolChanged)
	}
	time.Sleep(100 * time.Millisecond)
	if n := builds.Load(); n != 0 {
		t.Fatalf("built %d frames with zero clients", n)
	}

	// The first client still gets a frame: attach forces the rebuild.
	srv := httptest.NewServer(hub)
	defer srv.Close()
	ws, _, err := websocket.DefaultDialer.Dial(strings.Replace(srv.URL, "http", "ws", 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Close() }()
	if err := ws.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ws.ReadMessage(); err != nil {
		t.Fatal(err)
	}
}
