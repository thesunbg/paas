package server

import (
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/thesunbg/paas/internal/store"
)

var gitUserRe = regexp.MustCompile(`^[A-Za-z0-9._@+-]{0,100}$`)

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// GET /api/v1/teams/{team}/git-credentials (tokens are never returned)
func (s *Server) handleListGitCredentials(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")
	if !s.requireTeam(w, r, team, "viewer") {
		return
	}
	creds, err := s.store.GitCredentials(r.Context(), team)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"credentials": creds, "providers": gitProviders})
}

// POST /api/v1/teams/{team}/git-credentials {provider, host?, username?, token, name?}
// Stores the team's credential for a host, replacing the one it had.
func (s *Server) handleSetGitCredential(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")
	if !s.requireTeam(w, r, team, "admin") {
		return
	}
	var in struct {
		Provider string `json:"provider"`
		Host     string `json:"host"`
		Username string `json:"username"`
		Token    string `json:"token"`
		Name     string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	c, err := validateGitCredential(in.Provider, in.Host, in.Username, in.Token, in.Name)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	enc, err := s.box.Encrypt(c.TokenEnc)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	u := currentUser(r)
	c.TeamID, c.TokenEnc, c.CreatedBy = team, enc, &u.ID
	out, err := s.store.UpsertGitCredential(r.Context(), c)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	s.store.Audit(r.Context(), &u.ID, &team, "git_credential.set", out.Provider+" "+out.Host)
	writeJSON(w, 201, out)
}

// validateGitCredential checks API input and returns the credential with the
// plaintext token in TokenEnc (the caller encrypts it).
func validateGitCredential(provider, host, username, token, name string) (*store.GitCredential, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if !slices.Contains(gitProviders, provider) {
		return nil, errors.New("provider must be github, gitlab, gitea or bitbucket")
	}
	if strings.TrimSpace(host) == "" {
		host = defaultGitHosts[provider]
		if host == "" {
			return nil, errors.New("host is required for " + provider)
		}
	}
	h, err := normalizeGitHost(host)
	if err != nil {
		return nil, err
	}
	if p, ok := knownGitHosts[h]; ok && p != provider {
		return nil, errors.New(h + " is a " + p + " host")
	}
	if provider == providerBitbucket && h != "bitbucket.org" {
		return nil, errors.New("only Bitbucket Cloud (bitbucket.org) is supported")
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 4096 || strings.ContainsFunc(token, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return nil, errors.New("token is required (no spaces)")
	}
	username = strings.TrimSpace(username)
	if !gitUserRe.MatchString(username) {
		return nil, errors.New("invalid username")
	}
	name = strings.TrimSpace(name)
	if len(name) > 100 {
		return nil, errors.New("name is too long")
	}
	return &store.GitCredential{Provider: provider, Host: h, Username: username, TokenEnc: token, Name: firstNonEmpty(name, h)}, nil
}

// DELETE /api/v1/teams/{team}/git-credentials/{credential}
func (s *Server) handleDeleteGitCredential(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")
	if !s.requireTeam(w, r, team, "admin") {
		return
	}
	if !uuidRe.MatchString(r.PathValue("credential")) {
		writeError(w, 404, "credential not found")
		return
	}
	err := s.store.DeleteGitCredential(r.Context(), team, r.PathValue("credential"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, 404, "credential not found")
		return
	}
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, &team, "git_credential.delete", r.PathValue("credential"))
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}
