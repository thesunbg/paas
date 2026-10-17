package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// takeLeadership bumps the epoch on its own connection, like a new leader would.
func takeLeadership(t *testing.T, st *Store) int64 {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, st.DB.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	n, err := st.TakeLeadership(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestFencedWritesAfterNewLeader(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	svc := seedService(t, st, seedProject(t, st))

	n := takeLeadership(t, st)
	if n != 1 {
		t.Fatalf("first epoch = %d, want 1", n)
	}
	old := WithEpoch(ctx, n)
	if err := st.SetSetting(old, "k", "v1"); err != nil {
		t.Fatalf("write at current epoch: %v", err)
	}
	if _, err := st.CreateUser(old, "a@x.io", "a", "h", false); err != nil {
		t.Fatalf("QueryRow write at current epoch: %v", err)
	}
	if _, err := st.MarkStaleNodesOffline(old, time.Minute); err != nil {
		t.Fatalf("Query at current epoch: %v", err)
	}
	if err := st.AppendDeploymentLogs(old, "00000000-0000-0000-0000-000000000000", nil); err != nil {
		t.Fatal(err)
	}

	if m := takeLeadership(t, st); m != n+1 {
		t.Fatalf("second epoch = %d, want %d", m, n+1)
	}
	if err := st.SetSetting(old, "k", "v2"); !errors.Is(err, ErrFenced) {
		t.Fatalf("Exec err = %v, want ErrFenced", err)
	}
	if v, _ := st.GetSetting(ctx, "k"); v != "v1" {
		t.Fatalf("setting = %q, want v1 (fenced write must not apply)", v)
	}
	if _, err := st.CreateUser(old, "b@x.io", "b", "h", false); !errors.Is(err, ErrFenced) {
		t.Fatalf("QueryRow err = %v, want ErrFenced", err)
	}
	if _, err := st.UserByEmail(ctx, "b@x.io"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fenced insert visible: %v", err)
	}
	if _, err := st.MarkStaleNodesOffline(old, time.Minute); !errors.Is(err, ErrFenced) {
		t.Fatalf("Query err = %v, want ErrFenced", err)
	}
	if err := st.AppendDeploymentLogs(old, "x", []string{"l"}); !errors.Is(err, ErrFenced) {
		t.Fatalf("SendBatch err = %v, want ErrFenced", err)
	}
	_, err := st.UpdateService(old, svc.ID, func(f *Service) error { f.Spec.Replicas = 7; return nil })
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("Begin err = %v, want ErrFenced", err)
	}
	if row, _ := st.ServiceByID(ctx, svc.ID); row.Spec.Replicas == 7 {
		t.Fatal("fenced UpdateService applied")
	}
	if err := st.SetSetting(ctx, "k", "v3"); err != nil {
		t.Fatalf("write without epoch: %v", err)
	}
}

func TestTakeLeadershipWaitsForFencedTx(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	n := takeLeadership(t, st)

	tx, err := st.Q(WithEpoch(ctx, n)).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int64, 1)
	go func() { done <- takeLeadership(t, st) }()
	select {
	case m := <-done:
		t.Fatalf("TakeLeadership returned %d while a fenced tx was open", m)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-done:
		if m != n+1 {
			t.Fatalf("epoch = %d, want %d", m, n+1)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TakeLeadership still blocked after commit")
	}
}

func TestCheckFence(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.CheckFence(ctx); err != nil {
		t.Fatalf("no epoch: %v", err)
	}
	n := takeLeadership(t, st)
	if err := st.CheckFence(WithEpoch(ctx, n)); err != nil {
		t.Fatalf("current epoch: %v", err)
	}
	takeLeadership(t, st)
	if err := st.CheckFence(WithEpoch(ctx, n)); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale epoch: %v, want ErrFenced", err)
	}
}

