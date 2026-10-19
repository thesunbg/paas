package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/model"
)

func TestGate(t *testing.T) {
	g := &model.RouteGate{Key: "k3y", Header: "X-Dbweb-Token", Value: "secret"}
	host := "studio.example.com"

	// No ticket, no cookie: refused.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "http://"+host+"/", nil)
	if gate(w, r, g, host) || w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous request passed: %d", w.Code)
	}

	// A ticket for another host is refused.
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "http://"+host+"/?"+GateTicketParam+"="+GateTicket(g.Key, "other.example.com", time.Now()), nil)
	if gate(w, r, g, host) || w.Code != http.StatusUnauthorized {
		t.Fatal("ticket for another host accepted")
	}

	// A valid ticket sets the cookie and redirects without the ticket.
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "http://"+host+"/app?x=1&"+GateTicketParam+"="+GateTicket(g.Key, host, time.Now()), nil)
	if gate(w, r, g, host) || w.Code != http.StatusSeeOther {
		t.Fatalf("ticket: %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/app?x=1" {
		t.Fatalf("redirect to %q", loc)
	}
	cookie := w.Result().Cookies()[0]

	// The cookie admits the request; the header is replaced, the cookie stripped.
	r = httptest.NewRequest("GET", "http://"+host+"/api/connections", nil)
	r.AddCookie(cookie)
	r.AddCookie(&http.Cookie{Name: "other", Value: "1"})
	r.Header.Set("X-Dbweb-Token", "forged")
	if !gate(httptest.NewRecorder(), r, g, host) {
		t.Fatal("session cookie refused")
	}
	if r.Header.Get("X-Dbweb-Token") != "secret" || strings.Contains(r.Header.Get("Cookie"), gateCookie) || !strings.Contains(r.Header.Get("Cookie"), "other=1") {
		t.Fatalf("headers: %v", r.Header)
	}

	// An expired or tampered ticket is refused.
	old := GateTicket(g.Key, host, time.Now().Add(-time.Hour))
	if gateValid(g.Key, "ticket", host, old, GateTicketTTL, time.Now()) {
		t.Fatal("expired ticket accepted")
	}
	tampered := GateTicket(g.Key, host, time.Now())
	tampered = tampered[:len(tampered)-1] + "0"
	if gateValid(g.Key, "ticket", host, tampered, GateTicketTTL, time.Now()) && !strings.HasSuffix(GateTicket(g.Key, host, time.Now()), "0") {
		t.Fatal("tampered ticket accepted")
	}
	// A ticket cannot be replayed as a session cookie.
	r = httptest.NewRequest("GET", "http://"+host+"/", nil)
	r.AddCookie(&http.Cookie{Name: gateCookie, Value: GateTicket(g.Key, host, time.Now())})
	if gate(httptest.NewRecorder(), r, g, host) {
		t.Fatal("ticket accepted as session")
	}
}
