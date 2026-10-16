package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type User struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	Name         string    `json:"name"`
	PasswordHash string    `json:"-"`
	IsAdmin      bool      `json:"is_admin"`
	TOTPEnabled  bool      `json:"totp_enabled"`
	TOTPSecret   string    `json:"-"`
	GitHubID     int64     `json:"-"`
	GitHubLogin  string    `json:"github_login,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

const userCols = `id, email, name, password_hash, is_admin, totp_enabled, totp_secret_enc, COALESCE(github_id, 0), COALESCE(github_login, ''), created_at`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.IsAdmin, &u.TOTPEnabled, &u.TOTPSecret, &u.GitHubID, &u.GitHubLogin, &u.CreatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &u, nil
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.DB.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) CreateUser(ctx context.Context, email, name, hash string, admin bool) (*User, error) {
	return scanUser(s.DB.QueryRow(ctx, `INSERT INTO users (email, name, password_hash, is_admin) VALUES (lower($1),$2,$3,$4) RETURNING `+userCols, email, name, hash, admin))
}

func (s *Store) UserByEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(s.DB.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE email=lower($1)`, email))
}

func (s *Store) UserByID(ctx context.Context, id string) (*User, error) {
	return scanUser(s.DB.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1`, id))
}

func (s *Store) UpdatePassword(ctx context.Context, id, hash string) error {
	_, err := s.DB.Exec(ctx, `UPDATE users SET password_hash=$2 WHERE id=$1`, id, hash)
	return err
}

// Sessions.

func (s *Store) CreateSession(ctx context.Context, tokenHash, userID string, expires time.Time) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1,$2,$3)`, tokenHash, userID, expires)
	return err
}

func (s *Store) UserBySession(ctx context.Context, tokenHash string) (*User, error) {
	return scanUser(s.DB.QueryRow(ctx, `SELECT u.id, u.email, u.name, u.password_hash, u.is_admin, u.totp_enabled, u.totp_secret_enc, COALESCE(u.github_id, 0), COALESCE(u.github_login, ''), u.created_at
		FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=$1 AND s.expires_at > now()`, tokenHash))
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM sessions WHERE token_hash=$1`, tokenHash)
	return err
}

func (s *Store) PurgeExpiredSessions(ctx context.Context) {
	_, _ = s.DB.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`)
}

// API tokens.

type APIToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

func (s *Store) CreateAPIToken(ctx context.Context, userID, name, tokenHash string) (*APIToken, error) {
	var t APIToken
	err := s.DB.QueryRow(ctx, `INSERT INTO api_tokens (user_id, name, token_hash) VALUES ($1,$2,$3) RETURNING id, name, last_used_at, created_at`,
		userID, name, tokenHash).Scan(&t.ID, &t.Name, &t.LastUsedAt, &t.CreatedAt)
	return &t, err
}

func (s *Store) ListAPITokens(ctx context.Context, userID string) ([]APIToken, error) {
	rows, err := s.DB.Query(ctx, `SELECT id, name, last_used_at, created_at FROM api_tokens WHERE user_id=$1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (APIToken, error) {
		var t APIToken
		err := r.Scan(&t.ID, &t.Name, &t.LastUsedAt, &t.CreatedAt)
		return t, err
	})
}

func (s *Store) DeleteAPIToken(ctx context.Context, userID, id string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM api_tokens WHERE user_id=$1 AND id=$2`, userID, id)
	return err
}

func (s *Store) UserByAPIToken(ctx context.Context, tokenHash string) (*User, error) {
	u, err := scanUser(s.DB.QueryRow(ctx, `UPDATE api_tokens t SET last_used_at=now() FROM users u
		WHERE t.token_hash=$1 AND u.id=t.user_id
		RETURNING u.id, u.email, u.name, u.password_hash, u.is_admin, u.totp_enabled, u.totp_secret_enc, COALESCE(u.github_id, 0), COALESCE(u.github_login, ''), u.created_at`, tokenHash))
	return u, err
}

// Teams.

type Team struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	Role      string    `json:"role,omitempty"`
	IsSystem  bool      `json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) CreateTeam(ctx context.Context, name, slug, ownerID string, system bool) (*Team, error) {
	var t Team
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO teams (name, slug, is_system) VALUES ($1,$2,$3) RETURNING id, name, slug, is_system, created_at`,
			name, slug, system).Scan(&t.ID, &t.Name, &t.Slug, &t.IsSystem, &t.CreatedAt); err != nil {
			return err
		}
		if ownerID != "" {
			t.Role = "owner"
			_, err := tx.Exec(ctx, `INSERT INTO memberships (team_id, user_id, role) VALUES ($1,$2,'owner')`, t.ID, ownerID)
			return err
		}
		return nil
	})
	return &t, err
}

