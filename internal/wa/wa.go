// Package wa wraps whatsmeow, backed by pure-Go SQLite (modernc.org/sqlite).
package wa

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
	"modernc.org/sqlite"
)

var registerOnce sync.Once

// OpenContainer opens (creating if needed) the whatsmeow session store at path.
// modernc's driver is registered under "sqlite3", the name whatsmeow expects.
func OpenContainer(ctx context.Context, path string) (*sqlstore.Container, error) {
	registerOnce.Do(func() {
		sql.Register("sqlite3", &sqlite.Driver{})
	})
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", path)
	return sqlstore.New(ctx, "sqlite3", dsn, waLog.Noop)
}
