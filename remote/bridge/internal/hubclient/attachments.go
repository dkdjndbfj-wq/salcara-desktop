package hubclient

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Phone images arrive in small chunks (Hub commands are capped at 256 KiB), are
// assembled in a private temp folder and handed to the agent as local files.
// They never leave this computer and are deleted after a day.

const (
	maxAttachmentChunk  = 200_000 // base64 characters per command
	maxAttachmentChunks = 64
	maxAttachmentBytes  = 8 << 20
	attachmentTTL       = 24 * time.Hour
	maxTurnAttachments  = 4
)

var attachmentID = regexp.MustCompile(`^att_[a-f0-9]{32}$`)

var attachmentExt = map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp"}

type attachmentUpload struct {
	mime    string
	total   int
	chunks  map[int][]byte
	size    int
	started time.Time
}

type attachmentStore struct {
	mu      sync.Mutex
	dir     string
	pending map[string]*attachmentUpload
}

func newAttachmentStore() *attachmentStore {
	return &attachmentStore{dir: filepath.Join(os.TempDir(), "salcara-bridge", "attachments"), pending: map[string]*attachmentUpload{}}
}

func (s *attachmentStore) path(id, mime string) string {
	return filepath.Join(s.dir, id+attachmentExt[mime])
}

// find returns the assembled file for id, if complete.
func (s *attachmentStore) find(id string) (string, bool) {
	if !attachmentID.MatchString(id) {
		return "", false
	}
	for mime := range attachmentExt {
		p := s.path(id, mime)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p, true
		}
	}
	return "", false
}

// put stores one chunk; complete is true once the whole image is on disk.
func (s *attachmentStore) put(id, mime string, index, total int, data string) (bool, error) {
	if !attachmentID.MatchString(id) {
		return false, errors.New("无效的图片编号")
	}
	if _, ok := attachmentExt[mime]; !ok {
		return false, errors.New("只支持 JPEG、PNG、WebP 图片")
	}
	if total < 1 || total > maxAttachmentChunks || index < 0 || index >= total || len(data) > maxAttachmentChunk {
		return false, errors.New("图片分片无效")
	}
	if _, done := s.find(id); done {
		return true, nil // a retried chunk of a finished upload
	}
	raw := []byte(data) // base64 text; decoded once all chunks are in
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	u := s.pending[id]
	if u == nil {
		u = &attachmentUpload{mime: mime, total: total, chunks: map[int][]byte{}, started: time.Now()}
		s.pending[id] = u
	}
	if u.mime != mime || u.total != total {
		return false, errors.New("图片分片不一致")
	}
	if old, seen := u.chunks[index]; seen {
		u.size -= len(old)
	}
	u.chunks[index] = raw
	u.size += len(raw)
	if u.size/4*3 > maxAttachmentBytes {
		delete(s.pending, id)
		return false, errors.New("图片太大（最大 8 MB）")
	}
	if len(u.chunks) < u.total {
		return false, nil
	}
	text := make([]byte, 0, u.size)
	for i := 0; i < u.total; i++ {
		text = append(text, u.chunks[i]...)
	}
	delete(s.pending, id)
	buf, err := base64.StdEncoding.DecodeString(string(text))
	if err != nil {
		return false, errors.New("图片数据无效")
	}
	if !imageMagic(buf, mime) {
		return false, errors.New("图片内容和类型不符")
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return false, err
	}
	tmp := s.path(id, mime) + ".part"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, s.path(id, mime))
}

// resolve maps ids from a turn to local files; every id must be complete.
func (s *attachmentStore) resolve(ids []string) ([]string, error) {
	if len(ids) > maxTurnAttachments {
		return nil, errors.New("一次最多发送 4 张图片")
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		p, ok := s.find(id)
		if !ok {
			return nil, errors.New("图片还没有传完，请重新发送")
		}
		out = append(out, p)
	}
	return out, nil
}

func (s *attachmentStore) sweepLocked() {
	now := time.Now()
	for id, u := range s.pending {
		if now.Sub(u.started) > time.Hour {
			delete(s.pending, id)
		}
	}
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > attachmentTTL && strings.HasPrefix(e.Name(), "att_") {
			_ = os.Remove(filepath.Join(s.dir, e.Name()))
		}
	}
}

func imageMagic(b []byte, mime string) bool {
	switch mime {
	case "image/jpeg":
		return len(b) > 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF
	case "image/png":
		return len(b) > 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n"
	case "image/webp":
		return len(b) > 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP"
	}
	return false
}

// stringList reads a JSON array of strings from a command field.
func stringList(cmd map[string]any, key string) []string {
	raw, _ := cmd[key].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