func (s *Store) TeamsForUser(ctx context.Context, userID string) ([]Team, error) {
	rows, err := s.DB.Query(ctx, `SELECT t.id, t.name, t.slug, m.role, t.is_system, t.created_at FROM teams t
		JOIN memberships m ON m.team_id=t.id WHERE m.user_id=$1 AND NOT t.is_system ORDER BY t.created_at`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Team, error) {
		var t Team
		err := r.Scan(&t.ID, &t.Name, &t.Slug, &t.Role, &t.IsSystem, &t.CreatedAt)
		return t, err
	})
}

// MemberRole returns the user's role in the team, or ErrNotFound.
func (s *Store) MemberRole(ctx context.Context, teamID, userID string) (string, error) {
	var role string
	err := s.DB.QueryRow(ctx, `SELECT role FROM memberships WHERE team_id=$1 AND user_id=$2`, teamID, userID).Scan(&role)
	return role, notFound(err)
}

func (s *Store) SystemTeam(ctx context.Context) (*Team, error) {
	var t Team
	err := s.DB.QueryRow(ctx, `SELECT id, name, slug, is_system, created_at FROM teams WHERE is_system LIMIT 1`).
		Scan(&t.ID, &t.Name, &t.Slug, &t.IsSystem, &t.CreatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &t, nil
}

type Member struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	Name   string `json:"name"`
	Role   string `json:"role"`
}

func (s *Store) TeamMembers(ctx context.Context, teamID string) ([]Member, error) {
	rows, err := s.DB.Query(ctx, `SELECT u.id, u.email, u.name, m.role FROM memberships m JOIN users u ON u.id=m.user_id WHERE m.team_id=$1 ORDER BY m.created_at`, teamID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Member, error) {
		var m Member
		err := r.Scan(&m.UserID, &m.Email, &m.Name, &m.Role)
		return m, err
	})
}

func (s *Store) AddMember(ctx context.Context, teamID, userID, role string) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO memberships (team_id, user_id, role) VALUES ($1,$2,$3)
		ON CONFLICT (team_id, user_id) DO UPDATE SET role=EXCLUDED.role`, teamID, userID, role)
	return err
}

func (s *Store) RemoveMember(ctx context.Context, teamID, userID string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM memberships WHERE team_id=$1 AND user_id=$2`, teamID, userID)
	return err
}

func (s *Store) SetTOTP(ctx context.Context, userID, secretEnc string, enabled bool) error {
	_, err := s.DB.Exec(ctx, `UPDATE users SET totp_secret_enc=$2, totp_enabled=$3 WHERE id=$1`, userID, secretEnc, enabled)
	return err
}

func (s *Store) UserByGitHubID(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.DB.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE github_id=$1`, id))
}

// LinkGitHub attaches (or with id 0 detaches) a GitHub account.
func (s *Store) LinkGitHub(ctx context.Context, userID string, id int64, login string) error {
	if id == 0 {
		_, err := s.DB.Exec(ctx, `UPDATE users SET github_id=NULL, github_login=NULL WHERE id=$1`, userID)
		return err
	}
	_, err := s.DB.Exec(ctx, `UPDATE users SET github_id=$2, github_login=$3 WHERE id=$1`, userID, id, login)
	return err
}

// UserSummary is a user with the number of teams they belong to (admin listing).
type UserSummary struct {
	User
	Teams int `json:"teams"`
}

func (s *Store) ListUsers(ctx context.Context) ([]UserSummary, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+userCols+`, (SELECT count(*) FROM memberships m WHERE m.user_id=users.id) FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserSummary{}
	for rows.Next() {
		var u UserSummary
		if err := rows.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.IsAdmin, &u.TOTPEnabled, &u.TOTPSecret, &u.GitHubID, &u.GitHubLogin, &u.CreatedAt, &u.Teams); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) UpdateUser(ctx context.Context, id, name string, admin bool) error {
	_, err := s.DB.Exec(ctx, `UPDATE users SET name=$2, is_admin=$3 WHERE id=$1`, id, name, admin)
	return err
}
