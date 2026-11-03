package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrNoSudo: the login user is not root and the machine has no sudo.
var ErrNoSudo = errors.New("the user is not root and sudo is not installed: log in as root, or install sudo first")

// Host runs commands on a machine as the login user or as root, through sudo when the
// login user is not root.
type Host struct {
	R    Runner
	User string
	// Password is the login password, used for sudo -S when the user is not root.
	// It is written to sudo's stdin, never put on a command line.
	Password string

	root bool
	sudo string // "" (root), "nopasswd" or "password"
}

// Root reports whether the login user is root (valid after Privileges).
func (h *Host) Root() bool { return h.root }

// Privileges finds out how to become root: directly, sudo without a password, or sudo
// with the login password. Its error explains what is missing.
func (h *Host) Privileges(ctx context.Context) error {
	out, err := Output(ctx, h.R, "id -u", nil)
	if err != nil {
		return fmt.Errorf("id -u: %w: %s", err, strings.TrimSpace(out))
	}
	if strings.TrimSpace(out) == "0" {
		h.root = true
		return nil
	}
	if _, err := Output(ctx, h.R, "command -v sudo", nil); err != nil {
		var ee *ExitError
		if errors.As(err, &ee) {
			return ErrNoSudo
		}
		return err
	}
	if _, err := Output(ctx, h.R, "sudo -n true", nil); err == nil {
		h.sudo = "nopasswd"
		return nil
	} else if ctx.Err() != nil {
		return err
	}
	if h.Password == "" {
		return fmt.Errorf("sudo needs a password for %s: use password authentication, or allow it NOPASSWD in sudoers", h.User)
	}
	if out, err := Output(ctx, h.R, "sudo -S -p '' true", strings.NewReader(h.Password+"\n")); err != nil {
		if ctx.Err() != nil {
			return err
		}
		return fmt.Errorf("sudo refused %s (not in sudoers, or wrong password): %s", h.User, MaskText(strings.TrimSpace(out), []string{h.Password}))
	}
	h.sudo = "password"
	return nil
}

// RootCommand wraps a shell command so it runs as root, and returns the stdin to give it.
func (h *Host) RootCommand(cmd string) (string, io.Reader) {
	switch {
	case h.root:
		return "sh -c " + Quote(cmd), nil
	case h.sudo == "password":
		// sudo reads the first line of stdin as the password; the command sees the rest (none).
		return "sudo -S -p '' sh -c " + Quote(cmd), strings.NewReader(h.Password + "\n")
	default:
		return "sudo -n sh -c " + Quote(cmd), nil
	}
}

// RunRoot runs cmd as root.
func (h *Host) RunRoot(ctx context.Context, cmd string, out io.Writer) error {
	c, stdin := h.RootCommand(cmd)
	return h.R.Run(ctx, c, stdin, out)
}
