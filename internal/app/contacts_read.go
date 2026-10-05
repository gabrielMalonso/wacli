package app

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/types"
)

// ContactIdentityError preserves the cause locally; the agent boundary redacts it.
type ContactIdentityError struct{ Cause error }

func (e *ContactIdentityError) Error() string { return e.Cause.Error() }
func (e *ContactIdentityError) Unwrap() error { return e.Cause }

type contactIdentitySources struct{ Map, Own bool }

// ReadContacts streams complete canonical groups and retains at most limit+1
// results. SQLite may scan/sort all rows on every page. The read transaction
// lasts only for this call, with no cross-page or cross-file atomicity promise.
func (a *App) ReadContacts(ctx context.Context, p ContactReadOptions) (ContactsPage, error) {
	if p.Operation != ContactList && p.Operation != ContactSearch {
		return ContactsPage{}, fmt.Errorf("unsupported contact operation")
	}
	if p.Operation == ContactSearch && strings.TrimSpace(p.Query) == "" {
		return ContactsPage{}, fmt.Errorf("query is required")
	}
	if p.Operation == ContactList && p.Query != "" {
		return ContactsPage{}, fmt.Errorf("contact list does not accept a query")
	}
	if p.Limit < 1 || (p.Paginate && p.Limit > 200) {
		return ContactsPage{}, fmt.Errorf("invalid contact limit")
	}
	var cursor *contactCursor
	if p.Cursor != "" {
		var err error
		cursor, err = decodeContactsCursor(p.Cursor)
		if err != nil {
			return ContactsPage{}, err
		}
		if !p.Paginate || cursor.Operation != p.Operation {
			return ContactsPage{}, &ContactsCursorError{Mismatch: true}
		}
	}
	conn, owner, err := a.DB().OpenContactReadConn(ctx)
	if err != nil {
		return ContactsPage{}, err
	}
	defer owner.Close()
	defer conn.Close()
	if err := registerContactParsers(conn); err != nil {
		return ContactsPage{}, err
	}
	sessionPath := filepath.Join(a.StoreDir(), "session.db")
	session := false
	if _, err := os.Stat(sessionPath); err == nil {
		if _, err := conn.ExecContext(ctx, "ATTACH DATABASE ? AS identity", readOnlySessionURI(sessionPath)); err != nil {
			return ContactsPage{}, &ContactIdentityError{err}
		}
		session = true
	} else if !os.IsNotExist(err) {
		return ContactsPage{}, &ContactIdentityError{err}
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ContactsPage{}, err
	}
	defer tx.Rollback()
	sources := contactIdentitySources{}
	if session {
		sources, err = inspectContactIdentitySources(ctx, tx)
		if err != nil {
			return ContactsPage{}, &ContactIdentityError{err}
		}
	}
	queryJID, queryLID := "", ""
	if p.Operation == ContactSearch {
		queryJID, queryLID, err = contactQueryIdentity(ctx, tx, sources, p.Query)
		if err != nil {
			return ContactsPage{}, &ContactIdentityError{err}
		}
	}
	scope := (contactScope{Operation: p.Operation, Store: a.StoreDir(), Query: p.Query, ViewVersion: 1, Sources: sources, QueryJID: queryJID, QueryLID: queryLID}).hash()
	if cursor != nil && cursor.Scope != scope {
		return ContactsPage{}, &ContactsCursorError{Mismatch: true}
	}
	query, args := contactRowsQuery(sources, p)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return ContactsPage{}, err
	}
	defer rows.Close()
	capacity := p.Limit
	if p.Paginate {
		capacity++
	}
	result := make(contactHeap, 0, min(capacity, 201))
	var group contactGroup
	needle := strings.ToLower(p.Query)
	finish := func() {
		if !group.found {
			return
		}
		c := group.finish()
		matching := p.Operation == ContactList || group.match || c.JID == queryJID || strings.Contains(strings.ToLower(c.JID), needle) || strings.Contains(strings.ToLower(c.Phone), needle)
		if matching && (cursor == nil || compareContactKeys(keyForContact(c), cursor.Key) > 0) {
			result.retain(c, capacity)
		}
	}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return ContactsPage{}, err
		}
		var c store.Contact
		var preferred, alternate string
		var primary, rawMatch bool
		var updated int64
		if err := rows.Scan(&c.JID, &c.Phone, &c.Alias, &c.SystemName, &c.Name, &updated, &primary, &preferred, &alternate, &rawMatch); err != nil {
			return ContactsPage{}, err
		}
		if updated > 0 {
			c.UpdatedAt = time.Unix(updated, 0).UTC()
		}
		if group.found && group.contact.JID != c.JID {
			finish()
			group = contactGroup{}
		}
		group.add(c, primary, preferred, alternate, rawMatch, needle)
	}
	if err := rows.Err(); err != nil {
		return ContactsPage{}, err
	}
	finish()
	return contactsPageFromHeap(result, p, scope)
}

