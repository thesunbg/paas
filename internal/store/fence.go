package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrFenced: a newer leader took over; this ctx's epoch is stale and nothing was written.
var ErrFenced = errors.New("fenced: superseded by a newer leader")

// fenceLockKey serializes epoch bumps against in-flight fenced transactions (see paas_fence).
const fenceLockKey = 7313002

type epochKey struct{}

// WithEpoch makes every store call on the returned ctx fail with ErrFenced once the
// leader epoch has moved past n.
func WithEpoch(ctx context.Context, n int64) context.Context {
	return context.WithValue(ctx, epochKey{}, n)
}

func epochFrom(ctx context.Context) (int64, bool) {
	n, ok := ctx.Value(epochKey{}).(int64)
	return n, ok
}

// Querier is what store code needs from the pool; the fenced variant has the same shape.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

func (s *Store) q(ctx context.Context) Querier { return s.Q(ctx) }

// Q returns the pool, or a fenced view of it when ctx carries a leader epoch.
func (s *Store) Q(ctx context.Context) Querier {
	if n, ok := epochFrom(ctx); ok {
		return fencedDB{s.DB, n}
	}
	return s.DB
}

// TakeLeadership bumps the epoch on conn (the leader's lock-holding connection) and returns
// it. The exclusive xact lock waits for in-flight fenced transactions of the old leader.
func (s *Store) TakeLeadership(ctx context.Context, conn *pgx.Conn) (int64, error) {
	var n int64
	err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(fenceLockKey)); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `UPDATE leader SET epoch=epoch+1 WHERE id=1 RETURNING epoch`).Scan(&n)
	})
	return n, err
}

// CheckFence is nil without an epoch, ErrFenced when the epoch is stale. For guarding
// side effects outside the database.
func (s *Store) CheckFence(ctx context.Context) error {
	n, ok := epochFrom(ctx)
	if !ok {
		return nil
	}
	_, err := s.DB.Exec(ctx, fenceSQL, n)
	return fenced(err)
}

const fenceSQL = `SELECT paas_fence($1)`

func fenced(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "PF001" {
		return ErrFenced
	}
	return err
}

// fencedDB runs each statement in one batch (= one implicit tx, one round trip) led by
// paas_fence, so a stale epoch aborts the batch before the statement is applied.
type fencedDB struct {
	pool  *pgxpool.Pool
	epoch int64
}

// send queues the fence plus the given queries and consumes the fence result.
func (f fencedDB) send(ctx context.Context, queued ...*pgx.QueuedQuery) (pgx.BatchResults, error) {
	b := &pgx.Batch{}
	b.Queue(fenceSQL, f.epoch)
	b.QueuedQueries = append(b.QueuedQueries, queued...)
	br := f.pool.SendBatch(ctx, b)
	if _, err := br.Exec(); err != nil {
		_ = br.Close()
		return nil, fenced(err)
	}
	return br, nil
}

func (f fencedDB) one(ctx context.Context, sql string, args []any) (pgx.BatchResults, error) {
	return f.send(ctx, &pgx.QueuedQuery{SQL: sql, Arguments: args})
}

func (f fencedDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	br, err := f.one(ctx, sql, args)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	tag, err := br.Exec()
	if cerr := br.Close(); err == nil {
		err = cerr
	}
	return tag, err
}

func (f fencedDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	br, err := f.one(ctx, sql, args)
	if err != nil {
		return nil, err
	}
	rows, err := br.Query()
	if err != nil {
		_ = br.Close()
		return nil, err
	}
	return &batchRows{Rows: rows, br: br}, nil
}

func (f fencedDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	br, err := f.one(ctx, sql, args)
	if err != nil {
		return errRow{err}
	}
	return &batchRow{Row: br.QueryRow(), br: br}
}

func (f fencedDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, fenceSQL, f.epoch); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fenced(err)
	}
	return tx, nil
}

func (f fencedDB) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	br, err := f.send(ctx, b.QueuedQueries...)
	if err != nil {
		return errBatch{err}
	}
	return br
}

// batchRows closes the underlying batch with the rows.
type batchRows struct {
	pgx.Rows
	br  pgx.BatchResults
	err error
}

func (r *batchRows) Close() {
	r.Rows.Close()
	if r.err == nil {
		r.err = r.br.Close()
	}
}

func (r *batchRows) Err() error {
	if err := r.Rows.Err(); err != nil {
		return err
	}
	return r.err
}

type batchRow struct {
	pgx.Row
	br pgx.BatchResults
}

func (r *batchRow) Scan(dest ...any) error {
	err := r.Row.Scan(dest...)
	if cerr := r.br.Close(); err == nil {
		err = cerr
	}
	return err
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

type errBatch struct{ err error }

func (e errBatch) Exec() (pgconn.CommandTag, error) { return pgconn.CommandTag{}, e.err }
func (e errBatch) Query() (pgx.Rows, error)         { return nil, e.err }
func (e errBatch) QueryRow() pgx.Row                { return errRow(e) }
func (e errBatch) Close() error                     { return e.err }
