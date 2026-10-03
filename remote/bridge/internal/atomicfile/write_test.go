package atomicfile

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteFileCreatesThenReplacesAndCleansTemporaryFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	for _, data := range [][]byte{[]byte("fixture-original"), bytes.Repeat([]byte("fixture-new"), 1024)} {
		if err := WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatal("configuration was not persisted completely")
		}
		entries, err := os.ReadDir(filepath.Dir(path))
		if err != nil || len(entries) != 1 {
			t.Fatal("successful write left temporary files")
		}
	}
}

func TestWriteFileDoesNotReusePredictableTemporaryPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	for _, suffix := range []string{".tmp", ".salcara-tmp"} {
		if err := os.WriteFile(path+suffix, []byte("another-writer"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteFile(path, []byte("fixture-new"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".tmp", ".salcara-tmp"} {
		got, err := os.ReadFile(path + suffix)
		if err != nil || string(got) != "another-writer" {
			t.Fatal("write consumed a predictable temporary file owned by another writer")
		}
	}
}

func TestWriteFileKeepModePreservesExistingPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses directory ACLs instead of Unix file permissions")
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("fixture-original"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileKeepMode(path, []byte("fixture-new"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o640 {
		t.Fatal("existing file permissions were changed")
	}
}

func TestWriteFileFailedReplacementPreservesDestinationAndCleansTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	retained := filepath.Join(path, "retained.json")
	if err := os.WriteFile(retained, []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("fixture-new"), 0o600); err == nil {
		t.Fatal("replacement of an existing directory succeeded")
	}
	got, err := os.ReadFile(retained)
	if err != nil || string(got) != "retained" {
		t.Fatal("failed replacement changed the destination")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("failed replacement left a temporary credential file")
	}
}
