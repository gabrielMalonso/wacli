package app

import (
	"container/heap"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/openclaw/wacli/internal/store"
)

// Contact cursors retain both complete textual sort keys, with bounded decoding.
const MaxContactsCursorBytes = 16384

type ContactOperation string

const (
	ContactList   ContactOperation = "contacts list"
	ContactSearch ContactOperation = "contacts search"
)

type ContactReadOptions struct {
	Operation ContactOperation
	Query     string
	Limit     int
	Cursor    string
	Paginate  bool
}

type ContactsPage struct {
	Contacts   []store.Contact
	HasMore    bool
	NextCursor *string
}

// ContactsCursorError never includes stored keys, token contents or SQL.
type ContactsCursorError struct{ Mismatch bool }

func (e *ContactsCursorError) Error() string {
	if e.Mismatch {
		return "Cursor does not match the selected local archive or contact query; restart without --cursor."
	}
	return "Invalid or unsupported contacts cursor; restart without --cursor."
}

type contactKey struct {
	Name string `json:"name"`
	JID  string `json:"jid"`
}

func keyForContact(c store.Contact) contactKey {
	name := c.Name
	if name == "" {
		name = c.JID
	}
	return contactKey{Name: name, JID: c.JID}
}

// Go string ordering is shared by the heap, continuation and final output;
// compact DTO truncation never changes these complete, case-sensitive keys.
func compareContactKeys(a, b contactKey) int {
	if a.Name < b.Name {
		return -1
	}
	if a.Name > b.Name {
		return 1
	}
	if a.JID < b.JID {
		return -1
	}
	if a.JID > b.JID {
		return 1
	}
	return 0
}

type contactCursor struct {
	Version   int              `json:"v"`
	Operation ContactOperation `json:"op"`
	Scope     string           `json:"scope"`
	Key       contactKey       `json:"key"`
}

func ValidateContactsCursor(token string) error { _, err := decodeContactsCursor(token); return err }
func decodeContactsCursor(token string) (*contactCursor, error) {
	invalid := &ContactsCursorError{}
	if len(token) == 0 || len(token) > MaxContactsCursorBytes {
		return nil, invalid
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil {
		return nil, invalid
	}
	var c contactCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, invalid
	}
	canonical, err := json.Marshal(c)
	if err != nil || base64.RawURLEncoding.EncodeToString(canonical) != token || c.Version != 1 || (c.Operation != ContactList && c.Operation != ContactSearch) || c.Key.JID == "" || c.Key.Name == "" {
		return nil, invalid
	}
	scope, err := base64.RawURLEncoding.Strict().DecodeString(c.Scope)
	if err != nil || len(scope) != sha256.Size || base64.RawURLEncoding.EncodeToString(scope) != c.Scope {
		return nil, invalid
	}
	return &c, nil
}

type contactScope struct {
	Operation          ContactOperation
	Store, Query       string
	ViewVersion        int
	Sources            contactIdentitySources
	QueryJID, QueryLID string
}

func (s contactScope) hash() string {
	raw, _ := json.Marshal(s)
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// A max heap retains only the earliest K canonical results, not all identities.
type contactHeap []store.Contact

func (h contactHeap) Len() int { return len(h) }
func (h contactHeap) Less(i, j int) bool {
	return compareContactKeys(keyForContact(h[i]), keyForContact(h[j])) > 0
}
func (h contactHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *contactHeap) Push(v any)   { *h = append(*h, v.(store.Contact)) }
func (h *contactHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	old[len(old)-1] = store.Contact{}
	*h = old[:len(old)-1]
	return last
}
func (h *contactHeap) retain(c store.Contact, capacity int) {
	if h.Len() < capacity {
		heap.Push(h, c)
		return
	}
	if compareContactKeys(keyForContact(c), keyForContact((*h)[0])) < 0 {
		(*h)[0] = c
		heap.Fix(h, 0)
	}
}
func contactsPageFromHeap(h contactHeap, p ContactReadOptions, scope string) (ContactsPage, error) {
	slices.SortFunc(h, func(a, b store.Contact) int { return compareContactKeys(keyForContact(a), keyForContact(b)) })
	page := ContactsPage{Contacts: []store.Contact(h)}
	if !p.Paginate || len(h) <= p.Limit {
		return page, nil
	}
	page.HasMore = true
	page.Contacts = page.Contacts[:p.Limit]
	if key := keyForContact(page.Contacts[p.Limit-1]); !utf8.ValidString(key.Name) || !utf8.ValidString(key.JID) {
		return ContactsPage{}, fmt.Errorf("stored contact key cannot produce a supported continuation")
	}
	raw, err := json.Marshal(contactCursor{Version: 1, Operation: p.Operation, Scope: scope, Key: keyForContact(page.Contacts[p.Limit-1])})
	if err != nil {
		return ContactsPage{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	if err := ValidateContactsCursor(token); err != nil {
		return ContactsPage{}, fmt.Errorf("stored contact key cannot produce a supported continuation")
	}
	page.NextCursor = &token
	return page, nil
}
