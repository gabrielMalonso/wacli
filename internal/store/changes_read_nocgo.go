//go:build !cgo

package store

// The driver's no-CGO stub cannot open SQLite or report native contention.
func isChangeReadContention(error) bool { return false }
