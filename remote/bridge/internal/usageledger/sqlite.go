package usageledger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
	"salcara/bridge/internal/atomicfile"
)

const maxDatabaseBytes = 32 << 20

type sqliteStorage struct {
	mu   sync.Mutex
	path string
	// legacyPath is a compatibility marker for callers that historically used
	// a .json path. New SQLite stores keep a tiny, secret-free marker at that
	// path so external replacement/removal is visible to save operations. A
	// real legacy JSON source is never touched during migration.
	legacyPath string
	db         *sql.DB
	manifest   map[string]string
	ordinals   map[string]int
	digest     [32]byte
	revision   uint64
}

func databasePath(path string) (string, string) {
	if path == "" {
		return "", ""
	}
	if strings.HasSuffix(strings.ToLower(path), ".json") {
		return filepath.Join(path[:len(path)-5]+".sqlite.d", "ledger.sqlite"), path
	}
	if filepath.Base(path) == "ledger.sqlite" && strings.HasSuffix(strings.ToLower(filepath.Dir(path)), ".sqlite.d") {
		return path, ""
	}
	return filepath.Join(path+".sqlite.d", "ledger.sqlite"), ""
}

func connectDatabase(path string, create bool) (*sql.DB, error) {
	if path == "" {
		return nil, ErrSave
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if create {
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			return nil, err
		}
		// A dedicated directory also protects SQLite's rollback journal and
		// recovery temporary files, which do not inherit the main file's DACL.
		if err := protectStorage(filepath.Dir(abs), true); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
		if err := protectStorage(abs, false); err != nil {
			return nil, err
		}
	}
	mode := "rw"
	u := url.URL{Scheme: "file", Path: "/" + strings.TrimPrefix(filepath.ToSlash(abs), "/")}
	q := u.Query()
	q.Set("mode", mode)
	for _, pragma := range []string{"busy_timeout(2000)", "synchronous(FULL)", "secure_delete(ON)", "journal_mode(DELETE)", "max_page_count(8192)"} {
		q.Add("_pragma", pragma)
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	if create {
		_, err = db.Exec(`PRAGMA auto_vacuum=FULL;
CREATE TABLE IF NOT EXISTS ledger_meta (id INTEGER PRIMARY KEY CHECK(id=1), value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS request_records (
 record_id TEXT PRIMARY KEY, ordinal INTEGER NOT NULL, time INTEGER NOT NULL,
 account_id TEXT NOT NULL, client_id TEXT NOT NULL, route_id TEXT NOT NULL,
 model TEXT NOT NULL, state TEXT NOT NULL, http_status INTEGER NOT NULL,
 duration INTEGER NOT NULL, payload TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS request_time ON request_records(time,record_id);
CREATE INDEX IF NOT EXISTS request_model ON request_records(model,time);
CREATE INDEX IF NOT EXISTS request_account ON request_records(account_id,time);
CREATE INDEX IF NOT EXISTS request_client ON request_records(client_id,time);
PRAGMA user_version=1;`)
		if err != nil {
			db.Close()
			return nil, err
		}
	}
	return db, nil
}

func (s *sqliteStorage) load() (diskState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.legacyPath != "" {
		if info, statErr := os.Stat(s.legacyPath); statErr == nil && info.IsDir() {
			return diskState{}, ErrSave
		} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return diskState{}, statErr
		}
	}
	info, err := os.Stat(s.path)
	if err != nil {
		return diskState{}, err
	}
	if info.Size() > maxDatabaseBytes || info.IsDir() {
		return diskState{}, ErrSave
	}
	db, err := connectDatabase(s.path, false)
	if err != nil {
		return diskState{}, err
	}
	var raw string
	if err = db.QueryRow("SELECT value FROM ledger_meta WHERE id=1").Scan(&raw); err != nil {
		db.Close()
		return diskState{}, err
	}
	var state diskState
	if len(raw) > maxFileBytes || decodeStrict([]byte(raw), &state) != nil {
		db.Close()
		return diskState{}, ErrSave
	}
	rows, err := db.Query("SELECT record_id,ordinal,time,account_id,client_id,route_id,model,state,http_status,duration,payload FROM request_records ORDER BY ordinal LIMIT ?", MaxRecords+1)
	if err != nil {
		db.Close()
		return diskState{}, err
	}
	manifest := make(map[string]string)
	ordinals := make(map[string]int)
	state.Records = []Record{}
	for rows.Next() {
		var ordinal int
		var payload string
		var record Record
		var id, account, client, route, model, terminal string
		var stamp, duration int64
		var status int
		if rows.Scan(&id, &ordinal, &stamp, &account, &client, &route, &model, &terminal, &status, &duration, &payload) != nil || len(payload) > 4096 || decodeStrict([]byte(payload), &record) != nil || ordinal != len(state.Records) || id != record.ID || stamp != record.Time || account != record.AccountID || client != record.ClientID || route != record.RouteID || model != record.Model || terminal != record.State || status != record.HTTPStatus || duration != record.DurationMS {
			rows.Close()
			db.Close()
			return diskState{}, ErrSave
		}
		state.Records = append(state.Records, record)
		manifest[record.ID] = payload
		ordinals[record.ID] = ordinal
	}
	err = rows.Err()
	rows.Close()
	encoded, marshalErr := json.Marshal(state)
	if err != nil || marshalErr != nil || decodeState(encoded, &state) != nil {
		db.Close()
		return diskState{}, ErrSave
	}
	s.db, s.manifest, s.ordinals = db, manifest, ordinals
	s.digest, s.revision = sha256.Sum256(encoded), 1
	return state, nil
}

