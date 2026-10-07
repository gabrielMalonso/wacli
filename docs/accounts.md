# accounts

Read when: using more than one WhatsApp account, choosing the active account, or migrating from manual `--store` directories.

`wacli accounts` manages named accounts. Each account is an isolated store directory with its own WhatsApp linked-device session, local mirror database, media files, and lock.

## Commands

```bash
wacli accounts list
wacli accounts add NAME [--no-auth]
wacli accounts use NAME
wacli accounts show NAME
wacli accounts remove NAME
```

Use a named account with any command:

```bash
wacli --account work chats list
wacli --account personal send text --to 1234567890 --message "hi"
```

## Strict invocation binding

Use `-a NAME` (or `--for-account NAME`) to bind one invocation to an **existing configured account**, with protection against additional conflicting selectors. First inspect the registry without binding; choose the exact name from that output:

```bash
wacli --read-only accounts list --json
wacli_account='work' # Replace with the exact reviewed existing name.
: "${wacli_account:?Select an existing account}"
wacli -a "$wacli_account" --read-only --agent auth status
wacli --for-account "$wacli_account" --read-only --agent messages list --limit 20
wacli -a "$wacli_account" --read-only --json chats list
```

- The name is mandatory and uses the grammar below; whitespace is rejected, not trimmed. Missing/invalid config or an unknown name fails without fallback, creation or connection. Resolving an entry does not certify that its archive exists, has a compatible schema, is paired or is online.
- The binding overrides `WACLI_STORE_DIR` and `default_account`. Name and resolved absolute store path are fixed once per invocation, even if config is subsequently remapped. This does not pin an inode or isolate data from changes by the same user.
- Equal repetitions of the binding or `--account NAME` are allowed. Every parsed different or empty `--account`, a different binding (even another name for the same path), and **any** parsed `--store` including `--store=` are refused. A contradictory intermediate value is refused even if a later value would restore the name. Split, `=`, `-aNAME` and `-a=NAME` forms follow the CLI parser.
- Validation occurs during flag parsing, before command arguments/hooks, stdin or archive effects, including for help/version. With `--agent`, argument/conflict errors exit 2 with `invalid_arguments`; selection/config failures exit 4 with `store_unavailable`, as one JSON line on stderr. Without `--agent`, existing human/legacy error channels and exits apply. Help/version without binding are unchanged.
- Global `accounts` commands reject binding, including registry management: run them separately without `-a`. Flag-like strings consumed as another flag's value, and positional content after `--`, remain literal content.
- Global [`capabilities`](agent.md#static-capability-discovery) and `help capabilities` also reject binding before registry resolution; discovery runs without any account/store selector.

The same registry refusal applies to `help accounts` and its subcommands; run that help without binding. Help/version flags may precede or follow the selector. Hidden Cobra shell-completion requests keep their protocol: exit 0 and a directive such as `:0` can accompany a parsing diagnostic, and do not certify that the requested command was accepted or executed. Inspect diagnostics; `--agent` refuses these hidden completion requests.

Binding selects an account; it does not authorize auth, sync, sending or other mutations. Output modes, supported agent commands, timeout, signals, locks and readonly policy remain unchanged. Readonly still permits requested export/download output, explicit adapters and SQLite bookkeeping; see [store reads](store.md#local-reads-by-default). No wrapper, alias installation or global default change is required.

## Config

The default config path is `<base>/config.yaml`, where `<base>` is the default store root (`~/.wacli` on macOS and existing legacy Linux installs, otherwise `~/.local/state/wacli` on Linux).

```yaml
default_account: personal

accounts:
  personal:
    store: accounts/personal
  work:
    store: accounts/work
```

Relative `store` paths resolve from the config directory. Absolute paths are allowed for custom layouts.

## Selection Rules

With `-a`/`--for-account`, the strict rules above apply. Without it, existing selection remains:

1. `--store DIR` uses that exact store and cannot be combined with `--account`.
2. `--account NAME` resolves `NAME` from `config.yaml`.
3. `WACLI_STORE_DIR` keeps its existing override behavior for scripts and one-off stores.
4. If `default_account` is set, commands use that account.
5. Otherwise existing single-store behavior remains: XDG state dir on Linux, or `~/.wacli` elsewhere.

Account names may contain letters, digits, `.`, `_`, and `-`, and must start with a letter or digit.

## Notes

- `accounts add NAME` creates the isolated store and then runs the normal auth/bootstrap flow for that account. Use `--no-auth` to only write config and create the store.
- Locks are per account store, so `wacli --account personal sync --follow` and `wacli --account work chats list` do not block each other unless they share the same store path.
- Cross-account search or status should be explicit aggregate commands, not accidental shared database queries.
- Use `--store DIR` for one-off migration/debugging against an old manual store.
