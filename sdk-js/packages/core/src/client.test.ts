import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { MarpokatClient } from './client';
import type { Channel, Message } from './types';

// ---- a fake server: one WebSocket class and a fetch that answers like Marpokat does

class FakeSocket {
  static all: FakeSocket[] = [];
  readyState = 0;
  sent: any[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((e: { data: string }) => void) | null = null;
  onclose: ((e: { code: number }) => void) | null = null;
  constructor(public url: string) { FakeSocket.all.push(this); queueMicrotask(() => { this.readyState = 1; this.onopen?.(); }); }
  send(s: string) { this.sent.push(JSON.parse(s)); }
  close(code = 1000) { this.readyState = 3; this.onclose?.({ code }); }
  push(type: string, data: unknown, id?: string) { this.onmessage?.({ data: JSON.stringify({ type, data, id }) }); }
  frames(type: string) { return this.sent.filter((f) => f.type === type); }
}

const me = { id: 'u-alice', external_id: 'alice', name: 'Alice', deleted: false };
const bob = { id: 'u-bob', external_id: 'bob', name: 'Bob', deleted: false };
const CH = '00000000-0000-0000-0000-00000000c001';

const msg = (seq: number, over: Partial<Message> = {}): Message => ({
  id: `m${seq}`, channel_id: CH, seq, rev_seq: seq, type: 'text', sender: bob, text: `t${seq}`, attachments: [],
  reactions: { counts: {}, mine: [] }, created_at: new Date(1_700_000_000_000 + seq * 1000).toISOString(), ...over,
} as Message);

const channel = (over: Partial<Channel> = {}): Channel => ({
  id: CH, type: 'dm', member_count: 2, dm_peer: bob, last_seq: 3, last_message_seq: 3, frozen: false,
  created_at: '2023-01-01T00:00:00Z', updated_at: '2023-01-01T00:00:00Z', membership: { role: 'member', unread_count: 0, last_read_seq: 0 },
  last_message: msg(3), last_message_at: msg(3).created_at, ...over,
} as unknown as Channel);

interface Server { channels: Channel[]; history: Message[]; changes: Message[]; calls: string[]; expireTokenOnce: boolean }

function makeServer(): { server: Server; fetch: typeof fetch } {
  const server: Server = { channels: [channel()], history: [msg(1), msg(2), msg(3)], changes: [], calls: [], expireTokenOnce: false };
  const json = (body: unknown, status = 200) => new Response(status === 204 ? null : JSON.stringify(body), { status });
  const fetchFake = (async (input: string, init: RequestInit = {}) => {
    const url = new URL(input);
    const key = `${init.method ?? 'GET'} ${url.pathname}`;
    server.calls.push(key + url.search);
    const auth = (init.headers as Record<string, string>).Authorization;
    if (server.expireTokenOnce && auth === 'Bearer old') { server.expireTokenOnce = false; return json({ error: { code: 'token_expired', message: 'expired' } }, 401); }
    if (key === 'GET /v1/channels') return json({ channels: server.channels, next_cursor: null });
    if (key === 'GET /v1/presence') return json({ users: [{ user_id: 'bob', online: true }] });
    if (key === `GET /v1/channels/${CH}/messages`) {
      const before = url.searchParams.get('before_seq');
      const list = server.history.filter((m) => !before || m.seq < Number(before)).slice(-2);
      return json({ messages: [...list].reverse(), has_more: list[0] ? server.history[0]!.seq < list[0].seq : false });
    }
    if (key === `GET /v1/channels/${CH}/sync`) {
      const since = Number(url.searchParams.get('since_rev'));
      const list = server.changes.filter((m) => m.rev_seq > since);
      return json({ messages: list, next_rev: list.at(-1)?.rev_seq ?? since, has_more: false });
    }
    if (key === 'POST /v1/attachments') return json({ attachment: { id: 'att1' }, upload_url: 'https://files.test/put', upload_headers: { 'Content-Type': 'image/png' }, expires_at: '' }, 201);
    if (key === 'POST /v1/attachments/att1/complete') return json({ id: 'att1', status: 'processing', kind: 'image' });
    if (input === 'https://files.test/put') return new Response(null, { status: 200 });
    return json({ error: { code: 'not_found', message: key } }, 404);
  }) as unknown as typeof fetch;
  return { server, fetch: fetchFake };
}

const tick = async (n = 5) => { for (let i = 0; i < n; i++) await Promise.resolve(); };
const settle = async () => { await vi.advanceTimersByTimeAsync(0); await tick(); await vi.advanceTimersByTimeAsync(0); };

let env: ReturnType<typeof makeServer>;
let client: MarpokatClient;
let tokens: string[];

async function start(opts: Partial<ConstructorParameters<typeof MarpokatClient>[0]> = {}) {
  client = new MarpokatClient({
    baseUrl: 'https://api.test', apiKey: 'mpk_x', tokenProvider: async () => tokens.shift() ?? 'tok',
    WebSocketImpl: FakeSocket as unknown as typeof WebSocket, fetchImpl: env.fetch, backoffBase: 100, random: () => 1, ...opts,
  });
  await client.connect();
  await settle();
  const ws = FakeSocket.all.at(-1)!;
  ws.push('auth.ok', { user: me, heartbeat_interval_ms: 25000 });
  await settle();
  return ws;
}

beforeEach(() => { vi.useFakeTimers(); FakeSocket.all = []; env = makeServer(); tokens = []; });
afterEach(() => { client?.disconnect(); vi.useRealTimers(); });

describe('connecting', () => {
  it('authenticates in the first frame and loads channels and presence', async () => {
    const ws = await start();
    expect(ws.sent[0]).toEqual({ type: 'auth', id: 'auth', data: { api_key: 'mpk_x', token: 'tok', protocol_version: 1 } });
    const s = client.getSnapshot();
    expect(s.status).toBe('connected');
    expect(s.me?.external_id).toBe('alice');
    expect(s.channels.map((c) => c.id)).toEqual([CH]);
    expect(s.online.bob).toBe(true);
  });

  it('pings at the interval the server asks for', async () => {
    const ws = await start();
    await vi.advanceTimersByTimeAsync(50_000);
    expect(ws.frames('ping')).toHaveLength(2);
  });

  it('reconnects with a new token after 4004 and gives up on 4003', async () => {
    tokens = ['t1', 't2'];
    const ws = await start();
    ws.close(4004);
    await settle();
    expect(FakeSocket.all).toHaveLength(2);
    expect(FakeSocket.all[1]!.sent[0].data.token).toBe('t2');
    FakeSocket.all[1]!.close(4003);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(FakeSocket.all).toHaveLength(2);
    expect(client.getSnapshot().status).toBe('closed');
  });

  it('backs off with jitter and resets after a success', async () => {
    const ws = await start();
    ws.close(1006);
    await vi.advanceTimersByTimeAsync(99);
    expect(FakeSocket.all).toHaveLength(1); // random()=1: the first wait is the full 100 ms
    await vi.advanceTimersByTimeAsync(2);
    expect(FakeSocket.all).toHaveLength(2);
    FakeSocket.all[1]!.close(1006);
    await vi.advanceTimersByTimeAsync(150);
    expect(FakeSocket.all).toHaveLength(2); // second wait is 200 ms
    await vi.advanceTimersByTimeAsync(100);
    expect(FakeSocket.all).toHaveLength(3);
    FakeSocket.all[2]!.push('auth.ok', { user: me, heartbeat_interval_ms: 25000 });
    await settle();
    FakeSocket.all[2]!.close(1006);
    await vi.advanceTimersByTimeAsync(101);
    expect(FakeSocket.all).toHaveLength(4); // back to the first step
  });

  it('asks for a new token and repeats a request that failed with token_expired', async () => {
    tokens = ['old', 'fresh'];
    env.server.expireTokenOnce = true;
    await start();
    const calls = env.server.calls.filter((c) => c === 'GET /v1/channels?limit=100');
    expect(calls).toHaveLength(2);
    expect(client.getSnapshot().channels).toHaveLength(1);
  });
});

describe('messages', () => {
  it('opens a channel: history in order, cursor at the newest revision, marked read', async () => {
    const ws = await start();
    await client.openChannel(CH);
    expect(client.getSnapshot().messages[CH]!.map((m) => m.seq)).toEqual([2, 3]); // the fake serves 2 per page
    expect(ws.frames('read.mark')[0].data).toEqual({ channel_id: CH, seq: 3 });
  });

  it('pages back with before_seq and stops when there is no more', async () => {
    await start();
    await client.openChannel(CH);
    expect(await client.loadOlder(CH)).toBe(false);
    expect(client.getSnapshot().messages[CH]!.map((m) => m.seq)).toEqual([1, 2, 3]);
    expect(env.server.calls).toContain(`GET /v1/channels/${CH}/messages?limit=50&before_seq=2`);
  });

  it('applies the next revision directly and an old one not at all', async () => {
    const ws = await start();
    await client.openChannel(CH);
    ws.push('message.new', { message: msg(4) });
    ws.push('message.new', { message: msg(4, { text: 'stale', rev_seq: 4 }) });
    await settle();
    expect(client.getSnapshot().messages[CH]!.at(-1)!.text).toBe('t4');
    expect(env.server.calls.filter((c) => c.includes('/sync'))).toHaveLength(0);
  });

  it('syncs when a revision is skipped, then has everything once', async () => {
    const ws = await start();
    await client.openChannel(CH);
    env.server.changes = [msg(4), msg(5), msg(6)];
    ws.push('message.new', { message: msg(6) }); // 4 and 5 were missed
    await vi.advanceTimersByTimeAsync(200);
    await settle();
    expect(client.getSnapshot().messages[CH]!.map((m) => m.seq)).toEqual([2, 3, 4, 5, 6]);
    expect(env.server.calls.filter((c) => c.includes('/sync'))).toHaveLength(1);
  });

  it('catches up after a reconnect from the cursor', async () => {
    const ws = await start();
    await client.openChannel(CH);
    ws.close(1006);
    env.server.changes = [msg(4), msg(3, { text: 'edited', rev_seq: 5 })];
    await vi.advanceTimersByTimeAsync(101);
    FakeSocket.all[1]!.push('auth.ok', { user: me, heartbeat_interval_ms: 25000 });
    await settle();
    const list = client.getSnapshot().messages[CH]!;
    expect(list.map((m) => m.text)).toEqual(['t2', 'edited', 't4']);
    expect(env.server.calls.some((c) => c.includes('since_rev=3'))).toBe(true);
  });

  it('counts unread for new messages of other people in channels that are not open', async () => {
    const ws = await start();
    ws.push('message.new', { message: msg(4) });
    ws.push('message.new', { message: msg(5, { sender: me }) });
    await settle();
    expect(client.getSnapshot().unreadTotal).toBe(1);
    await client.openChannel(CH);
    expect(client.getSnapshot().unreadTotal).toBe(0);
  });

  it('applies a reaction event only if it is newer', async () => {
    const ws = await start();
    await client.openChannel(CH);
    ws.push('reaction.added', { channel_id: CH, rev_seq: 4, message_id: 'm3', user_id: 'bob', emoji: '👍', reactions: { counts: { '👍': 1 }, mine: [] } });
    await settle();
    expect(client.getSnapshot().messages[CH]!.at(-1)!.reactions.counts).toEqual({ '👍': 1 });
  });

  it('shows typing until it stops or 6 seconds pass', async () => {
    const ws = await start();
    ws.push('typing', { channel_id: CH, user_id: 'bob', typing: true });
    await settle();
    expect(client.getSnapshot().typing[CH]).toEqual(['bob']);
    await vi.advanceTimersByTimeAsync(6100);
    expect(client.getSnapshot().typing[CH]).toBeUndefined();
  });

  it('removes a channel when the server says so', async () => {
    const ws = await start();
    ws.push('channel.removed', { channel_id: CH });
    await settle();
    expect(client.getSnapshot().channels).toHaveLength(0);
  });
});

describe('sending', () => {
  it('keeps the message pending until the ack and then shows it once', async () => {
    const ws = await start();
    await client.openChannel(CH);
    const id = client.send(CH, { text: 'hi' });
    expect(client.getSnapshot().pending[CH]).toHaveLength(1);
    expect(ws.frames('message.send')[0].data).toMatchObject({ channel_id: CH, client_msg_id: id, text: 'hi', attachment_ids: [], reply_to_id: null });
    ws.push('ack', { message: msg(4, { client_msg_id: id, sender: me, text: 'hi' }) }, 'm:' + id);
    ws.push('message.new', { message: msg(4, { client_msg_id: id, sender: me, text: 'hi' }) });
    await settle();
    expect(client.getSnapshot().pending[CH]).toBeUndefined();
    expect(client.getSnapshot().messages[CH]!.filter((m) => m.text === 'hi')).toHaveLength(1);
  });

  it('sends a message written offline once the connection is back, with the same id', async () => {
    const ws = await start();
    ws.close(1006);
    const id = client.send(CH, { text: 'offline' });
    expect(client.getSnapshot().pending[CH]![0]!.status).toBe('sending');
    await vi.advanceTimersByTimeAsync(101);
    const ws2 = FakeSocket.all[1]!;
    ws2.push('auth.ok', { user: me, heartbeat_interval_ms: 25000 });
    await settle();
    expect(ws2.frames('message.send').map((f) => f.data.client_msg_id)).toEqual([id]);
  });

  it('marks a message the server refuses as failed, and retry sends the same id again', async () => {
    const ws = await start();
    const id = client.send(CH, { text: 'x' });
    ws.push('error', { code: 'forbidden', message: 'channel is frozen' }, 'm:' + id);
    await settle();
    const p = client.getSnapshot().pending[CH]![0]!;
    expect(p.status).toBe('failed');
    expect(p.error?.code).toBe('forbidden');
    client.retry(id);
    expect(ws.frames('message.send')).toHaveLength(2);
    expect(client.getSnapshot().pending[CH]![0]!.status).toBe('sending');
    client.discard(id);
    expect(client.getSnapshot().pending[CH]).toBeUndefined();
  });

  it('does not send typing more than every 3 seconds', async () => {
    const ws = await start();
    client.typingStart(CH); client.typingStart(CH);
    expect(ws.frames('typing.start')).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(3100);
    client.typingStart(CH);
    expect(ws.frames('typing.start')).toHaveLength(2);
  });
});

describe('uploads', () => {
  it('asks for a link, puts the file with the headers it was given, and confirms', async () => {
    await start();
    const seen: unknown[] = [];
    const a = await client.upload({ data: new Blob(['x'.repeat(10)], { type: 'image/png' }), name: 'a.png', onProgress: (p) => seen.push(p) });
    expect(a.id).toBe('att1');
    expect(env.server.calls).toEqual(expect.arrayContaining(['POST /v1/attachments', 'POST /v1/attachments/att1/complete']));
    expect(seen.at(-1)).toEqual({ loaded: 10, total: 10 });
  });
});

describe('observing', () => {
  it('notifies once for a burst and gives a new snapshot only when something changed', async () => {
    const ws = await start();
    const l = vi.fn();
    client.subscribe(l);
    const before = client.getSnapshot();
    expect(client.getSnapshot()).toBe(before);
    ws.push('presence', { user_id: 'bob', online: false });
    ws.push('presence', { user_id: 'bob', online: true });
    await settle();
    expect(l).toHaveBeenCalledTimes(1);
    expect(client.getSnapshot()).not.toBe(before);
  });

  it('shows cached channels and messages before the server answers', async () => {
    const cache = {
      load: async () => ({ channels: [channel()], messages: { [CH]: [msg(1), msg(2), msg(3)] } }),
      saveChannels: vi.fn(async () => {}), saveMessages: vi.fn(async () => {}), clear: vi.fn(async () => {}),
    };
    client = new MarpokatClient({ baseUrl: 'https://api.test', apiKey: 'k', tokenProvider: async () => 't', WebSocketImpl: FakeSocket as unknown as typeof WebSocket, fetchImpl: env.fetch, cache });
    await client.connect();
    expect(client.getSnapshot().channels).toHaveLength(1);
    expect(client.getSnapshot().messages[CH]).toHaveLength(3);
    expect(client.getSnapshot().status).toBe('connecting');
  });
});
