package server

import (
	"net/http"
	"net/mail"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

// Account management for administrators.

// GET /api/v1/users
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, users)
}

// POST /api/v1/users {email, name?, password?, is_admin?}
// Without a password a random one is generated and returned once.
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in struct {
		Email    string `json:"email"`
		Name     string `json:"name"`
		Password string `json:"password"`
		IsAdmin  bool   `json:"is_admin"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	in.Email = strings.TrimSpace(strings.ToLower(in.Email))
	if _, err := mail.ParseAddress(in.Email); err != nil {
		writeError(w, 400, "invalid email")
		return
	}
	generated := ""
	if in.Password == "" {
		generated = util.RandomPassword(16)
		in.Password = generated
	} else if len(in.Password) < 8 {
		writeError(w, 400, "password must be at least 8 characters")
		return
	}
	u, err := s.createUser(r, credentialsInput{Email: in.Email, Name: strings.TrimSpace(in.Name), Password: in.Password}, in.IsAdmin)
	if err != nil {
		if store.IsUniqueViolation(err) {
			writeError(w, 409, "email already registered")
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	me := currentUser(r)
	s.store.Audit(r.Context(), &me.ID, nil, "user.create", u.Email)
	out := map[string]any{"user": u}
	if generated != "" {
		out["password"] = generated
	}
	writeJSON(w, 201, out)
}

// PATCH /api/v1/users/{id} {name?, is_admin?, password?, reset_password?, disable_2fa?}
func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	u, err := s.store.UserByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "user not found")
		return
	}
	var in struct {
		Name          *string `json:"name"`
		IsAdmin       *bool   `json:"is_admin"`
		Password      string  `json:"password"`
		ResetPassword bool    `json:"reset_password"`
		Disable2FA    bool    `json:"disable_2fa"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	me := currentUser(r)
	if in.IsAdmin != nil && !*in.IsAdmin && u.ID == me.ID {
		writeError(w, 400, "you cannot remove your own administrator role")
		return
	}
	name, admin := u.Name, u.IsAdmin
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		name = strings.TrimSpace(*in.Name)
	}
	if in.IsAdmin != nil {
		admin = *in.IsAdmin
	}
	if err := s.store.UpdateUser(r.Context(), u.ID, name, admin); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	out := map[string]any{}
	if in.ResetPassword || in.Password != "" {
		pw := in.Password
		if pw == "" {
			pw = util.RandomPassword(16)
			out["password"] = pw
		} else if len(pw) < 8 {
			writeError(w, 400, "password must be at least 8 characters")
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		if err := s.store.UpdatePassword(r.Context(), u.ID, string(hash)); err != nil {
			writeError(w, 500, err.Error())
			return
		}
	}
	if in.Disable2FA {
		if err := s.store.SetTOTP(r.Context(), u.ID, "", false); err != nil {
			writeError(w, 500, err.Error())
			return
		}
	}
	s.store.Audit(r.Context(), &me.ID, nil, "user.update", u.Email)
	if out["user"], err = s.store.UserByID(r.Context(), u.ID); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, out)
}
