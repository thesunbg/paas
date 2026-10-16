// Package github talks to GitHub as a GitHub App: manifest-based app creation,
// installation tokens (no personal tokens needed) and repository listing.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

var API = "https://api.github.com"

// Web is the GitHub web origin (OAuth endpoints).
var Web = "https://github.com"

// App is the configuration returned by the manifest conversion.
type App struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	HTMLURL       string `json:"html_url"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	WebhookSecret string `json:"webhook_secret"`
	PEM           string `json:"pem"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// InstallURL is where users grant the app access to repositories.
func (a *App) InstallURL(state string) string {
	return fmt.Sprintf("%s/apps/%s/installations/new?state=%s", Web, a.Slug, url.QueryEscape(state))
}

// Manifest builds the GitHub App manifest for this installation of paas.
func Manifest(name, baseURL string) map[string]any {
	return map[string]any{
		"name":         name,
		"url":          baseURL,
		"redirect_url": baseURL + "/api/v1/github/app/callback",
		"setup_url":    baseURL + "/api/v1/github/installed",
		"hook_attributes": map[string]any{
			"url":    baseURL + "/api/v1/webhooks/github-app",
			"active": true,
		},
		"public":          true, // other GitHub users must be able to sign in and install it
		"setup_on_update": true,
		// Ask the installing user to authorize too, so the callback can prove
		// they really have access to the installation they bring back.
		"request_oauth_on_install": true,
		"callback_urls":            []string{baseURL + "/api/v1/github/installed"},
		"default_permissions": map[string]string{
			"contents":      "read",
			"metadata":      "read",
			"statuses":      "write",
			"pull_requests": "write",
			"issues":        "write",
			"emails":        "read",
		},
		"default_events": []string{"push", "pull_request"},
	}
}

var httpc = &http.Client{Timeout: 20 * time.Second}

func do(ctx context.Context, method, url, auth string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		return fmt.Errorf("github %s %s: HTTP %d %s", method, url, resp.StatusCode, e.Message)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// ConvertManifest exchanges the temporary code GitHub returns after app creation.
func ConvertManifest(ctx context.Context, code string) (*App, error) {
	var app App
	if err := do(ctx, http.MethodPost, API+"/app-manifests/"+code+"/conversions", "", nil, &app); err != nil {
		return nil, err
	}
	return &app, nil
}

// Client authenticates as the app and caches installation tokens.
type Client struct {
	app *App
	key *rsa.PrivateKey

	mu     sync.Mutex
	tokens map[int64]cachedToken
}

type cachedToken struct {
	token   string
	expires time.Time
}

func NewClient(app *App) (*Client, error) {
	block, _ := pem.Decode([]byte(app.PEM))
	if block == nil {
		return nil, errors.New("invalid app private key")
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = k
	} else if k8, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := k8.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("app key is not RSA")
		}
		key = rk
	} else {
		return nil, fmt.Errorf("parse app key: %w", err)
	}
	return &Client{app: app, key: key, tokens: map[int64]cachedToken{}}, nil
}

func (c *Client) App() *App { return c.app }

