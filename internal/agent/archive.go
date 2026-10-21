package agent

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/thesunbg/paas/internal/proto"
)

// Archives of directories under the data dir: volume backups (the service's
// container frozen meanwhile) and static site copies. Owners, modes and
// symlinks are kept; extraction never follows a symlink it created.

func (a *Agent) handleArchive(ctx context.Context, req *proto.ArchiveRequest) *proto.ArchiveReply {
	defer a.work()()
	dir, err := a.dataPath(req.Dir)
	if err != nil {
		return &proto.ArchiveReply{Error: err.Error()}
	}
	file, err := a.dataPath(req.File)
	if err != nil || dir == "" || file == "" {
		return &proto.ArchiveReply{Error: "dir and file are required"}
	}
	if req.PauseContainer != "" {
		if err := a.rt.Pause(ctx, req.PauseContainer); err != nil {
			slog.Warn("archive: pause container", "id", req.PauseContainer, "err", err)
		} else {
			defer func() { _ = a.rt.Resume(context.Background(), req.PauseContainer) }()
		}
	}
	size, err := archiveDir(dir, file)
	if err != nil {
		return &proto.ArchiveReply{Error: err.Error()}
	}
	return &proto.ArchiveReply{Size: size}
}

func (a *Agent) handleExtract(ctx context.Context, req *proto.ExtractRequest) *proto.GenericReply {
	defer a.work()()
	file, err := a.dataPath(req.File)
	if err != nil {
		return &proto.GenericReply{Error: err.Error()}
	}
	dir, err := a.dataPath(req.Dir)
	if err != nil || dir == "" || file == "" {
		return &proto.GenericReply{Error: "file and dir are required"}
	}
	if entries, _ := os.ReadDir(dir); len(entries) > 0 {
		if !req.Replace {
			return &proto.GenericReply{Error: "the target directory is not empty"}
		}
		if err := os.RemoveAll(dir); err != nil {
			return &proto.GenericReply{Error: err.Error()}
		}
	}
	if err := extractArchive(file, dir); err != nil {
		return &proto.GenericReply{Error: err.Error()}
	}
	return &proto.GenericReply{}
}

func archiveDir(dir, file string) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return 0, err
	}
	tmp := file + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return nil // vanished while walking
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		} else if !info.Mode().IsRegular() && !info.IsDir() {
			return nil // sockets, fifos, devices
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			h.Name += "/"
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			h.Uid, h.Gid = int(st.Uid), int(st.Gid)
		}
		h.Uname, h.Gname = "", ""
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			src, err := os.Open(path)
			if err != nil {
				return err
			}
			// A file that shrinks while copied would break the archive: copy exactly h.Size.
			_, err = io.CopyN(tw, io.MultiReader(src, zeroReader{}), h.Size)
			src.Close()
			if err != nil {
				return err
			}
		}
		return nil
	})
	err = errors.Join(walkErr, tw.Close(), zw.Close(), f.Close())
	if err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if err := os.Rename(tmp, file); err != nil {
		return 0, err
	}
	st, err := os.Stat(file)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// safeParent fails when a directory on the way from root to path is a symlink.
func safeParent(root, path string) error {
	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil || rel == "." {
		return err
	}
	cur := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if st.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s goes through a symlink", path)
		}
	}
	return nil
}

func extractArchive(file, dest string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	type dirMeta struct {
		path     string
		mode     fs.FileMode
		uid, gid int
	}
	var dirs []dirMeta
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(h.Name))
		if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe path %q in archive", h.Name)
		}
		target := filepath.Join(dest, name)
		if err := safeParent(dest, target); err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			dirs = append(dirs, dirMeta{target, fs.FileMode(h.Mode) & fs.ModePerm, h.Uid, h.Gid})
			continue
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target) // never write through an existing symlink
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, fs.FileMode(h.Mode)&fs.ModePerm)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(h.Linkname, target); err != nil {
				return err
			}
		default:
			continue
		}
		_ = os.Lchown(target, h.Uid, h.Gid)
	}
	// Directory modes last: a read-only directory would refuse its own files.
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		_ = os.Chmod(d.path, d.mode)
		_ = os.Lchown(d.path, d.uid, d.gid)
	}
	return nil
}
