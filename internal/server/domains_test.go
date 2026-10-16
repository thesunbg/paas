package server

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/thesunbg/paas/internal/config"
	"github.com/thesunbg/paas/internal/store"
)

func TestCheckCustomDomain(t *testing.T) {
	s := &Server{cfg: config.Server{BaseDomain: "apps.example.com", DashboardDomain: "paas.example.com", DBHost: "db.apps.example.com"}}
	for _, tc := range []struct {
		host  string
		admin bool
		ok    bool
	}{
		{"shop.example.org", false, true},
		{"paas.example.com", false, false},
		{"paas.example.com", true, false},
		{"db.apps.example.com", true, false},
		{"x.apps.example.com", false, false},
		{"apps.example.com", false, false},
		{"x.apps.example.com", true, true},
	} {
		r := httptest.NewRequest("POST", "/", nil)
		r = r.WithContext(context.WithValue(r.Context(), userKey, &store.User{IsAdmin: tc.admin}))
		if err := s.checkCustomDomain(r, tc.host); (err == nil) != tc.ok {
			t.Errorf("checkCustomDomain(%q, admin=%v) = %v, want ok=%v", tc.host, tc.admin, err, tc.ok)
		}
	}
}
