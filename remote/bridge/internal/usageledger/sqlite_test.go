package usageledger

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLedgerPersistsSQLiteRatherThanJSON(t *testing.T) {
	l := testLedger(t)
	l.Append(observedFixture())
	flushLedger(t, l)
	raw, err := os.ReadFile(l.path)
	if err != nil || !strings.HasPrefix(string(raw), "SQLite format 3\x00") {
		t.Fatal("local request storage is not a durable SQLite database")
	}
}

func TestLedgerMigratesValidatedJSONWithoutDeletingSourceOrChangingIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	state := diskState{Version: 1, Salt: strings.Repeat("ab", 32), Retention: Retention{MaxRecords: 10, Days: 30}, Dropped: 3, Records: []Record{{ID: strings.Repeat("1", 32), Time: time.Now().UnixMilli(), AccountID: "acct-" + strings.Repeat("a", 24), ClientID: "client-" + strings.Repeat("c", 24), RouteID: "route-" + strings.Repeat("b", 24), Model: "gpt-fixture", State: "completed", HTTPStatus: 200}}}
	raw, err := json.Marshal(state)
	if err != nil || os.WriteFile(path, raw, 0o600) != nil {
		t.Fatal("cannot create synthetic legacy fixture")
	}
	l := Open(path)
	defer l.Close(context.Background())
	if l.path == path {
		t.Fatal("legacy JSON must migrate into a separate SQLite file")
	}
	flushLedger(t, l)
	if retained, _ := os.ReadFile(path); string(retained) != string(raw) {
		t.Fatal("migration changed the original recoverable JSON")
	}
	got := ledgerSnapshot(t, l)
	if got.Count != 1 || got.Dropped != 3 || got.Retention != state.Retention || got.Records[0].AccountID != state.Records[0].AccountID || got.Records[0].Tokens.Total != nil {
		t.Fatal("migration changed identities, retention or unknown data", got)
	}
}
