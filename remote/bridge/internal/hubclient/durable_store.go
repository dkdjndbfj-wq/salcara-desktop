package hubclient

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// The key stays in the private Bridge profile. This protects stored payloads
// from casual file inspection; it is not end-to-end or OS credential encryption.
type durableStore struct {
	mu   sync.Mutex
	dir  string
	aead cipher.AEAD
}

func openDurableStore(dir string) (*durableStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	keyPath := filepath.Join(dir, "payload.key")
	key, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		// A replacement key cannot decrypt old receipts or queued messages.
		// Preserve the whole store so restoring its original key remains possible.
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			return nil, readErr
		}
		if len(entries) != 0 {
			return nil, errors.New("远程恢复密钥缺失，请从同一份备份恢复完整 remote-store 目录；不要删除执行记录")
		}
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		f, createErr := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(createErr, os.ErrExist) {
			key, err = os.ReadFile(keyPath)
		} else if createErr != nil {
			return nil, createErr
		} else {
			_, err = f.Write(key)
			if err == nil {
				err = f.Sync()
			}
			closeErr := f.Close()
			if err == nil {
				err = closeErr
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("持久存储密钥损坏，已停止远程写入")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &durableStore{dir: dir, aead: aead}, nil
}

func durableHash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (d *durableStore) seal(data []byte, label string) ([]byte, error) {
	nonce := make([]byte, d.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return d.aead.Seal(nonce, nonce, data, []byte(label)), nil
}

func (d *durableStore) unseal(data []byte, label string) ([]byte, error) {
	n := d.aead.NonceSize()
	if len(data) < n {
		return nil, errors.New("持久记录损坏")
	}
	return d.aead.Open(nil, data[:n], data[n:], []byte(label))
}

func durableWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".durable-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_ = f.Chmod(0600)
	_, err = f.Write(data)
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
	return os.Rename(tmp, path)
}

func (d *durableStore) write(name string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	b, err = d.seal(b, name)
	if err != nil {
		return err
	}
	return durableWrite(filepath.Join(d.dir, name), b)
}

func (d *durableStore) read(name string, value any) error {
	b, err := readAttachmentFile(filepath.Join(d.dir, name), 32<<20)
	if err != nil {
		return err
	}
	b, err = d.unseal(b, name)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, value)
}

func (d *durableStore) database() (*sql.DB, error) {
	db, err := sql.Open("sqlite", filepath.Join(d.dir, "operations.sqlite"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA busy_timeout=5000; PRAGMA synchronous=FULL;
	CREATE TABLE IF NOT EXISTS operations (scope TEXT NOT NULL, id TEXT NOT NULL, digest TEXT NOT NULL, source TEXT NOT NULL, status TEXT NOT NULL, payload BLOB, updated INTEGER NOT NULL, PRIMARY KEY(scope,id));`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// claim is committed before any adapter write. A crashed claim is deliberately
// terminal uncertainty: no restart or retry may turn it into another execution.
func (d *durableStore) claim(scope, id, digest, source string) (bool, any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	db, err := d.database()
	if err != nil {
		return false, nil, err
	}
	defer db.Close()
	var oldDigest, oldSource, status string
	var updated int64
	var payload []byte
	err = db.QueryRow("SELECT digest,source,status,payload,updated FROM operations WHERE scope=? AND id=?", scope, id).Scan(&oldDigest, &oldSource, &status, &payload, &updated)
	if err == nil {
		if oldDigest != digest || oldSource != source {
			return true, nil, commandDeliveryUncertainError{}
		}
		if status != "complete" || len(payload) == 0 || updated < time.Now().Add(-7*24*time.Hour).Unix() {
			return true, nil, commandDeliveryUncertainError{}
		}
		plain, e := d.unseal(payload, scope+":"+id)
		if e != nil {
			return true, nil, e
		}
		var result any
		e = json.Unmarshal(plain, &result)
		return true, result, e
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, nil, err
	}
	// Keep compact tombstones indefinitely. Deleting them would allow an old
	// UUID to execute again. At the bound new work fails closed, never evicts them.
	_, err = db.Exec("UPDATE operations SET payload=NULL,status='expired' WHERE payload IS NOT NULL AND updated < ?", time.Now().Add(-7*24*time.Hour).Unix())
	if err != nil {
		return false, nil, err
	}
	var count int
	if err = db.QueryRow("SELECT count(*) FROM operations").Scan(&count); err != nil {
		return false, nil, err
	}
	if count >= 100000 {
		return false, nil, errors.New("执行账本已满，请维护存储后重试")
	}
	_, err = db.Exec("INSERT INTO operations(scope,id,digest,source,status,updated) VALUES(?,?,?,?,'claimed',?)", scope, id, digest, source, time.Now().Unix())
	return false, nil, err
}

// completeKnownClaim repairs only an already-claimed exact operation after a
// separate native-history proof. Neither an absent row nor changed payload or
// source can be inserted, reset or executed through this path.
func (d *durableStore) completeKnownClaim(scope, id, digest, source string, result any) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	db, err := d.database()
	if err != nil {
		return false, err
	}
	defer db.Close()
	var oldDigest, oldSource, status string
	if err = db.QueryRow("SELECT digest,source,status FROM operations WHERE scope=? AND id=?", scope, id).Scan(&oldDigest, &oldSource, &status); err != nil {
		return false, err
	}
	if oldDigest != digest || oldSource != source || status != "claimed" && status != "uncertain" {
		return false, nil
	}
	plain, err := json.Marshal(result)
	if err != nil || len(plain) > 1024 {
		return false, errors.New("恢复回执无效")
	}
	payload, err := d.seal(plain, scope+":"+id)
	if err != nil {
		return false, err
	}
	var retained int64
	if err = db.QueryRow("SELECT COALESCE(sum(length(payload)),0) FROM operations").Scan(&retained); err != nil {
		return false, err
	}
	if retained+int64(len(payload)) > 32<<20 {
		return false, errors.New("执行回执存储已满")
	}
	updated, err := db.Exec("UPDATE operations SET status='complete',payload=?,updated=? WHERE scope=? AND id=? AND digest=? AND source=? AND status IN ('claimed','uncertain')", payload, time.Now().Unix(), scope, id, digest, source)
	if err != nil {
		return false, err
	}
	count, err := updated.RowsAffected()
	return count == 1, err
}

func (d *durableStore) finish(scope, id string, result any, runErr error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	status := "uncertain"
	var payload []byte
	if runErr == nil {
		plain, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if len(plain) > 1<<20 {
			return errors.New("执行回执过大")
		}
		payload, err = d.seal(plain, scope+":"+id)
		if err != nil {
			return err
		}
		status = "complete"
	}
	db, err := d.database()
	if err != nil {
		return err
	}
	defer db.Close()
	var retained int64
	if err = db.QueryRow("SELECT COALESCE(sum(length(payload)),0) FROM operations").Scan(&retained); err != nil {
		return err
	}
	if retained+int64(len(payload)) > 32<<20 {
		return errors.New("执行回执存储已满，执行结果保留为待确认")
	}
	_, err = db.Exec("UPDATE operations SET status=?,payload=?,updated=? WHERE scope=? AND id=?", status, payload, time.Now().Unix(), scope, id)
	return err
}

func (d *durableStore) expireReceipts() {
	d.mu.Lock()
	defer d.mu.Unlock()
	db, err := d.database()
	if err != nil {
		return
	}
	defer db.Close()
	_, _ = db.Exec("UPDATE operations SET payload=NULL,status='expired' WHERE payload IS NOT NULL AND updated < ?", time.Now().Add(-7*24*time.Hour).Unix())
}
