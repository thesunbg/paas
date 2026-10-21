package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/s3"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

// Off-site storage for backups (database dumps, PITR, volumes, static sites,
// the platform's own state). PAAS_S3_* in the environment wins; otherwise an
// administrator sets it in the dashboard and it is stored encrypted in the
// settings, so every control plane picks it up (reloaded every minute).

const settingS3 = "s3_config"

func (s *Server) storage() *s3.Config {
	if c := s.s3.Load(); c != nil {
		return c
	}
	return &s3.Config{}
}

// loadStorageSettings applies the dashboard's S3 settings unless the
// environment configures storage.
func (s *Server) loadStorageSettings(ctx context.Context) {
	if s.s3Env {
		return
	}
	enc, err := s.store.GetSetting(ctx, settingS3)
	if errors.Is(err, store.ErrNotFound) {
		if s.storage().Enabled() {
			s.s3.Store(&s3.Config{})
		}
		return
	}
	if err != nil {
		return
	}
	plain, err := s.box.Decrypt(enc)
	if err != nil {
		slog.Warn("s3 settings", "err", err)
		return
	}
	var c s3.Config
	if err := json.Unmarshal([]byte(plain), &c); err != nil {
		return
	}
	if c.Prefix == "" {
		c.Prefix = "paas-backups"
	}
	if cur := s.storage(); *cur != c {
		s.s3.Store(&c)
	}
}

func (s *Server) storageLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		s.loadStorageSettings(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type storageView struct {
	Enabled     bool   `json:"enabled"`
	FromEnv     bool   `json:"from_env"`
	Endpoint    string `json:"endpoint"`
	Region      string `json:"region"`
	Bucket      string `json:"bucket"`
	AccessKey   string `json:"access_key"`
	HasSecret   bool   `json:"has_secret"`
	Prefix      string `json:"prefix"`
	VirtualHost bool   `json:"virtual_host"`
}

func (s *Server) storageView() storageView {
	c := s.storage()
	return storageView{Enabled: c.Enabled(), FromEnv: s.s3Env, Endpoint: c.Endpoint, Region: c.Region, Bucket: c.Bucket,
		AccessKey: c.AccessKey, HasSecret: c.SecretKey != "", Prefix: c.Prefix, VirtualHost: c.VirtualHost}
}

// GET /api/v1/admin/storage
func (s *Server) handleGetStorage(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	writeJSON(w, 200, s.storageView())
}

// checkStorage writes, reads back and deletes a small object.
func checkStorage(c *s3.Config) error {
	key := c.Key("paas-check", util.RandomToken(8))
	body := []byte("paas storage check " + time.Now().UTC().Format(time.RFC3339))
	if err := c.Put(key, bytes.NewReader(body), int64(len(body))); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	rc, err := c.Get(key)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	got, _ := io.ReadAll(io.LimitReader(rc, 1024))
	rc.Close()
	if !bytes.Equal(got, body) {
		return errors.New("download: the object read back differs")
	}
	if err := c.Delete(key); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	return nil
}

// PUT /api/v1/admin/storage {endpoint, region, bucket, access_key, secret_key, prefix, virtual_host}
// An empty endpoint turns off-site storage off. The configuration is tested
// (upload, download, delete) before it is saved.
func (s *Server) handlePutStorage(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if s.s3Env {
		writeError(w, 409, "storage is configured by PAAS_S3_* on the server; change it there")
		return
	}
	var in struct {
		Endpoint    string `json:"endpoint"`
		Region      string `json:"region"`
		Bucket      string `json:"bucket"`
		AccessKey   string `json:"access_key"`
		SecretKey   string `json:"secret_key"`
		Prefix      string `json:"prefix"`
		VirtualHost bool   `json:"virtual_host"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	ctx := r.Context()
	u := currentUser(r)
	if strings.TrimSpace(in.Endpoint) == "" {
		if err := s.store.DeleteSetting(ctx, settingS3); err != nil && !errors.Is(err, store.ErrNotFound) {
			writeError(w, 500, err.Error())
			return
		}
		s.s3.Store(&s3.Config{})
		s.store.Audit(ctx, &u.ID, nil, "storage.disable", "")
		writeJSON(w, 200, s.storageView())
		return
	}
	c := s3.Config{
		Endpoint: strings.TrimRight(strings.TrimSpace(in.Endpoint), "/"), Region: firstNonEmpty(strings.TrimSpace(in.Region), "auto"),
		Bucket: strings.TrimSpace(in.Bucket), AccessKey: strings.TrimSpace(in.AccessKey), SecretKey: in.SecretKey,
		Prefix: strings.Trim(firstNonEmpty(strings.TrimSpace(in.Prefix), "paas-backups"), "/"), VirtualHost: in.VirtualHost,
	}
	if c.SecretKey == "" { // unchanged secret
		c.SecretKey = s.storage().SecretKey
	}
	if !strings.HasPrefix(c.Endpoint, "https://") && !strings.HasPrefix(c.Endpoint, "http://") {
		writeError(w, 400, "endpoint must start with https://")
		return
	}
	if !c.Enabled() {
		writeError(w, 400, "endpoint, bucket, access key and secret key are required")
		return
	}
	if err := checkStorage(&c); err != nil {
		writeError(w, 400, "storage check failed: "+err.Error())
		return
	}
	b, _ := json.Marshal(c)
	if err := s.store.SetSetting(ctx, settingS3, s.box.MustEncrypt(string(b))); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	s.s3.Store(&c)
	s.store.Audit(ctx, &u.ID, nil, "storage.update", c.Endpoint+"/"+c.Bucket)
	writeJSON(w, 200, s.storageView())
}
