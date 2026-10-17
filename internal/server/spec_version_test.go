package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/store"
)

func TestPatchServiceIfMatch(t *testing.T) {
	s, h := newTestServer(t)
	ctx := context.Background()
	u := mkUser(t, s, "a@example.com", "password1", true)
	_, tok := login(t, h, "a@example.com", "password1", "198.51.100.1")
	team, err := s.store.CreateTeam(ctx, "t", "t", u.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := s.store.CreateProject(ctx, team.ID, "p", "p")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := s.store.CreateService(ctx, &store.Service{
		ProjectID: proj.ID, Name: "web", Slug: "web", Type: model.TypeWeb,
		Source: model.ServiceSource{Kind: "image", Image: "nginx"}, Spec: model.ServiceSpec{Replicas: 1, Port: 8080},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/services/" + svc.ID

	get := do(t, h, "GET", path, tok, nil)
	if get.Code != 200 || get.Header().Get("ETag") != `"1"` {
		t.Fatalf("GET = %d etag %q %s", get.Code, get.Header().Get("ETag"), get.Body)
	}

	patch := func(ifMatch, name string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PATCH", path, strings.NewReader(`{"name":"`+name+`"}`))
		r.RemoteAddr = "192.0.2.1:1234"
		r.Header.Set("Authorization", "Bearer "+tok)
		if ifMatch != "" {
			r.Header.Set("If-Match", ifMatch)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := patch(`"1"`, "one"); w.Code != http.StatusOK {
		t.Fatalf("matching If-Match = %d %s", w.Code, w.Body)
	}
	// The row is now at version 2; the same header is stale.
	w := patch(`"1"`, "two")
	if w.Code != http.StatusPreconditionFailed || !strings.Contains(w.Body.String(), "changed elsewhere") {
		t.Fatalf("stale If-Match = %d %s", w.Code, w.Body)
	}
	if w := patch("", "three"); w.Code != http.StatusOK {
		t.Fatalf("no If-Match = %d %s", w.Code, w.Body)
	}
	row, _ := s.store.ServiceByID(ctx, svc.ID)
	if row.Name != "three" || row.Version != 3 {
		t.Fatalf("row = %q v%d, want three v3", row.Name, row.Version)
	}
}
