package linstorpoll

import "testing"

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
