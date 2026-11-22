# Marpokat realtime protocol v1

Contract of the WebSocket connection between SDKs and Marpokat. The REST contract is `spec/openapi.yaml`; the
object shapes used here (`Message`, `Channel`, `User`, `Member`, `Attachment`, `ReactionSummary`) are the schemas
defined there and are not repeated.

- **Status:** draft v1 (M0). Changes before M6 may break; after the first SDK release, only additive changes in v1.
- **Protocol version:** `1`

---

## 1. Connection

```
wss://api-marpokat.rizkysiregar.my.id/v1/ws        (prod)
wss://api-dev-marpokat.rizkysiregar.my.id/v1/ws    (dev)
```

- Text frames only, one JSON object per frame, UTF-8. Maximum frame size 64 KB (larger frames close the
  connection with `1009`).
- Browsers cannot set headers on a WebSocket, so credentials go in the first frame (`auth`), never in the URL.
- The server closes a connection that has not sent `auth` within **10 seconds** (`4001`).
- One user may have many connections (several devices or tabs). Each receives every event.

## 2. Envelope

Every frame, both directions:

```json
{ "type": "message.send", "id": "c-42", "data": { } }
```

| Field  | Type   | Notes                                                                                                   |
| ------ | ------ | ------------------------------------------------------------------------------------------------------- |
| `type` | string | Required. Unknown types from the server must be ignored by clients (forward compatibility)              |
| `id`   | string | Client request id, max 64 chars. Required on client requests that expect `ack`; echoed in `ack`/`error` |
| `data` | object | Payload, shape depends on `type`. Always present, may be `{}`                                           |

Server events also carry `"ts"`: server time (RFC 3339) when the event was emitted.

## 3. Lifecycle

```
client                                   server
  | ---- open wss://.../v1/ws ----------> |
  | ---- auth {token, protocol_version} -> |   (within 10 s)
  | <--- auth.ok {user, connection_id} --- |   or close 4001/4003/4004
  | ---- ping (every 25 s) --------------> |
  | <--- pong ---------------------------- |
  | ---- message.send, typing, read -----> |
  | <--- ack / error / events ------------ |
```

### 3.1 `auth` (client → server)

```json
{ "type": "auth", "id": "a-1", "data": {
    "api_key": "mpk_...",
    "token": "<user JWT signed by the tenant backend>",
    "protocol_version": 1,
    "client": { "sdk": "marpokat-js", "sdk_version": "0.1.0", "platform": "web" }
} }
```

### 3.2 `auth.ok` (server → client)

```json
{ "type": "auth.ok", "id": "a-1", "ts": "2026-11-22T04:00:00Z", "data": {
    "user": { "id": "0193...", "external_id": "u_123", "name": "Rizky", "avatar_url": null, "deleted": false },
    "connection_id": "0193...",
    "server_time": "2026-11-22T04:00:00Z",
    "heartbeat_interval_ms": 25000
} }
```

Authentication failures close the socket with a 4xxx code (section 9) after an `error` frame.

### 3.3 Heartbeat

- The client sends `{"type":"ping","data":{}}` every `heartbeat_interval_ms` (25 s). Cloudflare drops idle
  connections after about 100 s, so this is mandatory.
- The server answers `{"type":"pong","data":{}}`.
- The server closes a connection with no frame for **60 s** (`4008`). The client should treat 2 missing
  `pong`s as a dead connection and reconnect.

### 3.4 Token expiry

When the user token expires, the server sends `error` with code `token_expired` and closes with `4004`. The SDK
asks its `tokenProvider` for a new token and reconnects. Tokens are checked at `auth` only, plus a timer on the
server at `exp`.

## 4. Client → server requests

Every request with an `id` gets exactly one `ack` (success) or `error` (failure) with the same `id`.

### 4.1 `message.send`

```json
{ "type": "message.send", "id": "c-42", "data": {
    "channel_id": "0193...",
    "client_msg_id": "4b1d0c1e-....",
    "text": "halo!",
    "attachment_ids": [],
    "reply_to_id": null
} }
```

Ack carries the stored `Message`:

```json
{ "type": "ack", "id": "c-42", "ts": "...", "data": { "message": { "id": "0193...", "seq": 57, "rev_seq": 57, "...": "..." } } }
```

- **Idempotent**: resending the same `client_msg_id` in the same channel acks with the already stored message.
  The SDK keeps a pending message and retries with the same `client_msg_id` until it gets `ack` or a
  non-retryable `error` (section 8).
