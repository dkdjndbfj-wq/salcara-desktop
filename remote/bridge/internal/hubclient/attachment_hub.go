package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func (c *Client) attachmentsFor(ctx context.Context) *attachmentStore {
	if c.o.Store == nil {
		return phoneAttachments
	}
	return c.attachmentsForConfig(ctx, c.o.Store.Get())
}

func (c *Client) attachmentsForConfig(ctx context.Context, cfg config.Config) *attachmentStore {
	if phoneAttachments != defaultPhoneAttachments || c.o.Store == nil {
		return phoneAttachments
	}
	scope := eventIdentity(cfg)
	if eventScope := c.eventScopeFromContext(ctx, cfg); eventScope != "" && c.eventScopeRequired(cfg) {
		scope += "\x00" + eventScope
	}
	if IsDirectExecution(ctx) {
		scope = "direct:" + cfg.ComputerID + ":" + cfg.DirectPhoneID + ":" + cfg.DirectPhoneHash
	}
	if isLocalExecution(ctx) && !IsDirectExecution(ctx) {
		scope = "local:" + cfg.ComputerID
	}
	key := durableHash(scope)
	c.attachmentMu.Lock()
	defer c.attachmentMu.Unlock()
	if c.attachmentStores == nil {
		c.attachmentStores = map[string]*attachmentStore{}
	}
	if s := c.attachmentStores[key]; s != nil {
		return s
	}
	dir := filepath.Join(filepath.Dir(c.o.Store.Path()), "remote-store", "attachments", key)
	s := &attachmentStore{dir: dir, budgetDir: filepath.Dir(dir), bindingsFile: filepath.Join(dir, "bindings.json"), pending: map[string]*attachmentUpload{}, bindings: map[string]map[string]bool{}}
	s.loadBindings()
	c.attachmentStores[key] = s
	return s
}

func (c *Client) ensureCommandAttachments(ctx context.Context, cfg config.Config, s *attachmentStore, cmd map[string]any) error {
	ids := stringList(cmd, "attachments")
	if len(ids) > maxTurnAttachments {
		return errors.New("一次最多发送 4 张图片")
	}
	for _, id := range ids {
		if !attachmentID.MatchString(id) {
			return errors.New("无效的图片编号")
		}
		s.mu.Lock()
		_, found := s.find(id)
		s.mu.Unlock()
		if found {
			continue
		}
		if isLocalExecution(ctx) {
			return errors.New("图片还没有传完，请重新发送")
		}
		if err := c.fetchHubAttachment(ctx, cfg, s, id); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) fetchHubAttachment(ctx context.Context, cfg config.Config, s *attachmentStore, id string) error {
	req, err := c.requestFor(ctx, cfg, http.MethodGet, "/bridge/blobs?deviceId="+url.QueryEscape(cfg.DeviceID)+"&id="+url.QueryEscape(id), nil, false)
	if err != nil {
		return err
	}
	resp, err := c.o.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("图片获取失败，请重新上传（%d）", resp.StatusCode)
	}
	mime := strings.Split(resp.Header.Get("Content-Type"), ";")[0]
	if attachmentExt[mime] == "" || resp.ContentLength > maxAttachmentBytes {
		return errors.New("图片格式或大小无效")
	}
	if eventIdentity(cfg) != eventIdentity(c.o.Store.Get()) {
		return errors.New("连接已变化，请重新发送图片")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	if _, found := s.find(id); found {
		return nil
	}
	size := resp.ContentLength
	if size < 0 {
		size = maxAttachmentBytes
	}
	return s.writeImageReaderLocked(id, mime, resp.Body, size)
}

// Upload bytes before the containing event batch. Stable attachment IDs make a
// lost upload ACK harmless. Old stations keep their existing Bridge read path.
func (c *Client) uploadBatchAttachments(ctx context.Context, cfg config.Config, events []protocol.Event) error {
	s := c.attachmentsForConfig(ctx, cfg)
	seen := map[string]bool{}
	for _, ev := range events {
		for _, ref := range ev.Attachments {
			if seen[ref.ID] {
				continue
			}
			seen[ref.ID] = true
			s.mu.Lock()
			path, found := s.find(ref.ID)
			if !found {
				s.mu.Unlock()
				continue
			} // expired refs remain explicit, text can still recover
			f, err := os.Open(path)
			s.mu.Unlock()
			if err != nil {
				return err
			}
			req, err := c.requestFor(ctx, cfg, http.MethodPost, "/bridge/blobs?deviceId="+url.QueryEscape(cfg.DeviceID)+"&attachmentId="+url.QueryEscape(ref.ID), f, true)
			if err != nil {
				f.Close()
				return err
			}
			req.Header.Set("Content-Type", ref.Mime)
			req.ContentLength = ref.Size
			resp, err := c.o.HTTP.Do(req)
			f.Close()
			if err != nil {
				return err
			}
			if resp.StatusCode == http.StatusNotFound {
				resp.Body.Close()
				return nil
			}
			if resp.StatusCode/100 != 2 {
				err = checkResp(resp)
				resp.Body.Close()
				return err
			}
			var receipt struct {
				ID     string `json:"id"`
				BlobID string `json:"blobId"`
			}
			err = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&receipt)
			resp.Body.Close()
			if err != nil || (receipt.ID != ref.ID && receipt.BlobID != ref.ID) {
				return errors.New("图片存储回执不一致")
			}
		}
	}
	return nil
}

func (c *Client) syncOutboxAttachmentPinsLocked() {
	ctx := context.WithValue(context.Background(), eventScopeContextKey{}, c.queueEventScope)
	s := c.attachmentsForConfig(ctx, c.queueConfig)
	c.attachmentMu.Lock()
	for _, old := range c.attachmentStores {
		if old != s {
			old.mu.Lock()
			old.outboxPins = nil
			old.mu.Unlock()
		}
	}
	c.attachmentMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outboxPins = map[string]bool{}
	for _, ev := range c.queue {
		for _, ref := range ev.Attachments {
			s.outboxPins[ref.ID] = true
		}
	}
}

// Sweep former identity directories too. They must not accumulate indefinitely
// after a phone rebind. Only this component's named, private cache is scanned.
func (c *Client) sweepRetiredAttachmentStores() {
	if c.o.Store == nil {
		return
	}
	root := filepath.Join(filepath.Dir(c.o.Store.Path()), "remote-store", "attachments")
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if !e.IsDir() || len(e.Name()) != 64 {
			continue
		}
		c.attachmentMu.Lock()
		loaded := c.attachmentStores[e.Name()] != nil
		c.attachmentMu.Unlock()
		if loaded {
			continue
		}
		dir := filepath.Join(root, e.Name())
		s := &attachmentStore{dir: dir, bindingsFile: filepath.Join(dir, "bindings.json"), pending: map[string]*attachmentUpload{}, bindings: map[string]map[string]bool{}}
		s.loadBindings()
		s.sweepLocked()
	}
}
