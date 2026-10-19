package proxy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/model"
)

// Gated routes (see model.RouteGate): a ticket in the query string becomes a
// session cookie for the host; requests without one are refused.

const (
	GateTicketParam = "__paas_ticket"
	gateCookie      = "__paas_gate"
	// GateTicketTTL bounds how long a ticket from the dashboard stays usable.
	GateTicketTTL  = 2 * time.Minute
	gateSessionTTL = 8 * time.Hour
)

func gateSign(key, kind, host string, exp int64) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(kind + "|" + strings.ToLower(host) + "|" + strconv.FormatInt(exp, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

// GateTicket is what the control plane appends to a gated URL.
func GateTicket(key, host string, now time.Time) string {
	exp := now.Add(GateTicketTTL).Unix()
	return strconv.FormatInt(exp, 10) + "." + gateSign(key, "ticket", host, exp)
}

func gateValid(key, kind, host, v string, maxTTL time.Duration, now time.Time) bool {
	es, sig, ok := strings.Cut(v, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(es, 10, 64)
	if err != nil || now.Unix() > exp || time.Unix(exp, 0).Sub(now) > maxTTL+time.Minute {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(gateSign(key, kind, host, exp)))
}

// gate enforces a route's gate. It returns false when it answered the request.
func gate(w http.ResponseWriter, r *http.Request, g *model.RouteGate, host string) bool {
	now := time.Now()
	if t := r.URL.Query().Get(GateTicketParam); t != "" {
		if !gateValid(g.Key, "ticket", host, t, GateTicketTTL, now) {
			errorPage(w, http.StatusUnauthorized, "This link has expired", "Open it again from the dashboard.")
			return false
		}
		exp := now.Add(gateSessionTTL).Unix()
		http.SetCookie(w, &http.Cookie{
			Name: gateCookie, Value: strconv.FormatInt(exp, 10) + "." + gateSign(g.Key, "session", host, exp),
			Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode, MaxAge: int(gateSessionTTL.Seconds()),
		})
		q := r.URL.Query()
		q.Del(GateTicketParam)
		u := *r.URL
		u.RawQuery = q.Encode()
		u.Scheme, u.Host = "", ""
		http.Redirect(w, r, u.RequestURI(), http.StatusSeeOther)
		return false
	}
	c, err := r.Cookie(gateCookie)
	if err != nil || !gateValid(g.Key, "session", host, c.Value, gateSessionTTL, now) {
		errorPage(w, http.StatusUnauthorized, "Sign in through the dashboard", "Open this page from the dashboard of your project.")
		return false
	}
	// The upstream trusts this header; never pass on one a client sent, nor the cookie.
	r.Header.Del(g.Header)
	r.Header.Set(g.Header, g.Value)
	stripCookie(r, gateCookie)
	return true
}

func stripCookie(r *http.Request, name string) {
	cs := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cs {
		if c.Name != name {
			r.AddCookie(c)
		}
	}
}

func hostOnly(host string) string {
	host = strings.ToLower(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.TrimSuffix(host, ".")
}
