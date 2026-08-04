// Package linstorpoll mirrors LINSTOR's resource view into the snapshot:
// replica disk states and primaries per volume. LINSTOR has no watch API,
// so this is the one polled source; the bus turns each observed change
// into a normal level-triggered rebuild.
package linstorpoll

import (
	"context"
	"net/url"
	"reflect"
	"sync"
	"time"

	lapi "github.com/LINBIT/golinstor/client"

	"github.com/epheo/stornas/internal/eventbus"
	"github.com/epheo/stornas/internal/model"
)

type Poller struct {
	client *lapi.Client
	bus    *eventbus.Bus

	mu   sync.Mutex
	data map[string]*model.Replication
}

const pollInterval = 30 * time.Second

func New(controllerURL string, bus *eventbus.Bus) (*Poller, error) {
	u, err := url.Parse(controllerURL)
	if err != nil {
		return nil, err
	}
	c, err := lapi.NewClient(lapi.BaseURL(u))
	if err != nil {
		return nil, err
	}
	return &Poller{client: c, bus: bus, data: map[string]*model.Replication{}}, nil
}

// Run polls until ctx ends; a failed poll keeps the last-good view.
func (p *Poller) Run(ctx context.Context) {
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		_ = p.poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (p *Poller) poll(ctx context.Context) error {
	view, err := p.client.Resources.GetResourceView(ctx)
	if err != nil {
		return err
	}
	next := map[string]*model.Replication{}
	for _, res := range view {
		if len(res.Volumes) == 0 {
			continue
		}
		rep := next[res.Name]
		if rep == nil {
			rep = &model.Replication{}
			next[res.Name] = rep
		}
		replica := model.Replica{Node: res.NodeName, DiskState: res.Volumes[0].State.DiskState}
		if res.State != nil && res.State.InUse != nil {
			replica.InUse = *res.State.InUse
		}
		rep.Replicas = append(rep.Replicas, replica)
	}

	p.mu.Lock()
	changed := !reflect.DeepEqual(p.data, next)
	p.data = next
	p.mu.Unlock()
	if changed {
		p.bus.Publish(eventbus.VolumeChanged)
	}
	return nil
}

// Decorate attaches replication state to volumes by their PV name (the
// LINSTOR resource name for CSI volumes). Nil-safe for a nil Poller so
// main wires one code path whether LINSTOR is configured or not.
func (p *Poller) Decorate(snap *model.Snapshot) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range snap.Volumes {
		if rep, ok := p.data[snap.Volumes[i].Resource]; ok {
			snap.Volumes[i].Replication = rep
		}
	}
}
