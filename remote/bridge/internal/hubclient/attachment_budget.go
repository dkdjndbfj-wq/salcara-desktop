package hubclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type attachmentLimits struct {
	pendingFiles int
	pendingBytes int // base64 bytes, not decoded bytes
	cacheFiles   int
	cacheBytes   int64
}

func (s *attachmentStore) effectiveLimits() attachmentLimits {
	l := s.limits
	if l.pendingFiles <= 0 {
		l.pendingFiles = 8
	}
	// Four maximum-size images in one turn require ~43 MiB of base64.
	if l.pendingBytes <= 0 {
		l.pendingBytes = 48 << 20
	}
	if l.cacheFiles <= 0 {
		l.cacheFiles = 1024
	}
	if l.cacheBytes <= 0 {
		l.cacheBytes = 256 << 20
	}
	return l
}

func readAttachmentFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = errors.New("附件文件过大")
	}
	return b, err
}

func (s *attachmentStore) dropPendingLocked(id string) {
	if u := s.pending[id]; u != nil {
		s.pendingBytes -= u.size
		delete(s.pending, id)
		for i := 0; i < u.total; i++ {
			_ = os.Remove(s.chunkPath(id, i))
		}
		_ = os.Remove(s.manifestPath(id))
	}
}

// Short chunks do not rescan the entire cache or rewrite its index. Cache
// counters are refreshed periodically and incremented under the same lock as
// publication. In-flight, unexpired images are never evicted to admit new ones.
func (s *attachmentStore) sweepLocked() {
	s.recoverPendingLocked()
	now := time.Now()
	for id, u := range s.pending {
		if now.Sub(u.started) > time.Hour {
			s.dropPendingLocked(id)
		}
	}
	if !s.lastSweep.IsZero() && now.Sub(s.lastSweep) < time.Minute {
		return
	}
	s.sweepOrphanChunksLocked(now)
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		// Windows can report PATH_NOT_FOUND when ReadDir targets a regular
		// file. Only a genuinely absent cache root is an empty inventory.
		_, statErr := os.Lstat(s.dir)
		if !errors.Is(err, os.ErrNotExist) || !errors.Is(statErr, os.ErrNotExist) {
			s.lastSweep = time.Time{}
			return
		}
	}
	s.lastSweep, s.cacheBytes, s.cacheFiles = now, 0, 0
	live := map[string]bool{}
	for _, e := range entries {
		id, suffix, found := strings.Cut(e.Name(), ".")
		if !found || !attachmentID.MatchString(id) {
			continue
		}
		valid := false
		for _, ext := range attachmentExt {
			if "."+suffix == ext || "."+suffix == ext+".part" {
				valid = true
				break
			}
		}
		if !valid {
			continue
		}
		info, err := e.Info()
		if err != nil {
			s.lastSweep = time.Time{}
			return
		}
		if !info.Mode().IsRegular() {
			continue
		}
		ttl := attachmentTTL
		if strings.HasSuffix(suffix, ".part") {
			ttl = time.Hour
		}
		if now.Sub(info.ModTime()) > ttl && s.leases[id] == 0 && !s.outboxPins[id] && os.Remove(filepath.Join(s.dir, e.Name())) == nil {
			continue
		}
		s.cacheFiles++
		s.cacheBytes += info.Size()
		if !strings.HasSuffix(suffix, ".part") && (now.Sub(info.ModTime()) <= attachmentTTL || s.leases[id] > 0 || s.outboxPins[id]) {
			live[id] = true
		}
	}
	changed := false
	for id := range s.bindings {
		if !live[id] {
			delete(s.bindings, id)
			changed = true
		}
	}
	if changed {
		s.saveBindingsLocked()
	}
}

func (s *attachmentStore) writeImageLocked(id, mime string, data []byte) error {
	return s.writeImageReaderLocked(id, mime, bytes.NewReader(data), int64(len(data)))
}

func (s *attachmentStore) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		s.sweepLocked()
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