func (s *sqliteStorage) write(state diskState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state.reset {
		return s.resetLocked(state)
	}
	return s.writeLocked(state)
}
func (s *sqliteStorage) writeLocked(state diskState) error {
	if s.legacyPath != "" {
		if info, err := os.Stat(s.legacyPath); err == nil {
			if info.IsDir() {
				return ErrSave
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if s.db == nil {
		// Only create a genuinely absent database. Existing unreadable storage
		// requires an explicit clear/recovery candidate.
		_, err := os.Stat(s.path)
		if err == nil {
			return ErrSave
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		s.db, err = connectDatabase(s.path, true)
		if err != nil {
			return err
		}
		s.manifest = make(map[string]string)
		s.ordinals = make(map[string]int)
	}
	encoded, err := json.Marshal(state)
	if err != nil || len(encoded) > maxFileBytes {
		return ErrSave
	}
	digest := sha256.Sum256(encoded)
	if s.revision > 0 && digest == s.digest {
		return nil
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	meta := state
	meta.Records = []Record{}
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO ledger_meta(id,value) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET value=excluded.value", string(raw)); err != nil {
		return err
	}
	next := make(map[string]string, len(state.Records))
	nextOrdinals := make(map[string]int, len(state.Records))
	for ordinal, record := range state.Records {
		payload, err := json.Marshal(record)
		if err != nil {
			return err
		}
		next[record.ID] = string(payload)
		nextOrdinals[record.ID] = ordinal
		if s.manifest[record.ID] == string(payload) {
			// Ordinals change only when retention removed earlier rows.
			if s.ordinals[record.ID] != ordinal {
				if _, err := tx.Exec("UPDATE request_records SET ordinal=? WHERE record_id=?", ordinal, record.ID); err != nil {
					return err
				}
			}
			continue
		}
		_, err = tx.Exec(`INSERT INTO request_records(record_id,ordinal,time,account_id,client_id,route_id,model,state,http_status,duration,payload)
VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(record_id) DO UPDATE SET ordinal=excluded.ordinal,time=excluded.time,account_id=excluded.account_id,client_id=excluded.client_id,route_id=excluded.route_id,model=excluded.model,state=excluded.state,http_status=excluded.http_status,duration=excluded.duration,payload=excluded.payload`, record.ID, ordinal, record.Time, record.AccountID, record.ClientID, record.RouteID, record.Model, record.State, record.HTTPStatus, record.DurationMS, string(payload))
		if err != nil {
			return err
		}
	}
	for id := range s.manifest {
		if _, kept := next[id]; !kept {
			if _, err := tx.Exec("DELETE FROM request_records WHERE record_id=?", id); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if s.legacyPath != "" {
		if _, err := os.Stat(s.legacyPath); errors.Is(err, os.ErrNotExist) {
			if err := atomicfile.WriteFile(s.legacyPath, []byte(`{"format":"salcara-usage-ledger","storage":"sqlite","version":1}
`), 0o600); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	s.manifest, s.ordinals = next, nextOrdinals
	s.digest, s.revision = digest, s.revision+1
	return nil
}

// Explicit recovery builds a separate, fully committed database first. Atomic
// replacement never destroys unreadable original bytes on a failed write.
func (s *sqliteStorage) resetLocked(state diskState) error {
	if s.legacyPath != "" {
		if info, err := os.Stat(s.legacyPath); err == nil && info.IsDir() {
			return ErrSave
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".ledger-recovery-*.sqlite")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	defer os.Remove(name)
	defer os.Remove(name + "-journal")
	temp := &sqliteStorage{path: name}
	state.reset = false
	if err := temp.writeLocked(state); err != nil {
		temp.close()
		return err
	}
	if err := temp.close(); err != nil {
		return err
	}
	raw, err := os.ReadFile(name)
	if err != nil || len(raw) > maxDatabaseBytes {
		return ErrSave
	}
	if s.db != nil {
		if err := s.db.Close(); err != nil {
			return err
		}
		s.db = nil
	}
	if err := atomicfile.WriteFile(s.path, raw, 0o600); err != nil {
		return err
	}
	db, err := connectDatabase(s.path, false)
	if err != nil {
		return err
	}
	s.db, s.manifest, s.ordinals, s.digest, s.revision = db, temp.manifest, temp.ordinals, temp.digest, s.revision+1
	return nil
}

func (s *sqliteStorage) close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}
