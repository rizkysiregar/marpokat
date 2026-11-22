// Runs the reference client (client.js) against a live Marpokat, like two people in two browsers would. It needs the
// API and the worker running (the thumbnail is made by the worker).
//
//   MP_URL=http://localhost:8080 MP_KEY=mpk_... MP_TOKEN_ALICE=... MP_TOKEN_BOB=... MP_CHANNEL=<dm id> node test.mjs
import zlib from 'node:zlib';
import { Chat, sleep } from './client.js';

const env = (k) => { if (!process.env[k]) throw new Error(`set ${k}`); return process.env[k]; };
let failed = 0;
const check = (ok, what) => { console.log((ok ? 'PASS ' : 'FAIL ') + what); if (!ok) { failed++; process.exitCode = 1; } };
const until = async (what, cond, ms = 15000) => {
  const t0 = Date.now();
  while (Date.now() - t0 < ms) { if (await cond()) return true; await sleep(25); }
  check(false, `${what} (timed out)`); return false;
};

// a small valid PNG, made here so that the test needs no files
function png(w, h) {
  const crcTable = [...Array(256)].map((_, n) => { let c = n; for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1; return c >>> 0; });
  const crc = (buf) => { let c = 0xffffffff; for (const b of buf) c = crcTable[(c ^ b) & 255] ^ (c >>> 8); return (c ^ 0xffffffff) >>> 0; };
  const chunk = (type, data) => {
    const len = Buffer.alloc(4); len.writeUInt32BE(data.length);
    const body = Buffer.concat([Buffer.from(type), data]);
    const sum = Buffer.alloc(4); sum.writeUInt32BE(crc(body));
    return Buffer.concat([len, body, sum]);
  };
  const ihdr = Buffer.alloc(13); ihdr.writeUInt32BE(w, 0); ihdr.writeUInt32BE(h, 4); ihdr[8] = 8; ihdr[9] = 2;
  const raw = Buffer.alloc((w * 3 + 1) * h);
  for (let y = 0; y < h; y++) for (let x = 0; x < w; x++) { const o = y * (w * 3 + 1) + 1 + x * 3; raw[o] = x * 4; raw[o + 1] = y * 4; raw[o + 2] = 128; }
  return Buffer.concat([Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]), chunk('IHDR', ihdr), chunk('IDAT', zlib.deflateSync(raw)), chunk('IEND', Buffer.alloc(0))]);
}

const person = (token) => new Chat({ baseUrl: env('MP_URL'), apiKey: env('MP_KEY'), getToken: async () => token, backoffBase: 40 });
const alice = person(env('MP_TOKEN_ALICE')), bob = person(env('MP_TOKEN_BOB'));
const ch = env('MP_CHANNEL');
alice.onError = (e) => console.log('  alice error:', e.code, e.message);
bob.onError = (e) => console.log('  bob error:', e.code, e.message);

await alice.connect(); await bob.connect();
await until('both connected', () => alice.ready && bob.ready);
await alice.loadChannels(); await bob.loadChannels();
await alice.openChannel(ch); await bob.openChannel(ch);
check(alice.channels.get(ch)?.dm_peer && bob.channels.get(ch)?.dm_peer, 'both see the DM with the other as peer');

// text, in realtime, and the ack clears the pending message
const textOf = (c, text) => c.view(ch).find((m) => m.text === text);
const hello = alice.sendMessage(ch, { text: 'hello from alice' });
await until('bob receives', () => textOf(bob, 'hello from alice'));
await until('alice\'s ack', () => !alice.pending.has(hello) && textOf(alice, 'hello from alice'));
check(true, 'a message arrives at the other person and is acknowledged');
const first = textOf(alice, 'hello from alice');

// reaction
await bob.react(first.id, '👍');
await until('alice sees the reaction', () => alice.view(ch).find((m) => m.id === first.id)?.reactions.counts['👍'] === 1);
check(bob.view(ch).find((m) => m.id === first.id).reactions.mine[0] === '👍' && alice.view(ch).find((m) => m.id === first.id).reactions.mine.length === 0,
  'a reaction reaches the other person, and only the reactor has it as theirs');

