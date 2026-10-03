package hubclient

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
)

func TestPhoneImagesAssembleFromChunksAndResolve(t *testing.T) {
	s := newAttachmentStore()
	s.dir = t.TempDir()
	img := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte(strings.Repeat("x", 1000))...)
	enc := base64.StdEncoding.EncodeToString(img)
	id := "att_" + strings.Repeat("ab", 16)
	if _, err := s.resolve([]string{id}); err == nil {
		t.Fatal("unfinished upload resolved")
	}
	half := len(enc) / 4 * 2
	if done, err := s.put(id, "image/jpeg", 0, 2, enc[:half]); err != nil || done {
		t.Fatalf("first chunk: %v %v", done, err)
	}
	if done, err := s.put(id, "image/jpeg", 1, 2, enc[half:]); err != nil || !done {
		t.Fatalf("last chunk: %v %v", done, err)
	}
	paths, err := s.resolve([]string{id})
	if err != nil || len(paths) != 1 {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(paths[0]); string(b) != string(img) {
		t.Fatal("bytes differ")
	}
	if done, err := s.put(id, "image/jpeg", 1, 2, enc[half:]); err != nil || !done {
		t.Fatal("retried chunk of a finished upload must be accepted")
	}
	if _, err := s.put("att_"+strings.Repeat("cd", 16), "image/png", 0, 1, enc); err == nil {
		t.Fatal("JPEG bytes accepted as PNG")
	}
	if _, err := s.put("../../etc", "image/jpeg", 0, 1, enc); err == nil {
		t.Fatal("path-like id accepted")
	}
}
