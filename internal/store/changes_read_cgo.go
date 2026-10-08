//go:build cgo

package store

import (
	"errors"

	"github.com/mattn/go-sqlite3"
)

func isChangeReadContention(err error) bool {
	var sqliteErr sqlite3.Error
	return errors.As(err, &sqliteErr) && (sqliteErr.Code == sqlite3.ErrBusy || sqliteErr.Code == sqlite3.ErrLocked)
}
