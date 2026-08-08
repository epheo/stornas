package linstorpoll

import (
	"testing"

	"github.com/epheo/stornas/internal/model"
)

func TestDetectSplitBrain(t *testing.T) {
	up := func(peers ...model.Peer) model.Replica {
		return model.Replica{DiskState: "UpToDate", Peers: peers}
	}
	cases := []struct {
		name     string
		replicas []model.Replica
		want     bool
	}{
		{"healthy pair", []model.Replica{
			up(model.Peer{Node: "b", Connected: true, Status: "Connected"}),
			up(model.Peer{Node: "a", Connected: true, Status: "Connected"}),
		}, false},
		{"peer down: Connecting is not split brain", []model.Replica{
			up(model.Peer{Node: "b", Connected: false, Status: "Connecting"}),
			up(model.Peer{Node: "a", Connected: false, Status: "Connecting"}),
		}, false},
		{"standalone after refused reconnect", []model.Replica{
			up(model.Peer{Node: "b", Connected: false, Status: "StandAlone"}),
			up(model.Peer{Node: "a", Connected: false, Status: "StandAlone"}),
		}, true},
		{"single replica cannot split", []model.Replica{
			up(model.Peer{Node: "b", Connected: false, Status: "StandAlone"}),
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := detectSplitBrain(c.replicas); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestSplitSyncPercent(t *testing.T) {
	cases := []struct {
		in    string
		state string
		pct   int // -1 means nil expected
	}{
		{"UpToDate", "UpToDate", -1},
		{"SyncTarget(43.21%)", "SyncTarget", 43},
		{"SyncTarget(99.87%)", "SyncTarget", 100},
		{"SyncSource(0.00%)", "SyncSource", 0},
		{"Inconsistent", "Inconsistent", -1},
		{"Weird(abc%)", "Weird(abc%)", -1},
		{"", "", -1},
	}
	for _, c := range cases {
		state, pct := splitSyncPercent(c.in)
		if state != c.state {
			t.Errorf("%q: state %q, want %q", c.in, state, c.state)
		}
		if c.pct == -1 {
			if pct != nil {
				t.Errorf("%q: pct %d, want nil", c.in, *pct)
			}
		} else if pct == nil || *pct != c.pct {
			t.Errorf("%q: pct %v, want %d", c.in, pct, c.pct)
		}
	}
}
