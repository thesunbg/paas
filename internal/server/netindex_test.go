package server

import (
	"context"
	"testing"
)

func TestProjectNetIndexReuse(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()
	u := mkUser(t, s, "owner@example.com", "password1", false)
	team, err := s.store.CreateTeam(ctx, "t", "t", u.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.store.CreateProject(ctx, team.ID, "a", "a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.store.CreateProject(ctx, team.ID, "b", "b")
	if err != nil {
		t.Fatal(err)
	}
	if b.NetIndex != a.NetIndex+1 {
		t.Fatalf("want consecutive indexes, got %d then %d", a.NetIndex, b.NetIndex)
	}
	if err := s.store.DeleteProject(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	// Cooling down: the freed slot is skipped.
	c, err := s.store.CreateProject(ctx, team.ID, "c", "c")
	if err != nil {
		t.Fatal(err)
	}
	if c.NetIndex == a.NetIndex {
		t.Fatalf("index %d reused during cooldown", a.NetIndex)
	}
	// After the cooldown it is the lowest free slot again.
	if _, err := s.store.DB.Exec(ctx, `UPDATE net_index_released SET released_at = now() - interval '25 hours'`); err != nil {
		t.Fatal(err)
	}
	d, err := s.store.CreateProject(ctx, team.ID, "d", "d")
	if err != nil {
		t.Fatal(err)
	}
	if d.NetIndex != a.NetIndex {
		t.Fatalf("want released index %d reused, got %d", a.NetIndex, d.NetIndex)
	}
}

func TestProjectNetIndexExhausted(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()
	u := mkUser(t, s, "owner@example.com", "password1", false)
	team, err := s.store.CreateTeam(ctx, "t", "t", u.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.DB.Exec(ctx, `INSERT INTO net_index_released (net_index) SELECT i FROM generate_series(1, 1021) i
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.CreateProject(ctx, team.ID, "x", "x"); err == nil {
		t.Fatal("want an error when every slot is taken or cooling down")
	}
}
