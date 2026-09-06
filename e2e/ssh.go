//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os/exec"
)

// sshBaseArgs configures the runner's system SSH client.
func sshBaseArgs(t Target) []string {
	return []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
		"-i", t.SSHKeyPath,
	}
}

// Run executes remoteCmd on t's guest OS and returns its combined output.
func (t Target) Run(ctx context.Context, remoteCmd string) (string, error) {
	args := append(sshBaseArgs(t), fmt.Sprintf("%s@%s", t.SSHUser, t.SSHHost), remoteCmd)
	out, err := exec.CommandContext(ctx, "ssh", args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("e2e: ssh %s@%s %q: %w: %s", t.SSHUser, t.SSHHost, remoteCmd, err, out)
	}
	return string(out), nil
}

// RunBackground detaches a long-running remote command from the SSH session.
func (t Target) RunBackground(ctx context.Context, remoteCmd string) error {
	// The scenario stops detached processes explicitly during cleanup.
	wrapped := fmt.Sprintf("nohup %s >/tmp/e2e-%s.log 2>&1 & disown", remoteCmd, sanitizeLogName(remoteCmd))
	_, err := t.Run(ctx, wrapped)
	return err
}

func sanitizeLogName(cmd string) string {
	out := make([]rune, 0, len(cmd))
	for _, r := range cmd {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out = append(out, r)
		default:
			if len(out) > 0 && out[len(out)-1] != '-' {
				out = append(out, '-')
			}
		}
		if len(out) >= 32 {
			break
		}
	}
	return string(out)
}

// Copy uploads localPath to remotePath on t's guest OS.
func (t Target) Copy(ctx context.Context, localPath, remotePath string) error {
	dest := fmt.Sprintf("%s@%s:%s", t.SSHUser, t.SSHHost, remotePath)
	args := append(sshBaseArgs(t), localPath, dest)
	out, err := exec.CommandContext(ctx, "scp", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("e2e: scp %s -> %s: %w: %s", localPath, dest, err, out)
	}
	return nil
}
