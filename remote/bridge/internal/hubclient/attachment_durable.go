package hubclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type diskUpload struct {
	MIME    string
	Total   int
	Sizes   map[int]int
	Updated time.Time
}

var attachmentDiskBudgetMu sync.Mutex

func (s *attachmentStore) persistChunkLocked(id string, index int, data []byte) error {
	attachmentDiskBudgetMu.Lock()
	defer attachmentDiskBudgetMu.Unlock()
	root := s.budgetDir
	if root == "" {
		root = s.dir
	}
	var size int64
	uploads := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, e os.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if e.IsDir() {
			return nil
		}
		id, _, _ := strings.Cut(e.Name(), ".")
		if !attachmentID.MatchString(id) || !strings.HasSuffix(e.Name(), ".chunk") {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			size += info.Size()
			uploads[filepath.Join(filepath.Dir(path), id)] = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	l := s.effectiveLimits()
	if size+int64(len(data)) > int64(l.pendingBytes) || !uploads[filepath.Join(s.dir, "pending", id)] && len(uploads) >= l.pendingFiles {
		return errors.New("图片上传缓存已满，请稍后重试")
	}
	return durableWrite(s.chunkPath(id, index), data)
}

// All phone generations share one disk ceiling. Rebinding cannot multiply
// the cache allowance; only generated attachment files enter this inventory.
func (s *attachmentStore) aggregateCapacityLocked(extra int64) error {
	if s.budgetDir == "" {
		return nil
	}
	var size int64
	var count int
	err := filepath.WalkDir(s.budgetDir, func(path string, e os.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if e.IsDir() {
			return nil
		}
		id, suffix, ok := strings.Cut(e.Name(), ".")
		if !ok || !attachmentID.MatchString(id) {
			return nil
		}
		if suffix != "png" && suffix != "jpg" && suffix != "webp" && suffix != "png.part" && suffix != "jpg.part" && suffix != "webp.part" {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			size += info.Size()
			count++
		}
		return nil
	})
	if err != nil {
		return err
	}
	limits := s.effectiveLimits()
	if count >= limits.cacheFiles || size+extra > limits.cacheBytes {
		return errors.New("图片缓存已满，请稍后重试")
	}
	return nil
}

func (s *attachmentStore) chunkPath(id string, index int) string {
	return filepath.Join(s.dir, "pending", fmt.Sprintf("%s.%02d.chunk", id, index))
}
func (s *attachmentStore) manifestPath(id string) string {
	return filepath.Join(s.dir, "pending", id+".json")
}

func (s *attachmentStore) recoverPendingLocked() {
	if s.pendingLoaded {
		return
	}
	s.pendingLoaded = true
	if s.pending == nil {
		s.pending = map[string]*attachmentUpload{}
	}
	entries, _ := os.ReadDir(filepath.Join(s.dir, "pending"))
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := entry.Name()[:len(entry.Name())-5]
		if !attachmentID.MatchString(id) {
			continue
		}
		b, err := readAttachmentFile(s.manifestPath(id), 16<<10)
		if err != nil {
			continue
		}
		var m diskUpload
		if json.Unmarshal(b, &m) != nil || m.Total < 1 || m.Total > maxAttachmentChunks || attachmentExt[m.MIME] == "" {
			continue
		}
		u := &attachmentUpload{mime: m.MIME, total: m.Total, chunks: map[int][]byte{}, sizes: map[int]int{}, started: m.Updated}
		for i, size := range m.Sizes {
			if i < 0 || i >= m.Total || size < 0 || size > maxAttachmentChunk {
				continue
			}
			info, e := os.Lstat(s.chunkPath(id, i))
			if e == nil && info.Mode().IsRegular() && info.Size() == int64(size) {
				u.sizes[i] = size
				u.size += size
			}
		}
		s.pending[id] = u
		s.pendingBytes += u.size
	}
}