// JWT returns a short-lived token that authenticates as the app itself.
func (c *Client) JWT() (string, error) {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	now := time.Now()
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-60 * time.Second).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": strconv.FormatInt(c.app.ID, 10)})
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// Installation describes where the app is installed.
type Installation struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"account"`
	RepositorySelection string `json:"repository_selection"`
	HTMLURL             string `json:"html_url"`
}

func (c *Client) GetInstallation(ctx context.Context, id int64) (*Installation, error) {
	jwt, err := c.JWT()
	if err != nil {
		return nil, err
	}
	var inst Installation
	err = do(ctx, http.MethodGet, fmt.Sprintf("%s/app/installations/%d", API, id), "Bearer "+jwt, nil, &inst)
	return &inst, err
}

// Token returns an installation access token (valid one hour, cached).
func (c *Client) Token(ctx context.Context, installationID int64) (string, error) {
	c.mu.Lock()
	if t, ok := c.tokens[installationID]; ok && time.Until(t.expires) > 5*time.Minute {
		c.mu.Unlock()
		return t.token, nil
	}
	c.mu.Unlock()
	jwt, err := c.JWT()
	if err != nil {
		return "", err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := do(ctx, http.MethodPost, fmt.Sprintf("%s/app/installations/%d/access_tokens", API, installationID), "Bearer "+jwt, nil, &out); err != nil {
		return "", err
	}
	c.mu.Lock()
	c.tokens[installationID] = cachedToken{token: out.Token, expires: out.ExpiresAt}
	c.mu.Unlock()
	return out.Token, nil
}

// Repo is a repository the app can access.
type Repo struct {
	ID            int64     `json:"id"`
	FullName      string    `json:"full_name"`
	Private       bool      `json:"private"`
	DefaultBranch string    `json:"default_branch"`
	CloneURL      string    `json:"clone_url"`
	HTMLURL       string    `json:"html_url"`
	Description   string    `json:"description"`
	Language      string    `json:"language"`
	PushedAt      time.Time `json:"pushed_at"`
}

// Repos lists the repositories an installation granted (up to 1000).
func (c *Client) Repos(ctx context.Context, installationID int64) ([]Repo, error) {
	tok, err := c.Token(ctx, installationID)
	if err != nil {
		return nil, err
	}
	var all []Repo
	for page := 1; page <= 10; page++ {
		var out struct {
			Total        int    `json:"total_count"`
			Repositories []Repo `json:"repositories"`
		}
		if err := do(ctx, http.MethodGet, fmt.Sprintf("%s/installation/repositories?per_page=100&page=%d", API, page), "token "+tok, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Repositories...)
		if len(out.Repositories) < 100 {
			break
		}
	}
	return all, nil
}

// Branches lists branch names of a repository.
func (c *Client) Branches(ctx context.Context, installationID int64, fullName string) ([]string, error) {
	tok, err := c.Token(ctx, installationID)
	if err != nil {
		return nil, err
	}
	var out []struct {
		Name string `json:"name"`
	}
	if err := do(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/branches?per_page=100", API, fullName), "token "+tok, nil, &out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out))
	for _, b := range out {
		names = append(names, b.Name)
	}
	return names, nil
}

// Post calls a repository API with the installation token (statuses, comments).
func (c *Client) Post(ctx context.Context, installationID int64, path string, body any) error {
	tok, err := c.Token(ctx, installationID)
	if err != nil {
		return err
	}
	return do(ctx, http.MethodPost, API+path, "token "+tok, body, nil)
}

// UserToken exchanges an OAuth code from a GitHub redirect for a user token.
func (c *Client) UserToken(ctx context.Context, code string) (string, error) {
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error_description"`
	}
	body := map[string]string{"client_id": c.app.ClientID, "client_secret": c.app.ClientSecret, "code": code}
	if err := do(ctx, http.MethodPost, Web+"/login/oauth/access_token", "", body, &tok); err != nil {
		return "", err
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("github oauth: %s", firstNonEmpty(tok.Error, "no access token"))
	}
	return tok.AccessToken, nil
}

// UserInstallationIDs returns the installations of this app the user token can access.
func (c *Client) UserInstallationIDs(ctx context.Context, userToken string) (map[int64]bool, error) {
	var res struct {
		Installations []Installation `json:"installations"`
	}
	if err := do(ctx, http.MethodGet, API+"/user/installations?per_page=100", "token "+userToken, nil, &res); err != nil {
		return nil, err
	}
	ids := map[int64]bool{}
	for _, in := range res.Installations {
		ids[in.ID] = true
	}
	return ids, nil
}

// User is the GitHub account behind a user token.
type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Name  string `json:"name"`
	// Email is verified (GitHub only shows verified addresses as public or primary).
	Email string `json:"email"`
}

// CurrentUser returns the account and its verified primary email when visible.
func (c *Client) CurrentUser(ctx context.Context, userToken string) (*User, error) {
	var u User
	if err := do(ctx, http.MethodGet, API+"/user", "token "+userToken, nil, &u); err != nil {
		return nil, err
	}
	// Needs the "Email addresses" account permission; fall back to the public email.
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := do(ctx, http.MethodGet, API+"/user/emails", "token "+userToken, nil, &emails); err == nil {
		for _, e := range emails {
			if e.Primary && e.Verified {
				u.Email = e.Email
			}
		}
	}
	return &u, nil
}

// AuthorizeURL starts "Sign in with GitHub"; GitHub returns to the app's callback URL.
func (c *Client) AuthorizeURL(state string) string {
	return fmt.Sprintf("%s/login/oauth/authorize?client_id=%s&state=%s", Web, url.QueryEscape(c.app.ClientID), url.QueryEscape(state))
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// SetHookURL points the app webhook at url (when the server's public URL changes).
func (c *Client) SetHookURL(ctx context.Context, url string) error {
	jwt, err := c.JWT()
	if err != nil {
		return err
	}
	var cur struct {
		URL string `json:"url"`
	}
	if err := do(ctx, http.MethodGet, API+"/app/hook/config", "Bearer "+jwt, nil, &cur); err != nil {
		return err
	}
	if cur.URL == url {
		return nil
	}
	return do(ctx, http.MethodPatch, API+"/app/hook/config", "Bearer "+jwt, map[string]string{"url": url, "content_type": "json"}, nil)
}
