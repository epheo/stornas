package lvm

import (
	"os/exec"
	"strings"
	"testing"
)

// LVM prints "ignored with the devices file" on stderr for a filter that
// stornas-os ships; a parser fed that line read a thin pool as missing.
func TestExecKeepsStderrOutOfSuccess(t *testing.T) {
	args := []string{"-c", "echo '  Please remove the lvm.conf global_filter' >&2; echo '  twi-aotz--'"}
	out, err := Exec(exec.Command("sh", args...), "sh", args)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "twi-aotz--" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestExecReturnsStderrOnFailure(t *testing.T) {
	args := []string{"-c", "echo 'Volume group \"x\" not found' >&2; exit 5"}
	out, err := Exec(exec.Command("sh", args...), "sh", args)
	if err == nil || !strings.Contains(string(out), "not found") || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
}