func (s *attachmentStore) putDiskChunkLocked(id, mime string, index, total int, data string) (bool, error) {
	u := s.pending[id]
	if u == nil {
		if len(s.pending) >= s.effectiveLimits().pendingFiles {
			return false, errors.New("正在上传的图片过多，请稍后重试")
		}
		u = &attachmentUpload{mime: mime, total: total, sizes: map[int]int{}, started: time.Now()}
	}
	if u.mime != mime || u.total != total {
		return false, errors.New("图片分片不一致")
	}
	if oldSize, exists := u.sizes[index]; exists {
		old, err := readAttachmentFile(s.chunkPath(id, index), maxAttachmentChunk)
		if err != nil {
			return false, err
		}
		if oldSize != len(data) || !bytes.Equal(old, []byte(data)) {
			return false, errors.New("图片分片重复但内容不一致")
		}
	} else {
		if u.size+len(data) > base64.StdEncoding.EncodedLen(maxAttachmentBytes) {
			return false, errors.New("图片太大（最大 8 MB）")
		}
		if s.pendingBytes+len(data) > s.effectiveLimits().pendingBytes {
			return false, errors.New("图片上传缓存已满，请稍后重试")
		}
		if err := s.persistChunkLocked(id, index, []byte(data)); err != nil {
			return false, err
		}
		u.sizes[index] = len(data)
		u.size += len(data)
		s.pendingBytes += len(data)
		s.pending[id] = u
	}
	u.started = time.Now()
	b, _ := json.Marshal(diskUpload{u.mime, u.total, u.sizes, u.started})
	if err := durableWrite(s.manifestPath(id), b); err != nil {
		return false, err
	}
	if len(u.sizes) != u.total {
		return false, nil
	}
	// At most one encoded chunk and the decoder's small buffer are resident.
	reader := &attachmentChunkReader{store: s, id: id, total: u.total}
	err := s.writeImageReaderLocked(id, mime, base64.NewDecoder(base64.StdEncoding.Strict(), reader), int64(base64.StdEncoding.DecodedLen(u.size)))
	reader.Close()
	if err != nil {
		return false, err
	}
	s.dropPendingLocked(id)
	return true, nil
}

type attachmentChunkReader struct {
	store        *attachmentStore
	id           string
	total, index int
	current      *os.File
}

func (r *attachmentChunkReader) Read(p []byte) (int, error) {
	for r.index < r.total {
		if r.current == nil {
			f, err := os.Open(r.store.chunkPath(r.id, r.index))
			if err != nil {
				return 0, err
			}
			r.current = f
		}
		n, err := r.current.Read(p)
		if err == io.EOF {
			r.current.Close()
			r.current = nil
			r.index++
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
	return 0, io.EOF
}
func (r *attachmentChunkReader) Close() {
	if r.current != nil {
		r.current.Close()
		r.current = nil
	}
}

func (s *attachmentStore) writeImageReaderLocked(id, mime string, reader io.Reader, size int64) error {
	attachmentDiskBudgetMu.Lock()
	defer attachmentDiskBudgetMu.Unlock()
	part := s.path(id, mime) + ".part"
	if info, err := os.Lstat(part); err == nil && info.Mode().IsRegular() {
		if err = os.Remove(part); err != nil {
			return err
		}
	}
	if err := s.aggregateCapacityLocked(size); err != nil {
		return err
	}
	l := s.effectiveLimits()
	if s.cacheFiles >= l.cacheFiles || s.cacheBytes+size > l.cacheBytes {
		s.lastSweep = time.Time{}
		s.sweepLocked()
	}
	if s.lastSweep.IsZero() || s.cacheFiles >= l.cacheFiles || s.cacheBytes+size > l.cacheBytes {
		return errors.New("图片缓存已满，请稍后重试")
	}
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path(id, mime)+".part", os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	n, err := io.Copy(f, io.LimitReader(reader, maxAttachmentBytes+1))
	if err == nil && n > maxAttachmentBytes {
		err = errors.New("图片太大（最大 8 MB）")
	}
	if err == nil {
		var header [16]byte
		read, _ := f.ReadAt(header[:], 0)
		if !imageMagic(header[:read], mime) {
			err = errors.New("图片内容和类型不符")
		}
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, s.path(id, mime)); err != nil {
		return err
	}
	s.cacheFiles++
	s.cacheBytes += n
	return nil
}

// Active adapter reads receive a bounded lease. Session history alone never
// extends the 24h cache TTL; otherwise a session reference could leak forever.
func (s *attachmentStore) lease(ids []string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leases == nil {
		s.leases = map[string]int{}
	}
	for _, id := range ids {
		if _, ok := s.find(id); !ok {
			return nil, errors.New("图片已过期，请重新发送")
		}
	}
	for _, id := range ids {
		s.leases[id]++
	}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, id := range ids {
			s.leases[id]--
			if s.leases[id] <= 0 {
				delete(s.leases, id)
			}
		}
	}, nil
}

func (c *Client) cleanupAttachmentStores(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	lastReceipts := time.Time{}
	for {
		c.reconcileOutboxIdentity()
		c.attachmentMu.Lock()
		for _, s := range c.attachmentStores {
			s.mu.Lock()
			s.sweepLocked()
			s.mu.Unlock()
		}
		c.attachmentMu.Unlock()
		c.sweepRetiredAttachmentStores()
		if c.durable != nil && time.Since(lastReceipts) > time.Hour {
			c.durable.expireReceipts()
			lastReceipts = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *attachmentStore) sweepOrphanChunksLocked(now time.Time) {
	dir := filepath.Join(s.dir, "pending")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		id, _, _ := strings.Cut(e.Name(), ".")
		if !strings.HasPrefix(e.Name(), ".durable-") && (!attachmentID.MatchString(id) || s.pending[id] != nil) {
			continue
		}
		info, err := e.Info()
		if err == nil && info.Mode().IsRegular() && now.Sub(info.ModTime()) > time.Hour {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
