package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner extends lvm.Runner with stdin support (smbpasswd reads the
// password from stdin; nothing else needs it).
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	RunInput(ctx context.Context, stdin, name string, args ...string) ([]byte, error)
}

// HostRunner executes commands in the host's namespaces via nsenter: the
// DaemonSet runs privileged with hostPID, so PID 1 is the host's init.
// Mounts, exportfs, samba, and LVM all act on the real host this way, and
// the agent image needs no storage userland of its own.
type HostRunner struct{}

func (HostRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return hostExec(ctx, "", name, args...)
}

func (HostRunner) RunInput(ctx context.Context, stdin, name string, args ...string) ([]byte, error) {
	return hostExec(ctx, stdin, name, args...)
}

func hostExec(ctx context.Context, stdin, name string, args ...string) ([]byte, error) {
	full := append([]string{"-t", "1", "-m", "-u", "-i", "-n", "--", name}, args...)
	cmd := exec.CommandContext(ctx, "nsenter", full...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
