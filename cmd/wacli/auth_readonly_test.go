package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestAuthStatusAndDoctorNormalReadonlyJournalPermissions(t *testing.T) {
	for _, journal := range []string{"WAL", "DELETE"} {
		for _, readonlyDir := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/readonly-dir=%t", journal, readonlyDir), func(t *testing.T) {
				if readonlyDir && (runtime.GOOS == "windows" || os.Geteuid() == 0) {
					t.Skip("requires Unix directory permissions without root bypass")
				}
				dir := seedLocalReadStore(t)
				for _, name := range []string{"wacli.db", "session.db"} {
					db, err := sql.Open("sqlite3", filepath.Join(dir, name))
					if err != nil {
						t.Fatal(err)
					}
					if _, err := db.Exec("PRAGMA journal_mode=" + journal); err != nil {
						t.Fatal(err)
					}
					if err := db.Close(); err != nil {
						t.Fatal(err)
					}
					assertNoAuthSQLiteSidecars(t, filepath.Join(dir, name))
					if err := os.Chmod(filepath.Join(dir, name), 0o400); err != nil {
						t.Fatal(err)
					}
				}
				if readonlyDir {
					t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
					if err := os.Chmod(dir, 0o500); err != nil {
						t.Fatal(err)
					}
				}
				before := snapshotLocalStore(t, dir)
				wantReadable := journal == "DELETE" || !readonlyDir
				stdout, err := runLocalRead(t, dir, []string{"auth", "status", "--read-only"})
				if wantReadable {
					var result struct {
						Data struct {
							Authed bool   `json:"authenticated"`
							JID    string `json:"linked_jid"`
						} `json:"data"`
					}
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal([]byte(stdout), &result); err != nil || !result.Data.Authed || result.Data.JID != "15550000009@s.whatsapp.net" {
						t.Fatalf("auth result changed: %s, %v", stdout, err)
					}
				} else if err == nil || stdout != "" {
					t.Fatalf("auth must return a bookkeeping permission error: %s, %v", stdout, err)
				}
				stdout, err = runLocalRead(t, dir, []string{"doctor", "--read-only"})
				if err != nil {
					t.Fatal(err)
				}
				var result struct {
					Data doctorReport `json:"data"`
				}
				if err := json.Unmarshal([]byte(stdout), &result); err != nil {
					t.Fatal(err)
				}
				if result.Data.Connected || (result.Data.StoreError == "") != wantReadable || (wantReadable && (!result.Data.Authed || result.Data.LinkedJID != "15550000009@s.whatsapp.net")) {
					t.Fatalf("doctor diagnostic changed: %s", stdout)
				}
				if !reflect.DeepEqual(before, snapshotLocalStore(t, dir)) {
					t.Fatal("auth/doctor changed database/schema/permissions or created DB/session/LOCK")
				}
			})
		}
	}
}
