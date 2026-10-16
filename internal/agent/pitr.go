package agent

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/proto"
)

// handlePITRPrepare builds the data directory of a server that recovers a
// PostgreSQL database to a point in time (see server/dbaas_pitr.go).
func (a *Agent) handlePITRPrepare(ctx context.Context, req *proto.PITRPrepareRequest) *proto.GenericReply {
	if err := a.pitrPrepare(req); err != nil {
		return &proto.GenericReply{Error: err.Error()}
	}
	return &proto.GenericReply{}
}

func (a *Agent) pitrPrepare(req *proto.PITRPrepareRequest) error {
	base, err := a.dataPath(req.BaseBackup)
	if err != nil {
		return err
	}
	walSrc, err := a.dataPath(req.WALSource)
	if err != nil {
		return err
	}
	dataDir, err := a.dataPath(req.DataDir)
	if err != nil {
		return err
	}
	walDest, err := a.dataPath(req.WALDest)
	if err != nil {
		return err
	}
	if entries, _ := os.ReadDir(dataDir); len(entries) > 0 {
		return fmt.Errorf("target data directory is not empty")
	}
	// The volume root must stay traversable: the entrypoint re-runs as postgres.
	if err := os.MkdirAll(filepath.Dir(dataDir), 0o755); err != nil {
		return err
	}
	_ = os.Chmod(filepath.Dir(dataDir), 0o755)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	uid, gid, err := extractTarGz(base, dataDir)
	if err != nil {
		return fmt.Errorf("extract base backup: %w", err)
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return err
	}
	if err := os.Lchown(dataDir, uid, gid); err != nil {
		return err
	}
	// Recovery settings: replay archived WAL up to the target, then promote.
	conf := "\n# point-in-time recovery (paas)\nrestore_command = 'cp /wal/archive/%f %p'\nrecovery_target_action = 'promote'\n"
	if req.TargetTime != "" {
		t, err := time.Parse(time.RFC3339, req.TargetTime)
		if err != nil {
			return fmt.Errorf("target time: %w", err)
		}
		conf += fmt.Sprintf("recovery_target_time = '%s'\nrecovery_target_inclusive = true\n", t.UTC().Format("2006-01-02 15:04:05.000000+00"))
	}
	if err := appendFile(filepath.Join(dataDir, "postgresql.auto.conf"), conf, uid, gid); err != nil {
		return err
	}
	if err := appendFile(filepath.Join(dataDir, "recovery.signal"), "", uid, gid); err != nil {
		return err
	}
	// The new server reads WAL from its own archive volume.
	archive := filepath.Join(walDest, "archive")
	if err := os.MkdirAll(walDest, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(archive, 0o700); err != nil {
		return err
	}
	_ = os.Lchown(walDest, uid, gid)
	_ = os.Lchown(archive, uid, gid)
	files, err := os.ReadDir(walSrc)
	if err != nil {
		return fmt.Errorf("read WAL archive: %w", err)
	}
	for _, f := range files {
		if f.IsDir() || !f.Type().IsRegular() {
			continue
		}
		if err := copyFile(filepath.Join(walSrc, f.Name()), filepath.Join(archive, f.Name()), uid, gid); err != nil {
			return err
		}
	}
	return nil
}

// extractTarGz unpacks a pg_basebackup tar, keeping the owner of its entries.
func extractTarGz(path, dest string) (uid, gid int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return 0, 0, err
	}
	tr := tar.NewReader(zr)
	uid, gid = -1, -1
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, 0, err
		}
		name := filepath.Clean(h.Name)
		if name == "." {
			continue
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return 0, 0, fmt.Errorf("unsafe path %q in backup", h.Name)
		}
		target := filepath.Join(dest, name)
		if uid < 0 {
			uid, gid = h.Uid, h.Gid
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(h.Mode)&0o777|0o700); err != nil {
				return 0, 0, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return 0, 0, err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&0o777)
			if err != nil {
				return 0, 0, err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return 0, 0, err
			}
			out.Close()
		case tar.TypeSymlink:
			// pg_basebackup emits symlinks only for pg_wal / tablespaces; keep them inside dest.
			if filepath.IsAbs(h.Linkname) || strings.Contains(h.Linkname, "..") {
				continue
			}
			_ = os.Symlink(h.Linkname, target)
		default:
			continue
		}
		_ = os.Lchown(target, h.Uid, h.Gid)
	}
	if uid < 0 {
		return 0, 0, fmt.Errorf("empty base backup")
	}
	// pg_wal must exist (it is excluded from base backups taken with -X none).
	wal := filepath.Join(dest, "pg_wal")
	if err := os.MkdirAll(wal, 0o700); err != nil {
		return 0, 0, err
	}
	_ = os.Lchown(wal, uid, gid)
	return uid, gid, nil
}

func appendFile(path, content string, uid, gid int) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Lchown(path, uid, gid)
}

func copyFile(src, dst string, uid, gid int) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Lchown(dst, uid, gid)
}
