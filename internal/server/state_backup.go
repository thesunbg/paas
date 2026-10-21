package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/statebackup"
	"github.com/thesunbg/paas/internal/store"
)

// Backups of the platform's own state: the control-plane PostgreSQL (projects,
// services, domains, encrypted secrets, nodes, users…). Every hour the leader
// dumps it, seals it with a key derived from the master key, keeps the last
// 24 on this server and uploads to the off-site storage, where 48 hourly and
// 30 daily copies are kept. `paasd restore-state` puts one back.
//
// The master key itself is never in a backup: without it the sealed files and
// every secret in them are useless, so it has to be kept apart (a password
// manager), together with the storage credentials.

const (
	settingStateBackup = "state_backup_last"
	stateEvery         = time.Hour
	stateKeepLocal     = 24
	stateKeepHourly    = 48
	stateKeepDaily     = 30
	stateExt           = ".pgstate"
)

type stateBackupInfo struct {
	At       time.Time `json:"at"`
	Name     string    `json:"name,omitempty"`
	Size     int64     `json:"size,omitempty"`
	Offsite  bool      `json:"offsite"`
	Error    string    `json:"error,omitempty"`
	Duration string    `json:"duration,omitempty"`
}

var stateNameRe = regexp.MustCompile(`^state-\d{8}-\d{6}\.pgstate$`)

func (s *Server) stateDir() string { return filepath.Join(s.cfg.DataDir, "state-backups") }

func (s *Server) lastStateBackup(ctx context.Context) (stateBackupInfo, error) {
	var info stateBackupInfo
	v, err := s.store.GetSetting(ctx, settingStateBackup)
	if err != nil {
		return info, err
	}
	err = json.Unmarshal([]byte(v), &info)
	return info, err
}

func (s *Server) stateBackupTick(ctx context.Context) {
	last, err := s.lastStateBackup(ctx)
	if err == nil && time.Since(last.At) < stateEvery {
		return
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return
	}
	go func() {
		if _, err := s.backupState(context.WithoutCancel(ctx)); err != nil {
			slog.Warn("state backup", "err", err)
		}
	}()
}

// pgDump finds the newest pg_dump on this host (one at least as new as the server is required).
func pgDump() (string, error) {
	matches, _ := filepath.Glob("/usr/lib/postgresql/*/bin/pg_dump")
	sort.Slice(matches, func(i, j int) bool { return pgMajor(matches[i]) > pgMajor(matches[j]) })
	if len(matches) > 0 {
		return matches[0], nil
	}
	return exec.LookPath("pg_dump")
}

func pgMajor(p string) int {
	var n int
	fmt.Sscanf(filepath.Base(filepath.Dir(filepath.Dir(p))), "%d", &n)
	return n
}

// backupState takes one backup now.
func (s *Server) backupState(ctx context.Context) (stateBackupInfo, error) {
	if !s.stateBackupMu.TryLock() {
		return stateBackupInfo{}, errors.New("a backup of the platform state is already running")
	}
	defer s.stateBackupMu.Unlock()
	start := time.Now()
	info := stateBackupInfo{At: start.UTC(), Name: "state-" + start.UTC().Format("20060102-150405") + stateExt}
	err := s.writeStateBackup(ctx, &info)
	if err != nil {
		info.Error = err.Error()
		s.notify(ctx, "", "backup.failed", "Backup of the platform state failed: "+err.Error(), nil)
	}
	info.Duration = time.Since(start).Round(time.Second).String()
	b, _ := json.Marshal(info)
	_ = s.store.SetSetting(ctx, settingStateBackup, string(b))
	return info, err
}