- The sender's other connections receive the message as a normal `message.new`.

### 4.2 `typing.start` / `typing.stop`

```json
{ "type": "typing.start", "data": { "channel_id": "0193..." } }
```

- No `id`, no ack. Ephemeral: never stored.
- The client sends `typing.start` at most every 3 s while the user types and `typing.stop` when they stop or send.
  The server treats typing as expired after 6 s without a refresh.

### 4.3 `read.mark`

```json
{ "type": "read.mark", "id": "c-43", "data": { "channel_id": "0193...", "seq": 57 } }
```

Ack data is `{}`. Moving backwards is ignored. Same effect as `POST /v1/channels/{id}/read`.

### 4.4 `ping`

See section 3.3.

Everything else (edit, delete, reactions, members, channels, attachments) goes through REST; the resulting events
arrive over the socket like any other change.

## 5. Server → client events

All channel events carry `channel_id`. Events that change messages also carry `rev_seq`: the channel counter value
the change took (section 6).

| Type                | Data                                                                                     | When                                                       |
| ------------------- | ---------------------------------------------------------------------------------------- | ---------------------------------------------------------- |
| `message.new`       | `{ channel_id, rev_seq, message: Message }`                                              | A message was posted (including system messages)           |
| `message.updated`   | `{ channel_id, rev_seq, message: Message }`                                              | Edited, or an attachment became `ready`/`failed`           |
| `message.deleted`   | `{ channel_id, rev_seq, message: Message }` (tombstone: `deleted_at` set, no content)    | Deleted                                                    |
| `reaction.added`    | `{ channel_id, rev_seq, message_id, user_id, emoji, reactions: ReactionSummary }`        | A reaction was added                                       |
| `reaction.removed`  | `{ channel_id, rev_seq, message_id, user_id, emoji, reactions: ReactionSummary }`        | A reaction was removed                                     |
| `read.updated`      | `{ channel_id, user_id, last_read_seq }`                                                 | A member read up to a seq (for read receipts)              |
| `typing`            | `{ channel_id, user_id, typing: bool }`                                                  | A member started/stopped typing (never sent to the typist) |
| `presence`          | `{ user_id, online: bool, last_seen_at }`                                                | A user who shares a channel with you went online/offline   |
| `channel.added`     | `{ channel: Channel }`                                                                   | You joined or were added to a channel                      |
| `channel.updated`   | `{ channel: Channel }`                                                                   | Name, avatar or frozen state changed                       |
| `channel.removed`   | `{ channel_id, reason: "left" \| "removed" \| "deleted" }`                               | You are no longer a member                                 |
| `member.added`      | `{ channel_id, member: Member }`                                                         | Someone else joined                                        |
| `member.removed`    | `{ channel_id, user_id }`                                                                | Someone else left or was removed                           |
| `ack`               | `{ ... }` depending on the request                                                       | Reply to a client request                                  |
| `error`             | `{ code, message, details? }`                                                            | Reply to a failed request, or before a close               |
| `pong`              | `{}`                                                                                     | Reply to `ping`                                            |

`user_id` in events is the **external id**, the same id the tenant uses.

In `ReactionSummary` inside events, `mine` is computed for the receiving user.

## 6. Ordering, gaps and sync

Each channel has one counter. Every change takes the next value:

- a new message gets `seq = rev_seq = n`;
- an edit, delete or reaction sets the message's `rev_seq = n` (its `seq` stays).

Per channel the client stores a **cursor** = the highest `rev_seq` it has applied.

- An event with `rev_seq == cursor + 1`: apply it, cursor = `rev_seq`.
- An event with `rev_seq <= cursor`: already applied (duplicate after reconnect), ignore.
- An event with `rev_seq > cursor + 1`: a gap. Apply it, then call
  `GET /v1/channels/{id}/sync?since_rev=<old cursor>` and apply the results. Set the cursor to `next_rev` when
  `has_more` is false.

Realtime delivery is best-effort (Redis Pub/Sub); **sync is the source of truth**. Events from different channels
have no relative order.

Events without `rev_seq` (`read.updated`, `typing`, `presence`, `channel.*`, `member.*`) are state snapshots:
apply the latest. `channel.*` and `member.*` are also covered by `GET /v1/channels?updated_since=`.

## 7. Reconnect

On close (any code except `4003`, `4005`) or a dead heartbeat:

1. Wait `random(0, min(30 s, 500 ms * 2^attempt))` (full jitter); reset `attempt` after a connection has lasted 60 s.
2. Close code `1012` (server restart, e.g. a deploy) skips the first wait: reconnect at once.
3. Connect and `auth` (get a fresh token first if the last close was `4004`).
4. `GET /v1/channels?updated_since=<last successful channel sync>` to learn about channels added, changed or left.
5. For each channel the user is looking at, or that has a cursor: sync from its cursor (section 6). Others can
   sync lazily when opened; their unread count comes from step 4.
6. Resend pending `message.send` requests (same `client_msg_id`).

While disconnected the SDK keeps sends in a local queue, in order.

## 8. Errors

`error` frames: `{ "type": "error", "id": "<request id or absent>", "data": { "code", "message", "details" } }`.
The codes are shared with REST (`Error.code` in openapi.yaml).

| Code                  | Retryable | Meaning                                                                  |
| --------------------- | --------- | ------------------------------------------------------------------------ |
| `bad_request`         | no        | Malformed frame, unknown request type, missing field                     |
| `validation_failed`   | no        | A field breaks a rule (`details.fields`)                                 |
| `unauthenticated`     | no        | Bad api key or token (followed by close `4003`)                          |
| `token_expired`       | yes       | Get a new token and reconnect (followed by close `4004`)                 |
| `forbidden`           | no        | Not allowed (e.g. not a member, tenant config forbids it)                |
| `channel_frozen`      | no        | Channel is read-only                                                     |
| `not_found`           | no        | Channel, message or attachment does not exist or is not visible to you   |
| `conflict`            | no        | State conflict (e.g. attachment not ready)                               |
| `payload_too_large`   | no        | Over a size limit                                                        |
| `rate_limited`        | yes       | Slow down; `details.retry_after_ms`                                      |
| `internal_error`      | yes       | Server bug or dependency failure; retry with backoff                     |
| `service_unavailable` | yes       | Temporarily unavailable                                                  |

## 9. Close codes

| Code   | Meaning                          | Client action                         |
| ------ | -------------------------------- | ------------------------------------- |
| `1000` | Normal close                     | Reconnect only if still needed        |
| `1009` | Frame too large                  | Bug in the client; reconnect          |
| `1011` | Server error                     | Reconnect with backoff                |
| `1012` | Server restart (deploy/shutdown) | Reconnect immediately                 |
| `4000` | Protocol error (bad frame)       | Reconnect with backoff, report bug    |
| `4001` | No `auth` within 10 s            | Reconnect                             |
| `4003` | Authentication failed            | Do not reconnect until new credentials |
| `4004` | Token expired                    | Refresh token, reconnect              |
| `4005` | Unsupported `protocol_version`   | Do not reconnect; upgrade the SDK     |
| `4008` | Heartbeat timeout                | Reconnect                             |
| `4029` | Rate limited (connection flood)  | Reconnect with backoff                |

## 10. Limits and rate limits (v1 defaults, per tenant configurable later)

| What                         | Limit                                     |
| ---------------------------- | ----------------------------------------- |
| Frame size                   | 64 KB                                     |
| Connections per user         | 10                                        |
| `message.send` per user      | 10 per second burst, 60 per minute        |
| `typing.*` per user          | 1 per second per channel                  |
| Requests per connection      | 30 per second                             |
| Text, attachments, members   | as in openapi.yaml, section Limits        |

## 11. Push notification payload (FCM data message)

Sent to a member's devices when they have no open connection and the channel is not muted. Data-only, so the app
decides how to display it:

```json
{
  "marpokat": "1",
  "type": "message.new",
  "channel_id": "0193...",
  "channel_type": "dm",
  "channel_name": "",
  "message_id": "0193...",
  "seq": "57",
  "sender_id": "u_456",
  "sender_name": "Budi",
  "sender_avatar_url": "https://...",
  "text": "halo! (first 200 characters, empty for media)",
  "attachment_kind": "image",
  "unread_count": "3"
}
```

FCM data values are strings. The `marpokat` key lets the app tell Marpokat pushes from its own.

## 12. Webhook payload (M8)

`POST` to the tenant's webhook URL, header `X-Marpokat-Signature: sha256=<hex HMAC of the raw body with the
webhook secret>`, `X-Marpokat-Event-Id` for deduplication (delivery is at-least-once):

```json
{ "event_id": "0193...", "type": "message.new", "created_at": "...", "data": { "channel_id": "...", "message": { } } }
```

Any 2xx is success; anything else is retried (10 s, 1 min, 10 min) and then dead-lettered.