// The accumulator stores a fixed number of fields regardless of how many raw
// rows/aliases belong to the identity. Matching is OR-ed before discarding rows.
type contactGroup struct {
	contact                           store.Contact
	found, primary, match             bool
	preferred, alternate, sourceAlias string
}

func (g *contactGroup) add(c store.Contact, primary bool, preferred, alternate string, rawMatch bool, needle string) {
	if !g.found {
		g.contact = c
		g.found = true
	} else if primary && !g.primary {
		g.contact = store.MergeDisplayContacts(c, g.contact)
	} else {
		g.contact = store.MergeDisplayContacts(g.contact, c)
	}
	g.primary = g.primary || primary
	if g.sourceAlias == "" {
		g.sourceAlias = c.Alias
	}
	if g.preferred == "" {
		g.preferred = preferred
	}
	if g.alternate == "" {
		g.alternate = alternate
	}
	g.match = g.match || rawMatch || (c.Alias != "" && strings.Contains(strings.ToLower(c.Alias), needle)) || (preferred != "" && strings.Contains(strings.ToLower(preferred), needle)) || (alternate != "" && strings.Contains(strings.ToLower(alternate), needle))
}
func (g *contactGroup) finish() store.Contact {
	c := g.contact
	alias := g.preferred
	if alias == "" {
		alias = g.alternate
	}
	if alias == "" {
		alias = g.sourceAlias
	}
	if alias != "" {
		c.Alias = alias
		c.Name = alias
	}
	return c
}

// Functions are installed on a dedicated connection, never in the global driver
// registry. They only parse JIDs; no database, identity lookup or network access.
func registerContactParsers(conn *sql.Conn) error {
	return conn.Raw(func(driverConn any) error {
		sqlite, ok := driverConn.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("contact reader requires sqlite")
		}
		funcs := []struct {
			name string
			fn   func(string) string
		}{
			{"wacli_contact_kind", contactJIDKind},
			{"wacli_contact_user", contactJIDUser},
			{"wacli_contact_normal", contactJIDNormal},
			{"wacli_contact_trim", strings.TrimSpace},
		}
		for _, f := range funcs {
			if err := sqlite.RegisterFunc(f.name, f.fn, true); err != nil {
				return err
			}
		}
		return nil
	})
}
func contactJIDKind(raw string) string {
	jid, err := types.ParseJID(raw)
	if err != nil {
		return ""
	}
	switch jid.Server {
	case types.DefaultUserServer:
		return "pn"
	case types.HiddenUserServer:
		return "lid"
	}
	return ""
}
func contactJIDUser(raw string) string {
	jid, err := types.ParseJID(raw)
	if err != nil {
		return ""
	}
	return jid.User
}
func contactJIDNormal(raw string) string {
	jid, err := types.ParseJID(raw)
	if err != nil {
		return raw
	}
	if jid.Server == types.DefaultUserServer {
		jid = jid.ToNonAD()
	}
	return jid.String()
}

func inspectContactIdentitySources(ctx context.Context, tx *sql.Tx) (contactIdentitySources, error) {
	return inspectContactIdentitySourcesInSchema(ctx, tx, "identity")
}

