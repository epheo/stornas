package agent

import (
	"reflect"
	"testing"

	"github.com/epheo/stornas/internal/lvm"
)

func TestPlanDevices(t *testing.T) {
	wwn := "/dev/disk/by-id/wwn-0xdead"
	cases := []struct {
		name     string
		spec     []string
		resolved map[string]string
		pvs      []lvm.PV
		want     devicePlan
	}{
		{
			name:     "steady state",
			spec:     []string{wwn},
			resolved: map[string]string{wwn: "/dev/sda"},
			pvs:      []lvm.PV{{Name: "/dev/sda", UsedBytes: 10}},
			want:     devicePlan{},
		},
		{
			name:     "dead member swapped for new disk",
			spec:     []string{wwn, "/dev/sdc"},
			resolved: map[string]string{wwn: "/dev/sda", "/dev/sdc": "/dev/sdc"},
			pvs: []lvm.PV{
				{Name: "/dev/sda", UsedBytes: 10},
				{Name: "[unknown]", Missing: true},
			},
			want: devicePlan{Add: []string{"/dev/sdc"}, Missing: true},
		},
		{
			name:     "live swap: old member evacuates",
			spec:     []string{"/dev/sdc"},
			resolved: map[string]string{"/dev/sdc": "/dev/sdc"},
			pvs:      []lvm.PV{{Name: "/dev/sdb", UsedBytes: 10}},
			want:     devicePlan{Add: []string{"/dev/sdc"}, Evacuate: []string{"/dev/sdb"}},
		},
		{
			name:     "evacuated member drops",
			spec:     []string{"/dev/sdc"},
			resolved: map[string]string{"/dev/sdc": "/dev/sdc"},
			pvs: []lvm.PV{
				{Name: "/dev/sdb", UsedBytes: 0},
				{Name: "/dev/sdc", UsedBytes: 10},
			},
			want: devicePlan{Drop: []string{"/dev/sdb"}},
		},
		{
			name: "unresolvable spec path compares as itself",
			spec: []string{"/dev/sdd"},
			pvs:  []lvm.PV{{Name: "/dev/sdd", UsedBytes: 5}},
			want: devicePlan{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := planDevices(c.spec, c.resolved, c.pvs)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}
