package remote

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// CheckKnownHosts looks addr's host key up in OpenSSH known_hosts files. known is true
// when a file lists this very key; an error means a file lists a different key for the
// host (refuse to connect). Missing files count as "not listed".
func CheckKnownHosts(files []string, addr string, key ssh.PublicKey) (known bool, err error) {
	var existing []string
	for _, f := range files {
		if _, err := os.Stat(f); err == nil {
			existing = append(existing, f)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
	}
	if len(existing) == 0 {
		return false, nil
	}
	cb, err := knownhosts.New(existing...)
	if err != nil {
		return false, fmt.Errorf("known_hosts: %w", err)
	}
	var remote net.Addr = &net.TCPAddr{IP: net.IPv4zero}
	if host, port, err := net.SplitHostPort(addr); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			p := 0
			_, _ = fmt.Sscanf(port, "%d", &p)
			remote = &net.TCPAddr{IP: ip, Port: p}
		}
	}
	err = cb(addr, remote, key)
	if err == nil {
		return true, nil
	}
	var ke *knownhosts.KeyError
	if errors.As(err, &ke) {
		if len(ke.Want) == 0 {
			return false, nil
		}
		var want []string
		for _, k := range ke.Want {
			want = append(want, fmt.Sprintf("%s:%d %s", k.Filename, k.Line, Fingerprint(k.Key)))
		}
		return false, fmt.Errorf("%w (known_hosts has %s)", &HostKeyMismatchError{Want: Fingerprint(ke.Want[0].Key), Got: Fingerprint(key)}, strings.Join(want, ", "))
	}
	var re *knownhosts.RevokedError
	if errors.As(err, &re) {
		return false, fmt.Errorf("the host key %s is revoked in %s", Fingerprint(key), re.Revoked.Filename)
	}
	return false, err
}

// KnownHostsLine formats a known_hosts entry for addr.
func KnownHostsLine(addr string, key ssh.PublicKey) string {
	return knownhosts.Line([]string{knownhosts.Normalize(addr)}, key)
}