// schema is selected internally (main or identity), never supplied by a caller.
func inspectContactIdentitySourcesInSchema(ctx context.Context, tx *sql.Tx, schema string) (contactIdentitySources, error) {
	var result contactIdentitySources
	for _, table := range []string{"whatsmeow_lid_map", "whatsmeow_device"} {
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+schema+".sqlite_schema WHERE type='table' AND name=?)", table).Scan(&exists); err != nil {
			return result, err
		}
		if !exists {
			continue
		}
		// Table names are the two constants above, not caller input. Only schema
		// metadata is inspected; device data reads select jid/lid explicitly.
		rows, err := tx.QueryContext(ctx, "PRAGMA "+schema+".table_info("+table+")")
		if err != nil {
			return result, err
		}
		columns := map[string]bool{}
		for rows.Next() {
			var cid, notnull, pk int
			var name, kind string
			var defaultValue sql.NullString
			if err := rows.Scan(&cid, &name, &kind, &notnull, &defaultValue, &pk); err != nil {
				rows.Close()
				return result, err
			}
			columns[name] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return result, err
		}
		if table == "whatsmeow_lid_map" {
			if !columns["lid"] || !columns["pn"] {
				return result, fmt.Errorf("incompatible public identity mapping")
			}
			result.Map = true
		} else {
			if !columns["jid"] {
				return result, fmt.Errorf("incompatible public device identity")
			}
			// lid was added by whatsmeow's account-LID migration. Old jid-only
			// device tables have no own pair, and are never upgraded during reads.
			result.Own = columns["lid"]
		}
	}
	return result, nil
}

func publicContactIdentityCTE(s contactIdentitySources) string {
	mapSQL := "SELECT NULL AS lid, NULL AS pn WHERE 0"
	if s.Map {
		mapSQL = "SELECT lid, pn FROM identity.whatsmeow_lid_map"
	}
	ownSQL := "SELECT NULL AS lid, NULL AS pn WHERE 0"
	if s.Own {
		// A linked device may legitimately have no LID yet. Normalize every
		// nullable public input before the pure string UDFs, independently of
		// SQL predicate evaluation order; an absent own pair can use public_map.
		ownSQL = `SELECT wacli_contact_user(wacli_contact_trim(COALESCE(lid,''))) AS lid,
 wacli_contact_user(wacli_contact_trim(COALESCE(jid,''))) AS pn
 FROM identity.whatsmeow_device
 WHERE wacli_contact_kind(wacli_contact_trim(COALESCE(jid,'')))='pn'
 AND wacli_contact_kind(wacli_contact_trim(COALESCE(lid,'')))='lid'`
	}
	return `WITH public_map AS NOT MATERIALIZED (` + mapSQL + `), own_pair AS MATERIALIZED (` + ownSQL + `)`
}

// Point identity resolution shares the transaction and public projection with
// the stream. Missing pairs remain unknown; read/SQL failures are not swallowed.
func contactQueryIdentity(ctx context.Context, tx *sql.Tx, s contactIdentitySources, raw string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	jid, err := types.ParseJID(raw)
	if err != nil {
		return raw, "", nil
	}
	if jid.Server == types.HiddenUserServer {
		var pn sql.NullString
		err := tx.QueryRowContext(ctx, publicContactIdentityCTE(s)+` SELECT COALESCE((SELECT NULLIF(pn,'') FROM own_pair WHERE lid=? ORDER BY pn COLLATE BINARY LIMIT 1),(SELECT NULLIF(pn,'') FROM public_map WHERE lid=?))`, jid.User, jid.User).Scan(&pn)
		if err != nil {
			return "", "", err
		}
		if pn.Valid && pn.String != "" {
			jid = types.JID{User: pn.String, Server: types.DefaultUserServer}
		}
	}
	if jid.Server != types.DefaultUserServer {
		return jid.String(), "", nil
	}
	jid = jid.ToNonAD()
	var lid sql.NullString
	err = tx.QueryRowContext(ctx, publicContactIdentityCTE(s)+` SELECT COALESCE((SELECT NULLIF(lid,'') FROM own_pair WHERE pn=? ORDER BY lid COLLATE BINARY LIMIT 1),(SELECT NULLIF(lid,'') FROM public_map WHERE pn=?))`, jid.User, jid.User).Scan(&lid)
	if err != nil {
		return "", "", err
	}
	alternate := ""
	if lid.Valid && lid.String != "" {
		alternate = types.JID{User: lid.String, Server: types.HiddenUserServer}.String()
	}
	return jid.String(), alternate, nil
}

