# Marpokat JS SDK

| Package | What |
| ------- | ---- |
| `@rizkysiregar/marpokat-core` | Client for browser and Node 22+: WebSocket, reconnect, sync, pending messages, uploads, IndexedDB cache |
| `@rizkysiregar/marpokat-react` | Hooks on top of core: `useChannels`, `useChannel`, `useMessages`, `useSendMessage`, `useTyping`, `useUnreadCount` |

```ts
import { MarpokatClient, indexedDbCache } from '@rizkysiregar/marpokat-core';
import { MarpokatProvider, useChannels } from '@rizkysiregar/marpokat-react';

const client = new MarpokatClient({
  baseUrl: 'https://api-marpokat.example.com',
  apiKey: 'mpk_...',
  // Ask your own backend for a user token; called again when the last one expired.
  tokenProvider: () => fetch('/api/chat/token').then((r) => r.json()).then((j) => j.token),
  cache: indexedDbCache(`my-app:${userId}`), // optional: shows the last state at once
});

<MarpokatProvider client={client}><App /></MarpokatProvider>
```

The client keeps the rules of `spec/events.md` for you: auth in the first frame, ping, `rev_seq` cursor and gap
detection, reconnect with full jitter followed by sync, and idempotent sends (a message written offline goes out once
the connection is back, with the same `client_msg_id`). Types come from `spec/openapi.yaml` (`npm run gen`).

```bash
npm install
npm run typecheck && npm test && npm run build
# against a live server (see examples/web-chat/test.mjs for how to get the tokens)
MP_URL=... MP_KEY=... MP_TOKEN_ALICE=... MP_TOKEN_BOB=... MP_CHANNEL=... node packages/core/e2e/live.mjs
```

Publishing goes to GitHub Packages (`npm.pkg.github.com`, scope `@rizkysiregar`):

```bash
make publish     # refuses uncommitted changes and versions already published, tests, builds, publishes both
```

Raise the versions in `packages/*/package.json` first. The token comes from `GITHUB_PACKAGES_TOKEN` or the GitHub CLI
(`write:packages`); it is written to a temporary npm config only.
