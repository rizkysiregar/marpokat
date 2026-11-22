// Runs @marpokat/core (the built package) against a live Marpokat with two users, like two browsers would.
// MP_URL=http://localhost:8080 MP_KEY=mpk_... MP_TOKEN_ALICE=... MP_TOKEN_BOB=... MP_CHANNEL=<dm id> node e2e/live.mjs
import { MarpokatClient } from '../dist/index.js';

const env = (k) => { if (!process.env[k]) throw new Error(`set ${k}`); return process.env[k]; };
let failed = 0;
const check = (ok, what) => { console.log((ok ? 'PASS ' : 'FAIL ') + what); if (!ok) failed++; };
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const until = async (what, cond, ms = 15000) => {
  const t0 = Date.now();
  while (Date.now() - t0 < ms) { if (await cond()) return true; await sleep(25); }
  check(false, `${what} (timed out)`); return false;
};
const make = (token) => new MarpokatClient({ baseUrl: env('MP_URL'), apiKey: env('MP_KEY'), tokenProvider: async () => token, backoffBase: 40 });

// a valid 1x1 PNG
const PNG = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==', 'base64');

const alice = make(env('MP_TOKEN_ALICE')), bob = make(env('MP_TOKEN_BOB'));
const ch = env('MP_CHANNEL');
await alice.connect(); await bob.connect();
await until('connected', () => alice.getSnapshot().status === 'connected' && bob.getSnapshot().status === 'connected');
await alice.openChannel(ch); await bob.openChannel(ch);
const view = (c) => c.getSnapshot().messages[ch] ?? [];
const find = (c, text) => view(c).find((m) => m.text === text);

const id = alice.send(ch, { text: 'hello' });
await until('bob receives', () => find(bob, 'hello'));
await until('ack clears pending', () => !(alice.getSnapshot().pending[ch] ?? []).some((p) => p.clientMsgId === id) && find(alice, 'hello'));
check(view(alice).filter((m) => m.text === 'hello').length === 1, 'a message arrives once and is acknowledged');
const first = find(alice, 'hello');

await bob.react(first.id, '👍');
await until('reaction', () => find(alice, 'hello').reactions.counts['👍'] === 1);
check(find(bob, 'hello').reactions.mine[0] === '👍' && find(alice, 'hello').reactions.mine.length === 0, 'reaction: mine is per person');

await alice.edit(first.id, 'hello (edited)');
await until('edit', () => find(bob, 'hello (edited)'));
check(!!find(bob, 'hello (edited)').edited_at, 'edit reaches the other person');

bob.send(ch, { text: 'reply', replyToId: first.id });
await until('reply', () => find(alice, 'reply')?.reply_to?.id === first.id);
check(true, 'reply carries its preview');

let progress = null;
const att = await alice.upload({ data: new Blob([PNG], { type: 'image/png' }), name: 'dot.png', onProgress: (p) => { progress = p; } });
check(att.status === 'processing' && progress?.loaded === PNG.length, 'upload with progress');
alice.send(ch, { text: 'photo', attachmentIds: [att.id] });
await until('thumbnail ready for bob', () => find(bob, 'photo')?.attachments[0]?.status === 'ready');
check(!!find(bob, 'photo').attachments[0].thumbnail_url, 'attachment becomes ready through message.updated');

await alice.remove(first.id);
await until('tombstone', () => view(bob).find((m) => m.id === first.id)?.deleted_at);
check(true, 'delete leaves a tombstone');

// connection drops under alice; bob keeps going; alice writes offline
const seen = new Set();
alice.subscribe(() => seen.add(alice.getSnapshot().status));
alice.ws.close(4000);
await until('alice goes through reconnecting', () => seen.has('reconnecting'), 3000);
bob.send(ch, { text: 'missed 1' });
await until('bob has it', () => find(bob, 'missed 1'));
await bob.react(find(bob, 'reply').id, '🎉');
const off = alice.send(ch, { text: 'written offline' });
await until('alice back and caught up', () => alice.getSnapshot().status === 'connected' && find(alice, 'missed 1') && find(alice, 'reply').reactions.counts['🎉'] === 1);
await until('offline message delivered', () => find(bob, 'written offline') && !(alice.getSnapshot().pending[ch] ?? []).some((p) => p.clientMsgId === off));
await sleep(300);
check(view(bob).filter((m) => m.text === 'written offline').length === 1 && view(alice).filter((m) => m.text === 'missed 1').length === 1, 'reconnect: caught up once, offline message sent once');
const shape = (c) => view(c).map((m) => [m.seq, m.text, !!m.deleted_at, JSON.stringify(m.reactions.counts)].join('|')).join('\n');
check(shape(alice) === shape(bob), 'both end with the same conversation');

alice.disconnect(); bob.disconnect();
console.log(failed ? `${failed} FAILED` : 'ALL PASSED');
process.exit(failed ? 1 : 0);