// Keep the canonical projection materialized inside SQLite: flattening it
// repeatedly invokes the JID parser and identity subqueries for every output
// field. This temporary relation/sort can spill; it is not a Go catalog/cache.
func contactRowsQuery(s contactIdentitySources, p ContactReadOptions) (string, []any) {
	match := "0"
	var args []any
	if p.Operation == ContactSearch {
		match = `LOWER(COALESCE(a.alias,'')) LIKE LOWER(?) ESCAPE '\' OR LOWER(COALESCE(c.system_name,'')) LIKE LOWER(?) ESCAPE '\' OR LOWER(COALESCE(c.full_name,'')) LIKE LOWER(?) ESCAPE '\' OR LOWER(COALESCE(c.push_name,'')) LIKE LOWER(?) ESCAPE '\' OR LOWER(COALESCE(c.first_name,'')) LIKE LOWER(?) ESCAPE '\' OR LOWER(COALESCE(c.business_name,'')) LIKE LOWER(?) ESCAPE '\' OR LOWER(COALESCE(c.phone,'')) LIKE LOWER(?) ESCAPE '\' OR LOWER(c.jid) LIKE LOWER(?) ESCAPE '\'`
		needle := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(p.Query) + "%"
		for i := 0; i < 8; i++ {
			args = append(args, needle)
		}
	}
	q := publicContactIdentityCTE(s) + `, raw AS NOT MATERIALIZED (
 SELECT c.jid AS source, COALESCE(c.phone,'') AS stored_phone, COALESCE(a.alias,'') AS alias, COALESCE(c.system_name,'') AS system_name,
 COALESCE(NULLIF(a.alias,''),NULLIF(c.system_name,''),NULLIF(c.full_name,''),NULLIF(c.push_name,''),NULLIF(c.business_name,''),NULLIF(c.first_name,''),'') AS name,
 COALESCE(NULLIF(a.alias,''),NULLIF(c.system_name,''),NULLIF(c.full_name,''),NULLIF(c.push_name,''),NULLIF(c.business_name,''),NULLIF(c.first_name,''),c.jid) AS source_order,
 c.updated_at, wacli_contact_kind(c.jid) AS kind, wacli_contact_user(c.jid) AS user, (` + match + `) AS original_match
 FROM contacts c LEFT JOIN contact_aliases a ON a.jid=c.jid
 ), mapped AS NOT MATERIALIZED (
 SELECT r.*, CASE WHEN kind='lid' THEN COALESCE((SELECT NULLIF(pn,'') FROM own_pair WHERE lid=r.user ORDER BY pn COLLATE BINARY LIMIT 1),NULLIF(m.pn,'')) END AS mapped_user
 FROM raw r LEFT JOIN public_map m ON r.kind='lid' AND m.lid=r.user
 ), canonical AS MATERIALIZED (
 SELECT r.*, CASE WHEN kind='pn' THEN wacli_contact_normal(source) WHEN mapped_user IS NOT NULL THEN mapped_user || '@s.whatsapp.net' ELSE source END AS jid,
 CASE WHEN kind='pn' THEN user WHEN kind='lid' THEN COALESCE(mapped_user,'') ELSE stored_phone END AS phone
 FROM mapped r
 ), metadata AS NOT MATERIALIZED (
 SELECT r.*, wacli_contact_normal(wacli_contact_trim(jid)) AS metadata_jid,
 CASE WHEN wacli_contact_kind(jid)='pn' THEN COALESCE((SELECT NULLIF(lid,'') FROM own_pair WHERE pn=wacli_contact_user(r.jid) ORDER BY lid COLLATE BINARY LIMIT 1),(SELECT NULLIF(lid,'') FROM public_map WHERE pn=wacli_contact_user(r.jid))) END AS alternate_user
 FROM canonical r
 )
 SELECT r.jid,r.phone,r.alias,r.system_name,r.name,r.updated_at,r.kind='pn',COALESCE(preferred.alias,''),COALESCE(alternate.alias,''),r.original_match
 FROM metadata r
 LEFT JOIN contact_aliases preferred ON preferred.jid=r.metadata_jid
 LEFT JOIN contact_aliases alternate ON alternate.jid=r.alternate_user || '@lid'
 ORDER BY r.jid COLLATE BINARY, r.source_order COLLATE BINARY, r.source COLLATE BINARY`
	return q, args
}
