package store

import "context"

// LiveServiceCount counts services that serve a deployment, leaving out the
// given project (the platform's own system project; "" counts everything).
func (s *Store) LiveServiceCount(ctx context.Context, excludeProjectID string) (int, error) {
	var n int
	err := s.q(ctx).QueryRow(ctx, `SELECT count(*) FROM services
		WHERE current_deployment_id IS NOT NULL AND ($1 = '' OR project_id::text <> $1)`, excludeProjectID).Scan(&n)
	return n, err
}
