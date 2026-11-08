package hubclient

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"salcara/bridge/internal/protocol"
)

// Phone images arrive in small chunks (Hub commands are capped at 256 KiB), are
// assembled from durable disk chunks in a private managed folder and handed to the agent as local files.
// The agent can forward them to its selected provider. Phone reads remain
// session-bound; the bounded local cache expires after a day.

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
	chunks  map[int][]byte // retained for compatibility; chunk data lives on disk
	sizes   map[int]int
	size    int
	started time.Time
}

type attachmentStore struct {
	mu            sync.Mutex
	dir           string
	budgetDir     string
	bindingsFile  string
	pending       map[string]*attachmentUpload
	pendingBytes  int
	pendingLoaded bool
	leases        map[string]int
	outboxPins    map[string]bool
	limits        attachmentLimits
	lastSweep     time.Time
	cacheBytes    int64
	cacheFiles    int
	// bindings survives a restart, but does not extend the image TTL. A phone can only read an
	// attachment after the worker has associated it with the exact session that
	// received it; an id from another session is not enough authorization.
	bindings map[string]map[string]bool
}

func newAttachmentStore() *attachmentStore {
	bindingsFile := ""
	if root, err := os.UserConfigDir(); err == nil {
		bindingsFile = filepath.Join(root, "SalcaraBridge", "attachment-bindings.json")
	}
	s := &attachmentStore{dir: filepath.Join(filepath.Dir(bindingsFile), "remote-store", "attachments"), bindingsFile: bindingsFile, pending: map[string]*attachmentUpload{}, bindings: map[string]map[string]bool{}}
	s.loadBindings()
	return s
}

func (s *attachmentStore) loadBindings() {
	if s.bindingsFile == "" {
		return
	}
	b, err := readAttachmentFile(s.bindingsFile, 512<<10)
	if err != nil || len(b) > 512<<10 {
		return
	}
	var raw map[string][]string
	if json.Unmarshal(b, &raw) != nil {
		return
	}
	for id, keys := range raw {
		if !attachmentID.MatchString(id) || len(keys) > 128 {
			continue
		}
		for _, key := range keys {
			if len(key) == 0 || len(key) > 256 || strings.ContainsAny(key, "\r\n\x00") {
				continue
			}
			if s.bindings[id] == nil {
				s.bindings[id] = map[string]bool{}
			}
			s.bindings[id][key] = true
		}
	}
}

