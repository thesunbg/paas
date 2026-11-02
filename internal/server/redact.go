package server

import (
	"context"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/thesunbg/paas/internal/store"
)

// redactor masks secrets in text that leaves the platform's trust boundary
// (logs handed to AI agents or to a language model): the exact values of a
// service's secrets plus patterns that look like credentials anywhere.
type redactor struct {
	values []string // longest first, so a secret containing another is masked whole
}

const redacted = "[REDACTED]"

// minSecretLen keeps short values ("1", "true", "8080") from blanking out unrelated text.
const minSecretLen = 6

var redactPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// PEM private keys.
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z ]*PRIVATE KEY-----|$)`), "[REDACTED PRIVATE KEY]"},
	// Credentials in URLs: scheme://user:password@host.
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@]*:)[^@\s/]+@`), "${1}" + redacted + "@"},
	// Well-known token formats: paas API tokens, GitHub, GitLab, OpenAI/Anthropic-style keys, Slack, AWS access key ids.
	{regexp.MustCompile(`\b(paas_[A-Za-z0-9_-]{8,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,}|sk-[A-Za-z0-9_-]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16})`), redacted},
	// Authorization headers.
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`), "$1 " + redacted},
	// KEY=value assignments whose name looks secret (env dumps, query strings, CLI flags).
	{regexp.MustCompile(`(?i)\b([a-z0-9_.-]*(?:password|passwd|secret|token|api_?key|apikey|access_?key|private_?key|credentials?)[a-z0-9_.-]*\s*=\s*)("[^"]*"|'[^']*'|[^\s&;,]+)`), "${1}" + redacted},
	// "secretName": "value" in JSON and YAML-ish output.
	{regexp.MustCompile(`(?i)("[a-z0-9_.-]*(?:password|passwd|secret|token|api_?key|apikey|access_?key|private_?key)[a-z0-9_.-]*"\s*:\s*)"[^"]*"`), `${1}"` + redacted + `"`},
}

func newRedactor(values ...string) *redactor {
	r := &redactor{}
	seen := map[string]bool{}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if len(v) < minSecretLen || seen[v] {
			continue
		}
		seen[v] = true
		r.values = append(r.values, v)
		// A URL's password also appears on its own (connection errors, env dumps).
		if u, err := url.Parse(v); err == nil && u.User != nil {
			if pw, ok := u.User.Password(); ok && len(pw) >= minSecretLen && !seen[pw] {
				seen[pw] = true
				r.values = append(r.values, pw)
			}
		}
	}
	sort.Slice(r.values, func(i, j int) bool { return len(r.values[i]) > len(r.values[j]) })
	return r
}

// String masks every known secret value, then every credential-looking pattern.
func (r *redactor) String(s string) string {
	if r != nil {
		for _, v := range r.values {
			s = strings.ReplaceAll(s, v, redacted)
		}
	}
	for _, p := range redactPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

func (r *redactor) Lines(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = r.String(l)
	}
	return out
}

// serviceRedactor knows the secrets of a service: its environment values
// (including linked databases' URLs), its git token and its webhook secret.
func (s *Server) serviceRedactor(ctx context.Context, svc *store.Service) *redactor {
	var values []string
	if env, err := s.userEnv(ctx, svc); err == nil {
		for _, v := range env {
			values = append(values, v)
		}
	}
	for _, v := range svc.Spec.Env {
		values = append(values, v)
	}
	if svc.GitTokenEnc != "" {
		if tok, err := s.box.Decrypt(svc.GitTokenEnc); err == nil {
			values = append(values, tok)
		}
	}
	values = append(values, svc.WebhookSecret)
	return newRedactor(values...)
}
