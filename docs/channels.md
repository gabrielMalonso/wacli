# channels

Read when: listing, joining, leaving, inspecting, or sending to WhatsApp Channels.

`wacli channels` manages WhatsApp Channels, which `whatsmeow` calls newsletters. Commands use live WhatsApp APIs and require authentication. Commands that update WhatsApp or the local chat cache require writable mode.

## Commands

```bash
wacli channels list
wacli channels info --jid CHANNEL_JID
wacli channels join --invite LINK_OR_CODE
wacli channels leave --jid CHANNEL_JID
```

## Notes

- Local channel history is available read-only through `chats list/show` and `messages list/search/show --chat CHANNEL_JID`. `channels list/info` retain their live refresh behavior.
- Channel JIDs use the `...@newsletter` server.
- `channels list` fetches subscribed channels live and updates local chat rows with kind `newsletter`.
- `channels info` fetches one joined channel live and updates the local chat row.
- `channels join` accepts a full `https://whatsapp.com/channel/...` link or just the invite code.
- `channels leave` unfollows the channel on WhatsApp.
- `sync --refresh-channels` refreshes subscribed channel names into the local chat cache.
- `send text --to ...@newsletter` can send to channels when the authenticated account has permission.
- `send file --to ...@newsletter` uses WhatsApp's unencrypted newsletter media upload path and requires channel posting permission.
- Quoted file replies and `--ptt` voice-note mode are not supported for channels.

## Examples

```bash
wacli channels list
wacli channels info --jid 123456789012345@newsletter
wacli channels join --invite https://whatsapp.com/channel/AbCdEfGhIjK
wacli channels leave --jid 123456789012345@newsletter
wacli send text --to 123456789012345@newsletter --message "Hello channel"
wacli send file --to 123456789012345@newsletter --file ./image.png --caption "Update"
```