func (s *attachmentStore) saveBindingsLocked() error {
	if s.bindingsFile == "" {
		return nil
	}
	raw := map[string][]string{}
	for id, keys := range s.bindings {
		for key := range keys {
			raw[id] = append(raw[id], key)
		}
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	if len(b) > 512<<10 {
		return errors.New("图片引用记录已满")
	}
	return durableWrite(s.bindingsFile, b)
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
		if st, err := os.Lstat(p); err == nil && st.Mode().IsRegular() && st.Size() <= maxAttachmentBytes && (time.Since(st.ModTime()) <= attachmentTTL || s.leases[id] > 0 || s.outboxPins[id]) {
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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	if _, done := s.find(id); done {
		return true, nil // a retried chunk of a finished upload
	}
	return s.putDiskChunkLocked(id, mime, index, total, data)
}

// resolve maps ids from a turn to local files; every id must be complete.
func (s *attachmentStore) resolve(ids []string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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

// bind associates completed uploads with one logical worker session.  It is
// called only after the session id is known, so a failed start cannot expose an
// upload to an unrelated conversation.
func (s *attachmentStore) bind(ids []string, sessionKey string) error {
	if sessionKey == "" || len(ids) > maxTurnAttachments {
		return errors.New("附件会话无效")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := map[string]map[string]bool{}
	for _, id := range ids {
		copy := map[string]bool{}
		for key, value := range s.bindings[id] {
			copy[key] = value
		}
		previous[id] = copy
		if len(copy) >= 128 && !copy[sessionKey] {
			return errors.New("图片引用数量超过上限")
		}
		if !attachmentID.MatchString(id) {
			return errors.New("无效的图片编号")
		}
		if _, ok := s.find(id); !ok {
			return errors.New("图片还没有传完，请重新发送")
		}
		if s.bindings[id] == nil {
			s.bindings[id] = map[string]bool{}
		}
		s.bindings[id][sessionKey] = true
	}
	if err := s.saveBindingsLocked(); err != nil {
		for id, keys := range previous {
			s.bindings[id] = keys
		}
		return err
	}
	return nil
}

// read returns one base64 chunk.  It never accepts a path and it refuses a
// valid id that was not bound to the requested session.
func (s *attachmentStore) read(id, sessionKey string, offset, length int) (map[string]any, error) {
	if !attachmentID.MatchString(id) || sessionKey == "" {
		return nil, errors.New("附件读取请求无效")
	}
	if offset < 0 || offset > maxAttachmentBytes || length <= 0 || length > 90_000 {
		return nil, errors.New("附件读取范围无效")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	allowed := s.bindings[id] != nil && s.bindings[id][sessionKey]
	path, ok := s.find(id)
	if !allowed || !ok {
		return nil, errors.New("附件不属于当前会话")
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() > maxAttachmentBytes {
		return nil, errors.New("附件已过期，请重新发送")
	}
	if int64(offset) > info.Size() {
		return nil, errors.New("附件读取位置无效")
	}
	end := offset + length
	if int64(end) > info.Size() {
		end = int(info.Size())
	}
	// Keep every chunk on a 3-byte boundary, except the final chunk.  This
	// allows the phone to concatenate base64 strings without corrupting it.
	if int64(end) < info.Size() {
		end -= (end - offset) % 3
		if end <= offset {
			end = offset + 3
		}
	}
	chunk := make([]byte, end-offset)
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("附件已过期，请重新发送")
	}
	n, readErr := f.ReadAt(chunk, int64(offset))
	_ = f.Close()
	if n != len(chunk) || readErr != nil && readErr != io.EOF {
		return nil, errors.New("附件读取失败")
	}
	data := base64.StdEncoding.EncodeToString(chunk)
	mime := "image/jpeg"
	for candidate, ext := range attachmentExt {
		if strings.HasSuffix(path, ext) {
			mime = candidate
			break
		}
	}
	return map[string]any{"id": id, "mime": mime, "size": info.Size(), "offset": offset, "nextOffset": end,
		"data": data, "complete": int64(end) >= info.Size()}, nil
}

// ref builds path-free metadata; bytes are fetched separately by session.
func (s *attachmentStore) ref(id string) (map[string]any, bool) {
	if !attachmentID.MatchString(id) {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path, ok := s.find(id)
	if !ok {
		return nil, false
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() > maxAttachmentBytes {
		return nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(f, maxAttachmentBytes+1))
	if err != nil || n != info.Size() {
		return nil, false
	}
	digest := hash.Sum(nil)
	mime := "image/jpeg"
	for candidate, ext := range attachmentExt {
		if strings.HasSuffix(path, ext) {
			mime = candidate
			break
		}
	}
	return map[string]any{"id": id, "mime": mime, "size": info.Size(), "sha256": hex.EncodeToString(digest)}, true
}

func (s *attachmentStore) refs(ids []string) []protocol.AttachmentRef {
	if len(ids) > maxTurnAttachments {
		return nil
	}
	out := make([]protocol.AttachmentRef, 0, len(ids))
	for _, id := range ids {
		meta, ok := s.ref(id)
		if !ok {
			continue
		}
		mime, _ := meta["mime"].(string)
		size, _ := meta["size"].(int64)
		sha, _ := meta["sha256"].(string)
		out = append(out, protocol.AttachmentRef{ID: id, Mime: mime, Size: size, SHA256: sha})
	}
	return out
}

// importFile snapshots an agent-produced image (for example Codex's
// imageView item) into the same bounded store as phone uploads. The phone
// receives an opaque id, never the desktop path. A content-derived id avoids
// duplicating the same generated image when a history page and the live event
// are both read.
func (s *attachmentStore) importFile(sessionKey, source string) (protocol.AttachmentRef, bool) {
	if sessionKey == "" || source == "" || !filepath.IsAbs(source) {
		return protocol.AttachmentRef{}, false
	}
	// Resolve symlinks to prevent traversal to paths outside the intended scope.
	// os.Stat follows symlinks; use os.Lstat first to detect them, then
	// filepath.EvalSymlinks to get the real path for a final size/type check.
	linfo, lerr := os.Lstat(source)
	if lerr != nil {
		return protocol.AttachmentRef{}, false
	}
	if linfo.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(source)
		if err != nil {
			return protocol.AttachmentRef{}, false
		}
		source = resolved
	}
	info, err := os.Stat(source)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxAttachmentBytes {
		return protocol.AttachmentRef{}, false
	}
	mime, ok := imageMimeForPath(source)
	if !ok {
		return protocol.AttachmentRef{}, false
	}
	b, err := readAttachmentFile(source, maxAttachmentBytes)
	if err != nil || !imageMagic(b, mime) {
		return protocol.AttachmentRef{}, false
	}
	return s.importBytes(sessionKey, mime, b)
}

// importBytes stores provider-produced image bytes in the same private,
// session-bound store as images uploaded from the phone.  Callers must prove
// the MIME by magic bytes before reaching this function.
func (s *attachmentStore) importBytes(sessionKey, mime string, b []byte) (protocol.AttachmentRef, bool) {
	if sessionKey == "" || len(b) == 0 || len(b) > maxAttachmentBytes || !imageMagic(b, mime) {
		return protocol.AttachmentRef{}, false
	}
	digest := sha256.Sum256(b)
	id := "att_" + hex.EncodeToString(digest[:])[:32]
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	if existing, found := s.find(id); found {
		if st, statErr := os.Stat(existing); statErr == nil && st.Size() == int64(len(b)) {
			if s.bindings[id] == nil {
				s.bindings[id] = map[string]bool{}
			}
			s.bindings[id][sessionKey] = true
			if err := s.saveBindingsLocked(); err != nil {
				return protocol.AttachmentRef{}, false
			}
			return protocol.AttachmentRef{ID: id, Mime: mime, Size: int64(len(b)), SHA256: hex.EncodeToString(digest[:])}, true
		}
	}
	if err := s.writeImageLocked(id, mime, b); err != nil {
		return protocol.AttachmentRef{}, false
	}
	if s.bindings[id] == nil {
		s.bindings[id] = map[string]bool{}
	}
	s.bindings[id][sessionKey] = true
	if err := s.saveBindingsLocked(); err != nil {
		return protocol.AttachmentRef{}, false
	}
	return protocol.AttachmentRef{ID: id, Mime: mime, Size: int64(len(b)), SHA256: hex.EncodeToString(digest[:])}, true
}

// importBase64 accepts only the private marker emitted by codex_map.  It also
// accepts a data URL because a few app-server builds wrap result that way;
// the decoded bytes still have to pass an image magic check and the 8 MiB cap.
func (s *attachmentStore) importBase64(sessionKey, value string) (protocol.AttachmentRef, bool) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "data:") {
		comma := strings.IndexByte(value, ',')
		if comma < 0 || !strings.Contains(strings.ToLower(value[:comma]), ";base64") {
			return protocol.AttachmentRef{}, false
		}
		value = value[comma+1:]
	}
	if len(value) < 16 || len(value) > 12<<20 {
		return protocol.AttachmentRef{}, false
	}
	b, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil || len(b) == 0 || len(b) > maxAttachmentBytes {
		return protocol.AttachmentRef{}, false
	}
	mime, ok := imageMimeForBytes(b)
	if !ok {
		return protocol.AttachmentRef{}, false
	}
	return s.importBytes(sessionKey, mime, b)
}

func imageMimeForPath(path string) (string, bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg", true
	case ".png":
		return "image/png", true
	case ".webp":
		return "image/webp", true
	default:
		return "", false
	}
}

func imageMimeForBytes(b []byte) (string, bool) {
	for _, mime := range []string{"image/png", "image/jpeg", "image/webp"} {
		if imageMagic(b, mime) {
			return mime, true
		}
	}
	return "", false
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
	if raw, ok := cmd[key].([]string); ok {
		return append([]string(nil), raw...)
	}
	raw, _ := cmd[key].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
