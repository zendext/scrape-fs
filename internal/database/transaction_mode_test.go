package database

import (
	"net/url"
	"testing"
)

func TestSQLiteDSNUsesImmediateWriteTransactions(t *testing.T) {
	dsn, err := url.Parse(sqliteDSN("/tmp/scrapefs.db"))
	if err != nil {
		t.Fatalf("parse SQLite DSN: %v", err)
	}
	if got := dsn.Query().Get("_txlock"); got != "immediate" {
		t.Errorf("_txlock = %q, want immediate", got)
	}
}