func (s *Server) writeStateBackup(ctx context.Context, info *stateBackupInfo) error {
	bin, err := pgDump()
	if err != nil {
		return errors.New("pg_dump is not installed on this control plane (apt install postgresql-client)")
	}
	if err := os.MkdirAll(s.stateDir(), 0o700); err != nil {
		return err
	}
	path := filepath.Join(s.stateDir(), info.Name)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(dctx, bin, "--format=custom", "--no-owner", "--no-privileges", "--dbname="+s.cfg.DatabaseURL)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		f.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		f.Close()
		return err
	}
	sealErr := statebackup.Seal(f, out, s.masterKey)
	waitErr := cmd.Wait()
	closeErr := f.Close()
	if waitErr != nil {
		os.Remove(tmp)
		return fmt.Errorf("pg_dump: %v: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	if sealErr != nil || closeErr != nil {
		os.Remove(tmp)
		return errors.Join(sealErr, closeErr)
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if st, err := os.Stat(path); err == nil {
		info.Size = st.Size()
	}
	s.pruneLocalState()
	if c := s.storage(); c.Enabled() {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := c.Put(c.Key("state", info.Name), f, info.Size); err != nil {
			return fmt.Errorf("kept on this server, upload failed: %w", err)
		}
		info.Offsite = true
		s.pruneOffsiteState()
	}
	return nil
}

func (s *Server) localStateBackups() []string {
	entries, _ := os.ReadDir(s.stateDir())
	var names []string
	for _, e := range entries {
		if stateNameRe.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names
}

func (s *Server) pruneLocalState() {
	for i, n := range s.localStateBackups() {
		if i >= stateKeepLocal {
			_ = os.Remove(filepath.Join(s.stateDir(), n))
		}
	}
}

// keepState decides which off-site backups stay: the newest stateKeepHourly,
// plus the newest of each day for stateKeepDaily days. names are newest first.
func keepState(names []string) map[string]bool {
	keep := map[string]bool{}
	days := map[string]bool{}
	for i, n := range names {
		if i < stateKeepHourly {
			keep[n] = true
		}
		day := strings.TrimPrefix(n, "state-")[:8]
		if !days[day] && len(days) < stateKeepDaily {
			days[day] = true
			keep[n] = true
		}
	}
	return keep
}

func (s *Server) offsiteStateBackups() ([]string, error) {
	c := s.storage()
	objs, err := c.List(c.Key("state") + "/")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, o := range objs {
		if n := filepath.Base(o.Key); stateNameRe.MatchString(n) {
			names = append(names, n)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names, nil
}

func (s *Server) pruneOffsiteState() {
	names, err := s.offsiteStateBackups()
	if err != nil {
		return
	}
	keep := keepState(names)
	c := s.storage()
	for _, n := range names {
		if !keep[n] {
			_ = c.Delete(c.Key("state", n))
		}
	}
}

// GET /api/v1/admin/state-backups
func (s *Server) handleListStateBackups(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	type item struct {
		Name    string `json:"name"`
		Local   bool   `json:"local"`
		Offsite bool   `json:"offsite"`
	}
	byName := map[string]*item{}
	for _, n := range s.localStateBackups() {
		byName[n] = &item{Name: n, Local: true}
	}
	offErr := ""
	if s.storage().Enabled() {
		names, err := s.offsiteStateBackups()
		if err != nil {
			offErr = err.Error()
		}
		for _, n := range names {
			if byName[n] == nil {
				byName[n] = &item{Name: n}
			}
			byName[n].Offsite = true
		}
	}
	items := []item{}
	for _, it := range byName {
		items = append(items, *it)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name > items[j].Name })
	last, _ := s.lastStateBackup(r.Context())
	writeJSON(w, 200, map[string]any{"last": last, "backups": items, "offsite_error": offErr, "every": stateEvery.String()})
}

// POST /api/v1/admin/state-backups
func (s *Server) handleCreateStateBackup(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	info, err := s.backupState(context.WithoutCancel(r.Context()))
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, nil, "state.backup", info.Name)
	writeJSON(w, 201, info)
}

// GET /api/v1/admin/state-backups/{name}: the sealed file, to keep a copy elsewhere.
func (s *Server) handleDownloadStateBackup(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	name := r.PathValue("name")
	if !stateNameRe.MatchString(name) {
		writeError(w, 404, "backup not found")
		return
	}
	var body io.ReadCloser
	if f, err := os.Open(filepath.Join(s.stateDir(), name)); err == nil {
		body = f
	} else if c := s.storage(); c.Enabled() {
		rc, err := c.Get(c.Key("state", name))
		if err != nil {
			writeError(w, 404, "backup not found")
			return
		}
		body = rc
	} else {
		writeError(w, 404, "backup not found")
		return
	}
	defer body.Close()
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, nil, "state.download", name)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	_, _ = io.Copy(w, body)
}
