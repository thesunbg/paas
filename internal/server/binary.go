package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"runtime"
	"sync"

	"github.com/thesunbg/paas/internal/proto"
)

var (
	binaryOnce sync.Once
	binarySum  string
)

// ownBinarySHA256 is the checksum of the running paasd binary. A deploy
// replaces the binary and restarts the process, so computing it once is enough.
func ownBinarySHA256() string {
	binaryOnce.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			return
		}
		f, err := os.Open(exe)
		if err != nil {
			return
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err == nil {
			binarySum = hex.EncodeToString(h.Sum(nil))
		}
	})
	return binarySum
}

// onBinary tells agents which binary to run (agent self-update).
func (s *Server) onBinary(_ context.Context, _ *struct{}) *proto.BinaryInfo {
	return &proto.BinaryInfo{
		Arch: runtime.GOARCH, SHA256: ownBinarySHA256(),
		URL: s.cfg.PublicURL + "/download/paasd-linux-" + runtime.GOARCH,
	}
}
