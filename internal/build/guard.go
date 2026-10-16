package build

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/thesunbg/paas/internal/netguard"
)

// sanitizeSymlinks removes every symlink under src that dangles or resolves
// outside src. Afterwards nothing under src resolves outside src, so readers
// of the checkout need no per-path containment checks. WalkDir does not follow
// symlinked directories, and the criterion is evaluated on the untouched tree,
// so one pass is enough.
func sanitizeSymlinks(src string, log Logger) error {
	base, err := filepath.EvalSymlinks(src)
	if err != nil {
		return err
	}
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		t, err := filepath.EvalSymlinks(p)
		if err == nil {
			rel, rerr := filepath.Rel(base, t)
			if rerr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return nil
			}
		}
		if err := os.Remove(p); err != nil {
			return err
		}
		log("removed symlink " + strings.TrimPrefix(p, src) + " (points outside the repository)")
		return nil
	})
}

// gitGuardArgs are the `-c` options placed before every git subcommand of a
// remote checkout: pin the connection to the vetted ip (no second DNS lookup,
// so no rebinding), refuse redirects and every protocol but https.
func gitGuardArgs(host, port string, ip net.IP) []string {
	addr := ip.String()
	if ip.To4() == nil {
		addr = "[" + addr + "]"
	}
	return []string{
		"-c", "http.curloptResolve=" + host + ":" + port + ":" + addr,
		"-c", "http.followRedirects=false",
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
	}
}

// gitLocalArgs are the equivalent for file:// repositories (AllowLocalGit).
func gitLocalArgs() []string {
	return []string{"-c", "protocol.allow=never", "-c", "protocol.file.allow=always"}
}

// vetRepo validates a repository URL and returns the git `-c` options to use.
// Remote repositories must be https, carry no credentials, and resolve only to
// public addresses.
func (b *Builder) vetRepo(ctx context.Context, repo string) ([]string, error) {
	if strings.HasPrefix(repo, "file://") || strings.HasPrefix(repo, "/") {
		if !b.AllowLocalGit {
			return nil, errors.New("local repositories are disabled (set PAAS_ALLOW_LOCAL_GIT=true)")
		}
		return gitLocalArgs(), nil
	}
	u, err := url.Parse(repo)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return nil, errors.New("repository must be an https:// URL")
	}
	if u.User != nil {
		return nil, errors.New("repository url must not contain credentials")
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = "443"
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve repository host %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("repository host %s resolves to a non-public address", host)
	}
	for _, a := range addrs {
		if !netguard.Public(a.IP) {
			return nil, fmt.Errorf("repository host %s resolves to a non-public address", host)
		}
	}
	return gitGuardArgs(host, port, addrs[0].IP), nil
}
