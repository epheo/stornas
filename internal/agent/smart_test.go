package agent

import (
	"context"
	"testing"
)

func TestCheckSmart(t *testing.T) {
	healthy := `{"smart_status":{"passed":true},"temperature":{"current":34},"power_on_time":{"hours":18262}}`
	failing := `{"smart_status":{"passed":false},"temperature":{"current":51}}`
	cases := []struct {
		name    string
		out     string
		err     error
		verdict string
		temp    int32 // -1 = nil expected
		hours   int64 // -1 = nil expected
	}{
		{"healthy ata", healthy, nil, "Passed", 34, 18262},
		// smartctl exits nonzero for a failing disk; output still counts.
		{"failing disk nonzero exit", failing, errExit, "Failed", 51, -1},
		{"standby: no status block", `{"json_format_version":[1,0]}`, errExit, "Unknown", -1, -1},
		{"garbage output", "not json", errExit, "Unknown", -1, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeRunner{results: map[string]result{
				"smartctl --json=c -H -A -n standby /dev/sda": {out: c.out, err: c.err},
			}}
			info := CheckSmart(context.Background(), f, "/dev/sda")
			if info.Verdict != c.verdict {
				t.Fatalf("verdict = %s, want %s", info.Verdict, c.verdict)
			}
			if c.temp == -1 && info.TempCelsius != nil {
				t.Fatalf("temp = %d, want nil", *info.TempCelsius)
			}
			if c.temp != -1 && (info.TempCelsius == nil || *info.TempCelsius != c.temp) {
				t.Fatalf("temp = %v, want %d", info.TempCelsius, c.temp)
			}
			if c.hours == -1 && info.PowerOnHours != nil {
				t.Fatalf("hours = %d, want nil", *info.PowerOnHours)
			}
			if c.hours != -1 && (info.PowerOnHours == nil || *info.PowerOnHours != c.hours) {
				t.Fatalf("hours = %v, want %d", info.PowerOnHours, c.hours)
			}
		})
	}
}