// edit
await alice.edit(first.id, 'hello from alice (edited)');
await until('bob sees the edit', () => bob.view(ch).find((m) => m.id === first.id)?.text === 'hello from alice (edited)');
check(bob.view(ch).find((m) => m.id === first.id).edited_at, 'an edit reaches the other person');

// reply
bob.sendMessage(ch, { text: 'a reply', replyToId: first.id });
await until('alice gets the reply', () => textOf(alice, 'a reply')?.reply_to?.id === first.id);
check(textOf(alice, 'a reply').reply_to.text === 'hello from alice (edited)', 'a reply quotes the message it answers, as it is now');

// photo: upload in three steps, send it before the thumbnail exists, watch it become ready
const image = png(120, 80);
const att = await alice.upload({ type: 'image/png', size: image.length, name: 'gradient.png', data: image });
check(att.status === 'processing' && att.url === null, 'an upload is processing after it is confirmed');
alice.sendMessage(ch, { text: 'a photo', attachmentIds: [att.id] });
await until('bob gets the photo message', () => textOf(bob, 'a photo'));
await until('the thumbnail is ready for bob', () => textOf(bob, 'a photo').attachments[0].status === 'ready');
const a = textOf(bob, 'a photo').attachments[0];
check(a.width === 120 && a.height === 80 && a.blurhash && a.thumbnail_url && a.url, 'the other person gets size, blurhash and links when the thumbnail is made');
const thumb = new Uint8Array(await (await fetch(a.thumbnail_url)).arrayBuffer());
check(thumb[0] === 0xff && thumb[1] === 0xd8, 'the thumbnail link serves a JPEG');
check(Buffer.compare(Buffer.from(await (await fetch(a.url)).arrayBuffer()), image) === 0, 'the original link serves the file that was uploaded');

// delete
await alice.remove(first.id);
await until('bob sees the tombstone', () => bob.view(ch).find((m) => m.id === first.id)?.deleted_at);
check(bob.view(ch).find((m) => m.id === first.id).text === null, 'a deletion leaves a tombstone without content');

// the network drops under alice, and she misses a lot
const statuses = [];
alice.onStatus = (s) => statuses.push(s);
alice._drop();
await until('alice notices the connection is gone', () => statuses.some((s) => s.startsWith('closed 4000')), 3000);
bob.sendMessage(ch, { text: 'missed 1' });
bob.sendMessage(ch, { text: 'missed 2' });
const second = textOf(bob, 'a reply');
await bob.react(second.id, '🎉');
await bob.edit(second.id, 'a reply (edited while you were away)');
// and alice types while she has no connection: the message waits and goes out once she is back
const offline = alice.sendMessage(ch, { text: 'written offline' });
check(alice.pending.has(offline), 'a message written offline waits');
await until('alice is back', () => alice.ready);
await until('alice caught up and her message went out', () => alice.view(ch).find((m) => m.id === second.id)?.text === 'a reply (edited while you were away)' && !alice.pending.has(offline));
const seen = (c, text) => c.view(ch).filter((m) => m.text === text).length;
check(seen(alice, 'missed 1') === 1 && seen(alice, 'missed 2') === 1, 'after reconnecting she has what she missed, once');
check(alice.view(ch).find((m) => m.id === second.id).reactions.counts['🎉'] === 1, 'including the reaction and the edit of a message she already had');
await until('bob gets her offline message', () => seen(bob, 'written offline') >= 1);
await sleep(300);
check(seen(bob, 'written offline') === 1 && seen(alice, 'written offline') === 1, 'the message written offline arrived exactly once');

// in the end both see the same conversation
const shape = (c) => c.view(ch).map((m) => [m.seq, m.text, !!m.deleted_at, JSON.stringify(m.reactions.counts)].join('|')).join('\n');
check(shape(alice) === shape(bob), 'both end with exactly the same conversation');
if (shape(alice) !== shape(bob)) console.log('alice:\n' + shape(alice) + '\nbob:\n' + shape(bob));

alice.disconnect(); bob.disconnect();
console.log(failed ? `${failed} FAILED` : 'ALL PASSED');
process.exit(failed ? 1 : 0);
