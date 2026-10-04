# contacts

Read when: finding synced contacts, importing macOS Contacts names, or managing local contact metadata.

`wacli contacts` works with contact metadata stored locally. Aliases and tags are local to `wacli`; they do not edit WhatsApp contacts on the phone.

## Commands

```bash
wacli contacts list [--limit N]
wacli contacts search <query> [--limit N]
wacli contacts show --jid JID
wacli contacts resolve <lid|phone|jid> [...]
wacli contacts check <phone> [phone...]
wacli contacts refresh
wacli contacts import-system [--input FILE] [--dry-run] [--clear]
wacli contacts alias set --jid JID --alias NAME
wacli contacts alias rm --jid JID
wacli contacts tags add --jid JID --tag TAG
wacli contacts tags rm --jid JID --tag TAG
```

## Local pages

`contacts list` browses the same verified PN/LID view as search. Its table/legacy JSON default is 50 results. With `--agent`, both list and search default to 20, accept limits 1–200, and return `meta.page` with `returned`, `has_more` and `next_cursor`. Continue with the same command/store/query and `--cursor TOKEN`; page size and `--detail compact|full` may change. The cursor requires agent mode.

```bash
wacli --store /path/to/archive --agent contacts list --limit 20
wacli --store /path/to/archive --agent contacts search Alice --limit 20
wacli --store /path/to/archive --agent contacts search Alice --cursor TOKEN --detail full
```

Each page merges identities before limiting and preserves matches on either row, including original names hidden by aliases and metadata whose counterpart contact row is absent. Names sort by their complete case-sensitive Go string value, falling back to the complete JID, then by JID. Compact text truncation does not change this order. Raw source rows are folded in original display-name/JID-fallback order with stored JID as an explicit tie-breaker; conflicting metadata on source rows with formerly undefined identical sort keys now has deterministic precedence.

These are live local reads, with one read transaction per call and no transaction/snapshot between pages or universal atomic snapshot across the archive and session files. Changing a name or another contact's PN/LID mapping can move or merge groups and omit/repeat results. Source availability and the public pair applicable to a JID search are part of the cursor scope; changes require a restart. Other mapping changes are not universally detected. Normal session writes do not automatically invalidate a cursor. No filesystem timestamp, whole-map/catalog hash, saved page or persistent generation is used. An unknown mapping stays unknown, and exhausting local pages proves neither complete WhatsApp contacts nor freshness.

The reader streams canonical identity groups and retains only the earliest `limit+1` results in Go; a group accumulates fixed fields and match flags rather than all source IDs/aliases. Retained memory depends on page size and field widths, not catalogue cardinality. Each page still scans the raw contact set: SQLite performs public-map/alias lookups, materializes a temporary canonical projection and sorts it, potentially spilling to temporary files. This bounds neither SQLite work nor total process memory. There are no new indexes or persisted caches. See [implementation measurements](contacts-pagination.md) for reproducible synthetic benchmarks and query-plan evidence, and [agent pagination](agent.md#contact-pagination) for cursor/error details.

## Notes

- `search` matches alias, full name, push name, first name, business name, phone, and JID.
- `search` and `show` combine phone-number and `@lid` contact rows only when the local WhatsApp session has a verified mapping. Matching names alone never merge contacts. The combined result uses the phone-number JID and its actual phone number; an unmapped `@lid` stays separate with an empty `phone` field.
- Search matches metadata and stored IDs on either row, as well as the resolved phone number. `--limit` applies after combining duplicates. A contact stored only as `@lid` is also searchable by its mapped phone number, including partial numbers.
- For combined contacts, local aliases and system names retain their display precedence, with the phone-number row winning conflicts within each field. `show` accepts either stored JID or the resolved phone-number JID and combines tags. Reading contacts never rewrites their stored rows or metadata.
- Alias and tag commands accept either verified identity, including the JID returned by `search` or `show`. Changes apply atomically to both identities so removing metadata cannot reveal an older copy on the other row. These local commands read the session mapping without connecting to WhatsApp or modifying its session database; an unreadable session database fails the command before a metadata write.
- `resolve` maps each LID to its phone number and each phone number to its LID, using the local session's verified mapping. It reads local state only and takes no store lock, so it works while `sync --follow` is running. Every input gets an entry: an identity without a known pair has `resolved: false` (never dropped), and a group, channel or malformed input carries an `error`. JSON entries have `input`, `jid` (phone JID), `phone`, `lid`, `name`, `resolved` and `error` (#420).
- `check` connects with the account session and asks WhatsApp's servers whether each number is registered (accepts +E164, common formatting, or user JIDs). Results are reported per query and not stored locally; use `--json` for scripting. A number the server did not answer for is reported as `no response` (JSON `"responded": false`) — treat it as unknown, not as a confirmed negative.
- `refresh` imports contacts from the whatsmeow session store into `wacli.db`.
- `import-system` imports display names from macOS Contacts by matching phone numbers against already-synced wacli contacts. Run `contacts refresh` first.
- `import-system --input FILE` reads a JSON array or newline-delimited JSON contacts file with `full_name` and `phones` fields instead of opening macOS Contacts.
- Imported system names are local wacli metadata. They do not edit WhatsApp contacts or macOS Contacts.
- Display precedence is local alias, imported system name, then WhatsApp names.
- Use `import-system --dry-run` before writing. Use `import-system --clear` to remove imported system names.
- See [contacts import-system](contacts-import-system.md) for the full import workflow, JSON shape, file format, and verification steps.
- Tags are local grouping metadata for scripts and future workflows.

## Examples

```bash
wacli contacts search Alice
wacli contacts show --jid 1234567890@s.whatsapp.net
wacli contacts resolve 123456789@lid 987654321@lid --json
wacli contacts check +43 664 12345678 --json
wacli contacts refresh
wacli contacts import-system --dry-run
wacli contacts alias set --jid 1234567890@s.whatsapp.net --alias mom
wacli contacts tags add --jid 1234567890@s.whatsapp.net --tag family
```
