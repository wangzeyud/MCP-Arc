package audit

import (
	"errors"
	"log"
)

// NewStore builds an audit store from the driver name and DSN.
// Supported drivers: "sqlite" (default), "postgres", and "memory" (an
// in-process store used for demos / CI where a real database is unavailable;
// all data is lost when the process exits).
func NewStore(driver, dsn string) (Store, error) {
	switch driver {
	case "sqlite", "":
		return NewSQLite(dsn)
	case "postgres":
		return NewPostgres(dsn)
	case "memory":
		return NewMemory(), nil
	default:
		log.Printf("warn: unknown audit driver %q, falling back to sqlite", driver)
		return NewSQLite(dsn)
	}
}

// ErrNotFound is returned by Get / GetRule / UpdateRule / DeleteRule when the
// requested row does not exist.
var ErrNotFound = errors.New("audit: record not found")
