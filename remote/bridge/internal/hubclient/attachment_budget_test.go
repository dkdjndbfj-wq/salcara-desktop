package hubclient

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func budgetAttachmentID(i int) string  { return fmt.Sprintf("att_%032x", i) }
func budgetImage(suffix string) []byte { return []byte("\x89PNG\r\n\x1a\n" + suffix) }

func TestAttachmentFailedInventoryCannotReuseStaleCapacity(t *testing.T) {
	s := testAttachmentStore(t)
	s.dir = filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(s.dir, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.lastSweep = time.Now().Add(-2 * time.Minute)
	s.sweepLocked()
	if !s.lastSweep.IsZero() {
		t.Fatal("failed inventory retained a trusted capacity snapshot")
	}
	if err := s.writeImageLocked(budgetAttachmentID(1), "image/png", budgetImage("test")); err == nil {
		t.Fatal("unknown capacity accepted a new image")
	}
}

func TestAttachmentPendingBudgetsAndExpiry(t *testing.T) {
	s := testAttachmentStore(t)
	s.limits = attachmentLimits{pendingFiles: 2, pendingBytes: 12}
	for i := 1; i <= 2; i++ {
		if done, err := s.put(budgetAttachmentID(i), "image/png", 0, 3, "AAAA"); err != nil || done {
			t.Fatal(done, err)
		}
	}
	if _, err := s.put(budgetAttachmentID(3), "image/png", 0, 3, "AAAA"); err == nil {
		t.Fatal("unbounded pending count")
	}
	if _, err := s.put(budgetAttachmentID(1), "image/png", 1, 3, "AAAAAAAA"); err == nil {
		t.Fatal("unbounded pending bytes")
	}
	if _, err := s.put(budgetAttachmentID(1), "image/png", 0, 3, "AAAA"); err != nil {
		t.Fatal("same chunk retry consumed budget", err)
	}
	if s.pendingBytes != 8 {
		t.Fatal("rejected/duplicate chunks changed accounting")
	}
	s.pending[budgetAttachmentID(1)].started = time.Now().Add(-2 * time.Hour)
	if _, err := s.put(budgetAttachmentID(3), "image/png", 0, 3, "AAAA"); err != nil {
		t.Fatal("expired upload not reclaimed", err)
	}
	if s.pendingBytes != 8 || len(s.pending) != 2 {
		t.Fatal("expiry accounting mismatch")
	}
}

func TestAttachmentCompletedBudgetsDoNotEvictLiveImages(t *testing.T) {
	for _, mode := range []string{"files", "bytes"} {
		t.Run(mode, func(t *testing.T) {
			s := testAttachmentStore(t)
			if mode == "files" {
				s.limits.cacheFiles = 1
			} else {
				s.limits.cacheBytes = int64(len(budgetImage("first")))
			}
			ref, ok := s.importBytes("codex:one", "image/png", budgetImage("first"))
			if !ok {
				t.Fatal("first image rejected")
			}
			if _, ok := s.importBytes("codex:two", "image/png", budgetImage("other")); ok {
				t.Fatal("full cache accepted image")
			}
			if _, err := s.read(ref.ID, "codex:one", 0, 90); err != nil {
				t.Fatal("live task image evicted", err)
			}
			if _, ok := s.importBytes("codex:two", "image/png", budgetImage("first")); !ok {
				t.Fatal("dedup required extra capacity")
			}
			path, _ := s.find(ref.ID)
			past := time.Now().Add(-2 * attachmentTTL)
			if err := os.Chtimes(path, past, past); err != nil {
				t.Fatal(err)
			}
			if _, ok := s.find(ref.ID); ok {
				t.Fatal("expired cache readable before cleanup")
			}
			if _, err := s.read(ref.ID, "codex:one", 0, 90); err == nil {
				t.Fatal("expired session image readable")
			}
			if _, ok := s.importBytes("codex:two", "image/png", budgetImage("other")); !ok {
				t.Fatal("expired budget not reclaimed")
			}
			if s.cacheFiles != 1 || s.bindings[ref.ID] != nil {
				t.Fatal("expired binding/accounting survived")
			}
		})
	}
}

func TestAttachmentRestartAndParallelWritersShareBudget(t *testing.T) {
	s := testAttachmentStore(t)
	s.limits.cacheFiles = 4
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); s.importBytes("codex:fixture", "image/png", budgetImage(fmt.Sprint(i))) }(i)
	}
	wg.Wait()
	if s.cacheFiles != 4 {
		t.Fatalf("parallel writers exceeded budget: %d", s.cacheFiles)
	}
	restarted := testAttachmentStore(t)
	restarted.dir, restarted.limits = s.dir, s.limits
	if _, ok := restarted.importBytes("codex:fixture", "image/png", budgetImage("new")); ok {
		t.Fatal("restart reset on-disk quota")
	}
	if restarted.cacheFiles != 4 {
		t.Fatal("restart inventory incomplete")
	}
}

func TestAttachmentChunkPathDoesNotSweepDiskEachTime(t *testing.T) {
	s := testAttachmentStore(t)
	if _, err := s.put(budgetAttachmentID(1), "image/png", 0, 3, "AAAA"); err != nil {
		t.Fatal(err)
	}
	firstSweep := s.lastSweep
	if _, err := s.put(budgetAttachmentID(1), "image/png", 1, 3, "AAAA"); err != nil {
		t.Fatal(err)
	}
	if s.lastSweep != firstSweep {
		t.Fatal("every chunk rescanned disk")
	}
	if _, err := os.Stat(s.bindingsFile); !os.IsNotExist(err) {
		t.Fatal("partial upload rewrote binding index")
	}
}

func TestAttachmentExactEightMiBAndBoundedFileReads(t *testing.T) {
	s := testAttachmentStore(t)
	raw := append(budgetImage(""), []byte(strings.Repeat("x", maxAttachmentBytes-8))...)
	encoded := base64.StdEncoding.EncodeToString(raw)
	count := (len(encoded) + maxAttachmentChunk - 1) / maxAttachmentChunk
	for i := 0; i < count; i++ {
		end := min(len(encoded), (i+1)*maxAttachmentChunk)
		done, err := s.put(budgetAttachmentID(1), "image/png", i, count, encoded[i*maxAttachmentChunk:end])
		if err != nil || done != (i == count-1) {
			t.Fatalf("chunk %d: %v %v", i, done, err)
		}
	}
	if s.pendingBytes != 0 || len(s.pending) != 0 || s.cacheBytes != maxAttachmentBytes {
		t.Fatal("completion leaked budget")
	}
	path, _ := s.find(budgetAttachmentID(1))
	if b, err := readAttachmentFile(path, 32); err == nil || len(b) > 33 {
		t.Fatal("file read was not bounded")
	}
}

func TestAttachmentCleanupLoopStopsWithClient(t *testing.T) {
	s := testAttachmentStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.cleanupLoop(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup goroutine leaked")
	}
}
