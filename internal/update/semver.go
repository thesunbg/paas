// Package update upgrades paas itself: it finds the latest release, downloads
// and verifies binaries, swaps the running executable (keeping the previous one
// for a rollback) and drives the rolling upgrade of a cluster (rollout.go).
package update

import (
	"regexp"
	"strconv"
	"strings"
)

// Version is a semantic version (https://semver.org). Build metadata is dropped.
type Version struct {
	Major, Minor, Patch int
	Pre                 string // pre-release, e.g. "rc.1"; empty for a release
}

// gitDescribe matches what `git describe --tags --dirty` adds to a tag for a commit
// after it ("-12-gdeadbee") or for uncommitted changes ("-dirty").
var gitDescribe = regexp.MustCompile(`(-\d+-g[0-9a-f]{4,40})?(-dirty)?$`)

// preRe: dot-separated alphanumeric identifiers (a version also names a directory).
var preRe = regexp.MustCompile(`^[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*$`)

// Parse reads "v1.2.3", "1.2.3" or "v1.2.3-rc.1" (an optional "+build" suffix is ignored).
func Parse(s string) (Version, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	s, _, _ = strings.Cut(s, "+")
	core, pre, hasPre := strings.Cut(s, "-")
	if hasPre && !preRe.MatchString(pre) {
		return Version{}, false
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	var n [3]int
	for i, p := range parts {
		if p == "" || (len(p) > 1 && p[0] == '0') {
			return Version{}, false
		}
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 {
			return Version{}, false
		}
		n[i] = v
	}
	return Version{Major: n[0], Minor: n[1], Patch: n[2], Pre: pre}, true
}

func (v Version) String() string {
	s := "v" + strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}

// IsDev reports a development build: no version at all, "dev", a bare commit, or a
// `git describe` of a commit after a tag or of a dirty tree. Such a build is never
// told that a release is newer: nobody knows what it contains.
func IsDev(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || s == "dev" {
		return true
	}
	if m := gitDescribe.FindString(s); m != "" {
		return true
	}
	_, ok := Parse(s)
	return !ok
}

// Compare orders two versions by semver precedence: -1, 0 or 1.
func Compare(a, b Version) int {
	for _, d := range [3]int{a.Major - b.Major, a.Minor - b.Minor, a.Patch - b.Patch} {
		if d != 0 {
			return sign(d)
		}
	}
	return comparePre(a.Pre, b.Pre)
}

// comparePre: a release is newer than any of its pre-releases; identifiers compare
// numerically when both are numbers, else as strings, numbers first; a longer list wins a tie.
func comparePre(a, b string) int {
	if a == b {
		return 0
	}
	if a == "" {
		return 1
	}
	if b == "" {
		return -1
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, xerr := strconv.Atoi(as[i])
		y, yerr := strconv.Atoi(bs[i])
		switch {
		case xerr == nil && yerr == nil:
			if x != y {
				return sign(x - y)
			}
		case xerr == nil:
			return -1
		case yerr == nil:
			return 1
		default:
			if c := strings.Compare(as[i], bs[i]); c != 0 {
				return c
			}
		}
	}
	return sign(len(as) - len(bs))
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// Newer reports whether latest is a newer release than current. Always false for a
// development build or an unparsable version.
func Newer(latest, current string) bool {
	if IsDev(current) {
		return false
	}
	l, ok1 := Parse(latest)
	c, ok2 := Parse(current)
	return ok1 && ok2 && Compare(l, c) > 0
}

// Same reports whether a and b name the same version ("v1.2.3" and "1.2.3" do).
// Development builds are only the same as the identical string.
func Same(a, b string) bool {
	if a == b {
		return a != ""
	}
	if IsDev(a) || IsDev(b) {
		return false
	}
	x, ok1 := Parse(a)
	y, ok2 := Parse(b)
	return ok1 && ok2 && Compare(x, y) == 0
}
