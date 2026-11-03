package update

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// PrevSuffix names the copy of the replaced binary kept for a rollback (/usr/local/bin/paasd.prev).
const PrevSuffix = ".prev"

// ErrNoPrevious: there is no previous binary to go back to.
var ErrNoPrevious = errors.New("no previous binary to roll back to")

// Install replaces exe with staged (a fully written file in the same directory, see
// Download), keeping the current exe as exe.prev. Every step is a rename or a hard
// link inside one directory, so at any instant exe is either the complete old or the
// complete new binary, also across a crash or power loss (the directory is synced).
func Install(exe, staged string) error {
	if filepath.Dir(exe) != filepath.Dir(staged) {
		return fmt.Errorf("staged binary %s must be in the directory of %s", staged, exe)
	}
	prev := exe + PrevSuffix
	tmpPrev := prev + ".tmp"
	_ = os.Remove(tmpPrev)
	// A hard link keeps the old binary under a second name without copying it; the
	// rename then puts it in place of an older .prev atomically.
	if err := os.Link(exe, tmpPrev); err != nil {
		if err := copyFile(exe, tmpPrev); err != nil {
			return fmt.Errorf("keep the previous binary: %w", err)
		}
	}
	if err := os.Rename(tmpPrev, prev); err != nil {
		_ = os.Remove(tmpPrev)
		return fmt.Errorf("keep the previous binary: %w", err)
	}
	if err := os.Rename(staged, exe); err != nil {
		return fmt.Errorf("install the new binary: %w", err)
	}
	return syncDir(filepath.Dir(exe))
}

// Rollback puts exe.prev back in place of exe (one atomic rename; the failed binary
// is discarded and there is no .prev afterwards).
func Rollback(exe string) error {
	prev := exe + PrevSuffix
	if _, err := os.Stat(prev); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNoPrevious
		}
		return err
	}
	if err := os.Rename(prev, exe); err != nil {
		return err
	}
	return syncDir(filepath.Dir(exe))
}

// HasPrevious reports whether a rollback is possible.
func HasPrevious(exe string) bool {
	_, err := os.Stat(exe + PrevSuffix)
	return err == nil
}

// Executable is the path of the running binary with symlinks resolved.
func Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
