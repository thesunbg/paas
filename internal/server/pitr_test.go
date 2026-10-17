package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/bus"
	"github.com/thesunbg/paas/internal/databases"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/s3"
	"github.com/thesunbg/paas/internal/store"
)

func TestNeededWAL(t *testing.T) {
	base := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return base.Add(d) }
	got := neededWAL([]s3.Object{
		{Key: "pitr/db/wal/old", LastModified: at(-31 * time.Minute)},
		{Key: "pitr/db/wal/edge", LastModified: at(-30 * time.Minute)},
		{Key: "pitr/db/wal/new", LastModified: at(time.Hour)},
		{Key: "pitr/db/wal/00000002.history", LastModified: at(-72 * time.Hour)},
		{Key: "pitr/db/wal/.hidden", LastModified: at(time.Hour)},
	}, base)
	if want := []string{"edge", "new", "00000002.history"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDedicatedSpecArchive(t *testing.T) {
	s := &Server{}
	db := &store.Database{Engine: "postgres", Mode: "dedicated", SizePreset: "nano", Options: store.DBOptions{PITR: true}}
	spec, err := s.dedicatedSpec(db, databases.Credentials{Username: "u", Password: "p", DBName: "d"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := strings.Join(spec.Command, "\x00")
	for _, want := range []string{"archive_mode=on", "archive_command=" + archiveCommand} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("missing %q in %q", want, spec.Command)
		}
	}
	if !strings.Contains(archiveCommand, "cp %p /wal/archive/.%f.tmp && mv /wal/archive/.%f.tmp /wal/archive/%f") {
		t.Fatalf("archive_command is not atomic: %s", archiveCommand)
	}
}

// fakePITRAgent answers exec, transfer and rmfiles for nodeID and records them.
type fakePITRAgent struct {
	mu        sync.Mutex
	spool     string // stdout of `ls -1 /wal/archive`
	stale     string // stdout of the `find` for old files
	failKey   string // PUT URLs containing this fail
	transfers []proto.TransferRequest
	removed   []string
}

func newFakePITRAgent(t *testing.T, s *Server, nodeID string) *fakePITRAgent {
	f := &fakePITRAgent{}
	reg := func(_ any, err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	reg(bus.Handle(s.nc, proto.AgentSubject(nodeID, proto.CmdExec), func(_ context.Context, r *proto.ExecRequest) *proto.ExecReply {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Cmd[0] == "find" {
			return &proto.ExecReply{Stdout: f.stale}
		}
		return &proto.ExecReply{Stdout: f.spool}
	}))
	reg(bus.Handle(s.nc, proto.AgentSubject(nodeID, proto.CmdTransfer), func(_ context.Context, r *proto.TransferRequest) *proto.TransferReply {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.transfers = append(f.transfers, *r)
		if f.failKey != "" && strings.Contains(r.URL, f.failKey) {
			return &proto.TransferReply{Error: "boom"}
		}
		if f.failKey == "*" {
			return &proto.TransferReply{Error: "boom"}
		}
		rep := &proto.TransferReply{}
		for i := range r.PartURLs {
			rep.ETags = append(rep.ETags, `"e`+string(rune('1'+i))+`"`)
		}
		return rep
	}))
	reg(bus.Handle(s.nc, proto.AgentSubject(nodeID, proto.CmdRemoveFiles), func(_ context.Context, r *proto.RemoveFilesRequest) *proto.GenericReply {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.removed = append(f.removed, r.Paths...)
		return &proto.GenericReply{}
	}))
	if err := s.nc.Flush(); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestShipSpool(t *testing.T) {
	s := newTestServerBus(t)
	ctx := context.Background()
	s.s3 = &s3.Config{Endpoint: "http://s3.invalid", Bucket: "b", AccessKey: "k", SecretKey: "s"}
	svc := seedDeployService(t, s)
	n, err := s.store.UpsertNodeLegacy(ctx, &proto.RegisterRequest{Name: "n", Address: "10.0.0.1", Roles: []string{"worker"}, CPUCores: 2, MemoryMB: 2048, Capabilities: map[string]string{}}, 16)
	if err != nil {
		t.Fatal(err)
	}
	in, err := s.store.CreateInstance(ctx, &store.Instance{ServiceID: svc.ID, NodeID: n.ID, ContainerID: "c1", Port: 5432, Status: model.InstRunning})
	if err != nil {
		t.Fatal(err)
	}
	_ = in
	f := newFakePITRAgent(t, s, n.ID)
	f.spool = "000000010000000000000001\n.000000010000000000000002.tmp\n000000010000000000000003\n"
	db := &store.Database{ID: "dbid", Name: "db"}

	if stale := s.shipSpool(ctx, db, svc.ID); stale {
		t.Fatal("a healthy ship must not warn")
	}
	if len(f.transfers) != 2 || !strings.Contains(f.transfers[0].URL, "/b/pitr/dbid/wal/000000010000000000000001?") {
		t.Fatalf("transfers: %+v", f.transfers)
	}
	wal := "volumes/" + svc.ID + "/wal/archive/"
	if want := []string{wal + "000000010000000000000001", wal + "000000010000000000000003"}; !slices.Equal(f.removed, want) {
		t.Fatalf("removed %v, want %v (the .tmp file must stay)", f.removed, want)
	}

	// The second file fails: the first is still removed, the failure is reported,
	// and the warning appears once a spooled file is older than 10 minutes.
	f.transfers, f.removed, f.failKey, f.stale = nil, nil, "0000000003", "/wal/archive/x\n"
	if stale := s.shipSpool(ctx, db, svc.ID); !stale {
		t.Fatal("expected a stale warning")
	}
	if len(f.removed) != 1 || !strings.HasSuffix(f.removed[0], "0001") {
		t.Fatalf("removed %v", f.removed)
	}
	f.failKey, f.stale = "*", ""
	if stale := s.shipSpool(ctx, db, svc.ID); stale || len(f.removed) != 1 {
		t.Fatalf("failing within 10 minutes must not warn nor remove: stale=%v removed=%v", stale, f.removed)
	}
}

func TestUploadLargeMultipart(t *testing.T) {
	s := newTestServerBus(t)
	var mu sync.Mutex
	var completed string
	aborted := false
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && q.Has("uploads"):
			w.Write([]byte(`<InitiateMultipartUploadResult><UploadId>u1</UploadId></InitiateMultipartUploadResult>`))
		case r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			completed = string(b)
			w.Write([]byte(`<CompleteMultipartUploadResult/>`))
		case r.Method == http.MethodDelete:
			aborted = true
		}
	}))
	defer fs.Close()
	s.s3 = &s3.Config{Endpoint: fs.URL, Bucket: "b", AccessKey: "k", SecretKey: "s"}
	f := newFakePITRAgent(t, s, "node1")
	ctx := context.Background()

	size := int64(2*multipartPart + 1) // three parts, the last one a single byte
	if err := s.uploadLarge(ctx, "node1", "pitr/db/base/x.tar.gz", "backups/db/x.tar.gz", size); err != nil {
		t.Fatal(err)
	}
	if len(f.transfers) != 1 || len(f.transfers[0].PartURLs) != 3 || f.transfers[0].PartSize != multipartPart || f.transfers[0].URL != "" {
		t.Fatalf("transfers: %+v", f.transfers)
	}
	for i, u := range f.transfers[0].PartURLs {
		if !strings.Contains(u, "partNumber="+string(rune('1'+i))+"&") || !strings.Contains(u, "uploadId=u1") {
			t.Fatalf("part url %d: %s", i, u)
		}
	}
	i1, i2, i3 := strings.Index(completed, "e1"), strings.Index(completed, "e2"), strings.Index(completed, "e3")
	if i1 < 0 || i1 > i2 || i2 > i3 {
		t.Fatalf("complete body must list the ETags in part order: %s", completed)
	}

	// A small file is one plain PUT.
	f.transfers = nil
	if err := s.uploadLarge(ctx, "node1", "pitr/db/base/y", "backups/db/y", 1<<20); err != nil {
		t.Fatal(err)
	}
	if len(f.transfers) != 1 || len(f.transfers[0].PartURLs) != 0 || f.transfers[0].URL == "" {
		t.Fatalf("small upload: %+v", f.transfers)
	}

	// A failing part aborts the multipart upload.
	f.transfers, f.failKey = nil, "*"
	if err := s.uploadLarge(ctx, "node1", "pitr/db/base/z", "backups/db/z", size); err == nil {
		t.Fatal("expected an error")
	}
	mu.Lock()
	defer mu.Unlock()
	if !aborted {
		t.Fatal("the multipart upload must be aborted")
	}
}

func TestSetPITRNeedsS3(t *testing.T) {
	s, h := newTestServer(t)
	u := mkUser(t, s, "a@example.com", "password1", true)
	_ = u
	_, tok := login(t, h, "a@example.com", "password1", "198.51.100.1")
	svc := seedDeployService(t, s)
	db, err := s.store.CreateDatabase(context.Background(), &store.Database{ProjectID: svc.ProjectID, Name: "d", Engine: "postgres", Mode: "dedicated", DBName: "d", Username: "u", SizePreset: "nano"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetDatabaseStatus(context.Background(), db.ID, "ready", ""); err != nil {
		t.Fatal(err)
	}
	w := do(t, h, "POST", "/api/v1/databases/"+db.ID+"/pitr", tok, map[string]any{"enabled": true})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "S3") {
		t.Fatalf("enabling without S3 = %d %s", w.Code, w.Body.String())
	}
	if got, _ := s.store.DatabaseByID(context.Background(), db.ID); got.Options.PITR {
		t.Fatal("the option must not be saved")
	}
}
