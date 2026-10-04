package main

import (
	"context"
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/types"
)

type contactDisplay struct {
	contact store.Contact
	sources []string
	aliases []string
	primary bool
}

func searchContactsForDisplay(ctx context.Context, a *app.App, query string, limit int) ([]store.Contact, error) {
	if limit <= 0 {
		limit = 50
	}
	page, err := a.ReadContacts(ctx, app.ContactReadOptions{Operation: app.ContactSearch, Query: query, Limit: limit})
	if len(page.Contacts) == 0 {
		return nil, err
	}
	return page.Contacts, err
}

func getContactForDisplay(ctx context.Context, a *app.App, rawJID string) (store.Contact, error) {
	resolver, err := contactReadResolver(a)
	if err != nil {
		return store.Contact{}, err
	}
	contacts, err := contactsForDisplay(ctx, a, resolver)
	if err != nil {
		return store.Contact{}, err
	}
	jid := resolveContactReadJID(ctx, resolver, rawJID)
	for _, display := range contacts {
		match := display.contact.JID == jid
		for _, source := range display.sources {
			match = match || source == jid
		}
		if !match {
			continue
		}
		contact := display.contact
		tags := make(map[string]bool)
		for _, source := range display.sources {
			values, err := a.DB().ListTags(source)
			if err != nil {
				return store.Contact{}, err
			}
			for _, tag := range values {
				tags[tag] = true
			}
		}
		for tag := range tags {
			contact.Tags = append(contact.Tags, tag)
		}
		sort.Strings(contact.Tags)
		return contact, nil
	}
	return store.Contact{}, sql.ErrNoRows
}

// Local identity failures keep their original legacy error text. The agent
// boundary can distinguish unreadable state from a missing search result.
type localIdentityError struct{ cause error }

func (e *localIdentityError) Error() string { return e.cause.Error() }
func (e *localIdentityError) Unwrap() error { return e.cause }

func contactReadResolver(a *app.App) (app.LocalResolver, error) {
	if _, err := os.Stat(filepath.Join(a.StoreDir(), "session.db")); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, &localIdentityError{err}
	}
	resolver, err := a.ReadOnlyResolver()
	if err != nil {
		return nil, &localIdentityError{err}
	}
	return resolver, nil
}

func resolveContactReadJID(ctx context.Context, resolver app.LocalResolver, rawJID string) string {
	rawJID = strings.TrimSpace(rawJID)
	jid, err := types.ParseJID(rawJID)
	if err != nil {
		return rawJID
	}
	if jid.Server == types.HiddenUserServer {
		if resolver != nil {
			pn := resolver.ResolveLIDToPN(ctx, jid)
			if pn.User != "" && pn.Server == types.DefaultUserServer {
				return pn.ToNonAD().String()
			}
		}
	}
	return canonicalCLIJID(jid).String()
}

func contactMetadataJIDs(ctx context.Context, a *app.App, rawJID string) ([]string, error) {
	resolver, err := contactReadResolver(a)
	if err != nil {
		return nil, err
	}
	return contactIdentityJIDs(ctx, resolver, rawJID), nil
}

func contactIdentityJIDs(ctx context.Context, resolver app.LocalResolver, rawJID string) []string {
	canonical := resolveContactReadJID(ctx, resolver, rawJID)
	jids := []string{canonical}
	jid, err := types.ParseJID(canonical)
	if err == nil && resolver != nil && jid.Server == types.DefaultUserServer {
		lid := resolver.ResolvePNToLID(ctx, jid).ToNonAD()
		if lid.User != "" && lid.Server == types.HiddenUserServer {
			jids = append(jids, lid.String())
		}
	}
	return jids
}

func contactsForDisplay(ctx context.Context, a *app.App, resolver app.LocalResolver) ([]contactDisplay, error) {
	// Load before limiting so duplicate rows cannot displace distinct contacts,
	// and a LID-only row is searchable by its resolved phone number.
	contacts, err := a.DB().ListContacts(math.MaxInt)
	if err != nil {
		return nil, err
	}
	aliases, err := a.DB().ListContactAliases()
	if err != nil {
		return nil, err
	}
	result := make([]contactDisplay, 0, len(contacts))
	seen := make(map[string]int, len(contacts))
	for _, contact := range contacts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		source := contact.JID
		jid, err := types.ParseJID(source)
		primary := err == nil && jid.Server == types.DefaultUserServer
		if err == nil && jid.Server == types.HiddenUserServer {
			// A LID's user component is an opaque identifier, never a phone number.
			contact.Phone = ""
			if resolver != nil {
				pn := resolver.ResolveLIDToPN(ctx, jid)
				if pn.User != "" && pn.Server == types.DefaultUserServer {
					contact.JID = pn.ToNonAD().String()
					contact.Phone = pn.User
				}
			}
		} else if primary {
			contact.JID = jid.ToNonAD().String()
			contact.Phone = jid.User
		}
		if idx, ok := seen[contact.JID]; ok {
			display := &result[idx]
			if primary && !display.primary {
				display.contact = store.MergeDisplayContacts(contact, display.contact)
			} else {
				display.contact = store.MergeDisplayContacts(display.contact, contact)
			}
			display.primary = display.primary || primary
			display.sources = append(display.sources, source)
			continue
		}
		seen[contact.JID] = len(result)
		result = append(result, contactDisplay{contact: contact, sources: []string{source}, primary: primary})
	}
	for i := range result {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		display := &result[i]
		// Metadata can precede its contact row. Prefer the PN alias even then.
		sources := contactIdentityJIDs(ctx, resolver, display.contact.JID)
		for _, source := range display.sources {
			if !slices.Contains(sources, source) {
				sources = append(sources, source)
			}
		}
		display.sources = sources
		for _, source := range sources {
			if alias := aliases[source]; alias != "" {
				display.aliases = append(display.aliases, alias)
				if len(display.aliases) == 1 {
					display.contact.Alias = alias
					display.contact.Name = alias
				}
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i].contact, result[j].contact
		nameA, nameB := a.Name, b.Name
		if nameA == "" {
			nameA = a.JID
		}
		if nameB == "" {
			nameB = b.JID
		}
		if nameA != nameB {
			return nameA < nameB
		}
		return a.JID < b.JID
	})
	return result, nil
}
