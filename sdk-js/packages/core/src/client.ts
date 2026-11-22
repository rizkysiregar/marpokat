import {
  type Attachment, type Channel, type ChannelState, type ClientOptions, type ConnectionStatus, type Message, MarpokatError,
  type PendingMessage, type Snapshot, type UploadInput, type User,
} from './types';

type Listener = () => void;
type Frame = { type: string; id?: string; data?: any };

const PROTOCOL_VERSION = 1;
const MAX_BACKOFF_MS = 30_000;

/**
 * The Marpokat client: one WebSocket, the REST API, and the state a chat screen needs (channels, messages, unread,
 * typing, presence). It follows spec/events.md: auth in the first frame, ping every heartbeat, a per-channel
 * `rev_seq` cursor with a gap rule, reconnect with full jitter and sync afterwards, and idempotent sends.
 *
 * State is read with {@link getSnapshot} and observed with {@link subscribe}; each change gives a new snapshot object.
 */
export class MarpokatClient {
  private readonly opt: Required<Pick<ClientOptions, 'backoffBase' | 'pageSize'>> & ClientOptions;
  private readonly fetchImpl: typeof fetch;
  private readonly listeners = new Set<Listener>();

  private status: ConnectionStatus = 'idle';
  private statusDetail = '';
  private me: User | null = null;
  private readonly channels = new Map<string, ChannelState>();
  private readonly messages = new Map<string, Map<number, Message>>();
  private readonly pending = new Map<string, PendingMessage>();
  private readonly typing = new Map<string, Map<string, ReturnType<typeof setTimeout>>>();
  private readonly online = new Map<string, boolean>();
  private current: string | null = null;
  private version = 0;
  private snapshot: Snapshot | null = null;

  private ws: WebSocket | null = null;
  private token = '';
  private attempt = 0;
  private stopped = true;
  private pingTimer: ReturnType<typeof setInterval> | undefined;
  private retryTimer: ReturnType<typeof setTimeout> | undefined;
  private readonly syncTimers = new Map<string, ReturnType<typeof setTimeout>>();
  private syncing = new Map<string, Promise<void>>();
  private lastTyping = 0;
  private notify = false;

  constructor(options: ClientOptions) {
    this.opt = { backoffBase: 500, pageSize: 50, ...options, baseUrl: options.baseUrl.replace(/\/+$/, '') };
    this.fetchImpl = options.fetchImpl ?? globalThis.fetch.bind(globalThis);
  }

  // ------------------------------------------------------------------ observing
  subscribe = (listener: Listener): (() => void) => {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  };

  getSnapshot = (): Snapshot => {
    if (this.snapshot && this.snapshot.version === this.version) return this.snapshot;
    const channels = [...this.channels.values()].sort(
      (a, b) => Date.parse(b.last_message_at ?? b.created_at) - Date.parse(a.last_message_at ?? a.created_at),
    );
    const messages: Snapshot['messages'] = {};
    for (const [id, m] of this.messages) messages[id] = [...m.values()].sort((a, b) => a.seq - b.seq);
    const pending: Snapshot['pending'] = {};
    for (const p of this.pending.values()) (pending[p.channelId] ??= []).push(p);
    for (const list of Object.values(pending)) list.sort((a, b) => a.createdAt - b.createdAt);
    const typing: Snapshot['typing'] = {};
    for (const [id, users] of this.typing) if (users.size) typing[id] = [...users.keys()];
    this.snapshot = {
      version: this.version, status: this.status, statusDetail: this.statusDetail, me: this.me, channels, messages, pending,
      typing, online: Object.fromEntries(this.online),
      unreadTotal: channels.reduce((n, c) => n + (c.membership.unread_count ?? 0), 0),
    };
    return this.snapshot;
  };

  private changed(): void {
    this.version++;
    if (this.notify) return;
    this.notify = true;
    queueMicrotask(() => { // one notification for a burst of changes
      this.notify = false;
      for (const l of [...this.listeners]) l();
    });
  }

  private setStatus(status: ConnectionStatus, detail = ''): void {
    this.status = status;
    this.statusDetail = detail;
    this.changed();
  }

  // ------------------------------------------------------------------ connection
  /** Opens the connection. Resolves when the first attempt was started; use `status` to follow it. */
  async connect(): Promise<void> {
    if (!this.stopped) return;
    this.stopped = false;
    await this.hydrateFromCache();
    await this.open();
  }

  disconnect(): void {
    this.stopped = true;
    clearTimeout(this.retryTimer);
    clearInterval(this.pingTimer);
    for (const t of this.syncTimers.values()) clearTimeout(t);
    this.syncTimers.clear();
    if (this.ws) {
      this.ws.onclose = null;
      this.ws.close(1000);
      this.ws = null;
    }
    this.setStatus('closed');
  }

  /** Forgets everything of this user (sign out). */
  async reset(): Promise<void> {
    this.disconnect();
    this.channels.clear(); this.messages.clear(); this.pending.clear(); this.online.clear();
    for (const m of this.typing.values()) for (const t of m.values()) clearTimeout(t);
    this.typing.clear();
    this.me = null; this.current = null; this.token = '';
    await this.opt.cache?.clear();
    this.status = 'idle';
    this.changed();
  }

  private async hydrateFromCache(): Promise<void> {
    if (!this.opt.cache || this.channels.size) return;
    try {
      const cached = await this.opt.cache.load();
      if (!cached) return;
      for (const c of cached.channels) this.channels.set(c.id, { ...c, cursor: 0, loaded: false, hasOlder: true });
      for (const [id, list] of Object.entries(cached.messages)) this.messages.set(id, new Map(list.map((m) => [m.seq, m])));
      this.changed();
    } catch { /* the cache is an optimization */ }
  }

  private async open(): Promise<void> {
    clearTimeout(this.retryTimer);
    try {
      this.token = await this.opt.tokenProvider();
    } catch (e) {
      this.stopped = true;
      this.setStatus('closed', `no token: ${(e as Error).message}`);
      return;
    }
    this.setStatus(this.attempt === 0 ? 'connecting' : 'reconnecting', this.statusDetail);
    const WS = this.opt.WebSocketImpl ?? globalThis.WebSocket;
    const ws = new WS(this.opt.baseUrl.replace(/^http/, 'ws') + '/v1/ws');
    this.ws = ws;
    ws.onopen = () => this.sendFrame('auth', { api_key: this.opt.apiKey, token: this.token, protocol_version: PROTOCOL_VERSION }, 'auth');
    ws.onmessage = (e) => this.onFrame(JSON.parse(String(e.data)) as Frame);
    ws.onclose = (e) => { if (ws === this.ws) this.onClosed(e.code); };
  }

  private sendFrame(type: string, data: unknown, id?: string): boolean {
    if (this.ws?.readyState !== 1) return false;
    this.ws.send(JSON.stringify({ type, data, ...(id ? { id } : {}) }));
    return true;
  }

  private onClosed(code: number): void {
    clearInterval(this.pingTimer);
    this.ws = null;
    if (code === 4003 || code === 4005) { // credentials or protocol not accepted: trying again cannot help
      this.stopped = true;
      this.setStatus('closed', `closed ${code}: credentials or protocol not accepted`);
      return;
    }
    if (this.stopped) return;
    // 4004: the token ended, the provider gives a new one. 1012: the server restarts: come back at once, once.
    const rand = this.opt.random ?? Math.random;
    const delay = code === 4004 || (code === 1012 && this.attempt === 0)
      ? 0 : rand() * Math.min(MAX_BACKOFF_MS, this.opt.backoffBase * 2 ** this.attempt);
    this.attempt++;
    this.setStatus('reconnecting', `closed ${code}, retrying in ${Math.round(delay)} ms`);
    this.retryTimer = setTimeout(() => void this.open(), delay);
  }

  private async onAuthOk(d: { user: User; heartbeat_interval_ms: number }): Promise<void> {
    this.me = d.user;
    this.attempt = 0;
    clearInterval(this.pingTimer);
    this.pingTimer = setInterval(() => this.sendFrame('ping', {}), d.heartbeat_interval_ms);
    this.setStatus('connected', '');
    try {
      await this.loadChannels(); // what changed in the list (spec 7.4)
      if (this.current) await this.sync(this.current); // the open channel from its cursor (7.5)
    } catch (e) {
      this.setStatus('connected', `sync failed: ${(e as Error).message}`);
    }
    for (const p of this.pending.values()) if (p.status === 'sending') this.transmit(p); // 7.6: same client_msg_id
  }

  // ------------------------------------------------------------------ REST
  async api<T = any>(path: string, init: { method?: string; body?: unknown } = {}, retried = false): Promise<T> {
    const res = await this.fetchImpl(this.opt.baseUrl + path, {
      method: init.method ?? 'GET',
      headers: {
        'X-Api-Key': this.opt.apiKey, Authorization: 'Bearer ' + this.token,
        ...(init.body !== undefined ? { 'Content-Type': 'application/json' } : {}),
      },
      body: init.body !== undefined ? JSON.stringify(init.body) : undefined,
    });
    const text = res.status === 204 ? '' : await res.text();
    const json = text ? JSON.parse(text) : null;
    if (res.ok) return json as T;
    if (res.status === 401 && json?.error?.code === 'token_expired' && !retried) {
      this.token = await this.opt.tokenProvider();
      return this.api<T>(path, init, true);
    }
    throw new MarpokatError(json?.error?.message ?? res.statusText, json?.error?.code ?? 'http_' + res.status, res.status, json?.error?.details);
  }

  // ------------------------------------------------------------------ channels, history, sync
  private putChannel(c: Channel): ChannelState {
    const old = this.channels.get(c.id);
    const state: ChannelState = { ...c, cursor: old?.cursor ?? 0, loaded: old?.loaded ?? false, hasOlder: old?.hasOlder ?? true };
    this.channels.set(c.id, state);
    if (!old && c.last_message) this.storeMessage(c.id, c.last_message);
    return state;
  }

  async loadChannels(): Promise<void> {
    let cursor = '';
    const seen = new Set<string>();
    do {
      const r = await this.api<{ channels: Channel[]; next_cursor?: string | null }>(
        '/v1/channels?limit=100' + (cursor ? '&cursor=' + encodeURIComponent(cursor) : ''));
      for (const c of r.channels) { this.putChannel(c); seen.add(c.id); }
      cursor = r.next_cursor ?? '';
    } while (cursor);
    for (const id of [...this.channels.keys()]) if (!seen.has(id)) this.channels.delete(id); // removed while away
    const peers = [...this.channels.values()].flatMap((c) => (c.dm_peer ? [c.dm_peer.external_id] : []));
    if (peers.length) { // presence events only tell about changes: ask who is online now
      const p = await this.api<{ users: { user_id: string; online: boolean }[] }>('/v1/presence?user_ids=' + encodeURIComponent(peers.slice(0, 100).join(',')));
      for (const u of p.users) this.online.set(u.user_id, u.online);
    }
    this.changed();
    void this.persistChannels();
  }

  /** Makes a channel the open one: loads its latest messages, syncs it and marks it read. */
  async openChannel(id: string): Promise<void> {
    this.current = id;
    const ch = this.channels.get(id);
    if (!ch) throw new MarpokatError('unknown channel', 'not_found');
    const r = await this.api<{ messages: Message[]; has_more: boolean }>(`/v1/channels/${id}/messages?limit=${this.opt.pageSize}`);
    for (const m of r.messages) this.storeMessage(id, m);
    if (!ch.loaded) { // the history starts the cursor at the newest revision it holds
      ch.cursor = Math.max(ch.cursor, 0, ...r.messages.map((m) => m.rev_seq));
      ch.loaded = true;
      ch.hasOlder = r.has_more;
    } else {
      await this.sync(id);
    }
    this.markRead(id);
    this.changed();
    void this.persistMessages(id);
  }

  closeChannel(id: string): void {
    if (this.current === id) this.current = null;
  }

  /** Loads the page of messages before the oldest one held. Resolves to whether more remain. */
  async loadOlder(id: string): Promise<boolean> {
    const ch = this.channels.get(id);
    const held = this.messages.get(id);
    if (!ch || !held?.size) return false;
    const oldest = Math.min(...held.keys());
    const r = await this.api<{ messages: Message[]; has_more: boolean }>(`/v1/channels/${id}/messages?limit=${this.opt.pageSize}&before_seq=${oldest}`);
    for (const m of r.messages) this.storeMessage(id, m);
    ch.hasOlder = r.has_more;
    this.changed();
    return r.has_more;
  }

  /** Spec section 6: everything that changed after the cursor, until the server has no more. */
  sync(id: string): Promise<void> {
    const running = this.syncing.get(id);
    if (running) return running;
    const p = (async () => {
      const ch = this.channels.get(id);
      if (!ch) return;
      for (;;) {
        const r = await this.api<{ messages: Message[]; next_rev: number; has_more: boolean }>(`/v1/channels/${id}/sync?since_rev=${ch.cursor}&limit=100`);
        for (const m of r.messages) this.storeMessage(id, m);
        ch.cursor = Math.max(ch.cursor, r.next_rev);
        if (!r.has_more) break;
      }
      const last = this.latest(id);
      if (last) ch.last_message = last;
      this.changed();
      void this.persistMessages(id);
    })().finally(() => this.syncing.delete(id));
    this.syncing.set(id, p);
    return p;
  }

  private scheduleSync(id: string): void {
    clearTimeout(this.syncTimers.get(id));
    this.syncTimers.set(id, setTimeout(() => void this.sync(id).catch(() => undefined), 100));
  }

  /** The newest state of a message wins: events can arrive in another order than they happened. */
  private storeMessage(channelId: string, m: Message): boolean {
    let held = this.messages.get(channelId);
    if (!held) this.messages.set(channelId, (held = new Map()));
    const cur = held.get(m.seq);
    if (cur && cur.rev_seq >= m.rev_seq) return false;
    held.set(m.seq, m);
    return true;
  }

  private latest(channelId: string): Message | undefined {
    const held = this.messages.get(channelId);
    if (!held?.size) return undefined;
    return held.get(Math.max(...held.keys()));
  }

  /** The gap rule: the next revision moves the cursor, a later one means something was missed (apply it and sync),
   *  an older one was applied already. */
  private revision(ch: ChannelState, rev: number): boolean {
    if (rev <= ch.cursor) return false;
    if (rev === ch.cursor + 1) ch.cursor = rev;
    else this.scheduleSync(ch.id);
    return true;
  }

  private receive(m: Message, isNew = false): void {
    const ch = this.channels.get(m.channel_id);
    if (!ch) { void this.loadChannels().catch(() => undefined); return; }
    if (!this.revision(ch, m.rev_seq)) return;
    this.storeMessage(m.channel_id, m);
    const latest = this.latest(m.channel_id);
    if (latest) { ch.last_message = latest; ch.last_message_at = latest.created_at; }
    const mine = m.sender?.external_id === this.me?.external_id;
    if (m.type !== 'system' && !mine && ch.id !== this.current && !m.deleted_at && isNew) {
      ch.membership.unread_count = (ch.membership.unread_count ?? 0) + 1;
    }
    if (ch.id === this.current && !mine && isVisible()) this.markRead(ch.id);
    this.changed();
    void this.persistMessages(ch.id);
  }

  // ------------------------------------------------------------------ frames
  private onFrame(f: Frame): void {
    const d = f.data ?? {};
    switch (f.type) {
      case 'auth.ok': void this.onAuthOk(d); return;
      case 'ack': {
        const m: Message | undefined = d.message;
        if (m) { this.pending.delete(m.client_msg_id ?? ''); this.receive(m); } // our own message: never unread
        return;
      }
      case 'error': {
        const cid = f.id?.startsWith('m:') ? f.id.slice(2) : undefined;
        const p = cid ? this.pending.get(cid) : undefined;
        if (p) { // a message the server refuses will never be accepted: keep it visible, as failed
          this.pending.set(p.clientMsgId, { ...p, status: 'failed', error: { code: d.code, message: d.message } });
          this.changed();
        }
        return;
      }
      case 'message.new': this.receive(d.message, true); return;
      case 'message.updated': case 'message.deleted': this.receive(d.message); return;
      case 'reaction.added': case 'reaction.removed': {
        const ch = this.channels.get(d.channel_id);
        if (!ch || !this.revision(ch, d.rev_seq)) return;
        const held = this.messages.get(d.channel_id);
        const msg = held && [...held.values()].find((m) => m.id === d.message_id);
        if (held && msg && d.rev_seq > msg.rev_seq) held.set(msg.seq, { ...msg, rev_seq: d.rev_seq, reactions: d.reactions });
        this.changed();
        return;
      }
      case 'read.updated':
        if (d.user_id === this.me?.external_id) {
          const ch = this.channels.get(d.channel_id);
          if (ch) { ch.membership.last_read_seq = Math.max(ch.membership.last_read_seq ?? 0, d.last_read_seq); ch.membership.unread_count = this.unreadOf(ch); this.changed(); }
        }
        return;
      case 'typing': {
        let users = this.typing.get(d.channel_id);
        if (!users) this.typing.set(d.channel_id, (users = new Map()));
        clearTimeout(users.get(d.user_id));
        if (d.typing) {
          users.set(d.user_id, setTimeout(() => { users!.delete(d.user_id); this.changed(); }, 6000));
        } else users.delete(d.user_id);
        this.changed();
        return;
      }
      case 'presence': this.online.set(d.user_id, d.online); this.changed(); return;
      case 'channel.added': case 'channel.updated': this.putChannel(d.channel); this.changed(); void this.persistChannels(); return;
      case 'channel.removed':
        this.channels.delete(d.channel_id); this.messages.delete(d.channel_id);
        if (this.current === d.channel_id) this.current = null;
        this.changed();
        return;
      default: // member.* and unknown events: ignored, new event types never break old clients
    }
  }

  private unreadOf(ch: ChannelState): number {
    const read = ch.membership.last_read_seq ?? 0;
    const held = this.messages.get(ch.id);
    if (!held) return Math.min(ch.membership.unread_count ?? 0, Math.max(0, ch.last_message_seq - read));
    return [...held.values()].filter((m) => m.seq > read && m.type !== 'system' && !m.deleted_at && m.sender?.external_id !== this.me?.external_id).length;
  }

  // ------------------------------------------------------------------ what a user does
  /** Queues a message. It is sent now or, when offline, after the next connect, with the same `client_msg_id`. */
  send(channelId: string, input: { text?: string; attachmentIds?: string[]; replyToId?: string | null }): string {
    const p: PendingMessage = {
      clientMsgId: globalThis.crypto.randomUUID(), channelId, text: input.text ?? '', attachmentIds: input.attachmentIds ?? [],
      replyToId: input.replyToId ?? null, status: 'sending', createdAt: Date.now(),
    };
    this.pending.set(p.clientMsgId, p);
    this.transmit(p);
    this.changed();
    return p.clientMsgId;
  }

  /** Sends a failed message again (same id: it cannot be duplicated if the first try did arrive). */
  retry(clientMsgId: string): void {
    const p = this.pending.get(clientMsgId);
    if (!p) return;
    const again: PendingMessage = { ...p, status: 'sending', error: undefined };
    this.pending.set(clientMsgId, again);
    this.transmit(again);
    this.changed();
  }

  discard(clientMsgId: string): void {
    if (this.pending.delete(clientMsgId)) this.changed();
  }

  private transmit(p: PendingMessage): void {
    this.sendFrame('message.send', {
      channel_id: p.channelId, client_msg_id: p.clientMsgId, text: p.text, attachment_ids: p.attachmentIds, reply_to_id: p.replyToId,
    }, 'm:' + p.clientMsgId);
  }

  /** The three steps of an upload: ask for a link, send the file to the storage (with progress), confirm. */
  async upload(input: UploadInput): Promise<Attachment> {
    const mime = input.mime ?? input.data.type;
    const c = await this.api<{ attachment: Attachment; upload_url: string; upload_headers: Record<string, string> }>(
      '/v1/attachments', { method: 'POST', body: { mime, size: input.data.size, file_name: input.name } });
    await this.put(c.upload_url, c.upload_headers, input.data, input.onProgress, input.signal);
    return this.api<Attachment>(`/v1/attachments/${c.attachment.id}/complete`, { method: 'POST' });
  }

  private put(url: string, headers: Record<string, string>, data: Blob, onProgress?: (p: { loaded: number; total: number }) => void, signal?: AbortSignal): Promise<void> {
    if (onProgress && typeof XMLHttpRequest !== 'undefined') { // fetch cannot report upload progress
      return new Promise((resolve, reject) => {
        const xhr = new XMLHttpRequest();
        xhr.open('PUT', url);
        for (const [k, v] of Object.entries(headers)) xhr.setRequestHeader(k, v);
        xhr.upload.onprogress = (e) => onProgress({ loaded: e.loaded, total: e.lengthComputable ? e.total : data.size });
        xhr.onload = () => (xhr.status >= 200 && xhr.status < 300 ? resolve() : reject(new MarpokatError(`the storage refused the upload (${xhr.status})`, 'upload_failed', xhr.status)));
        xhr.onerror = () => reject(new MarpokatError('the upload could not reach the storage', 'upload_failed'));
        xhr.onabort = () => reject(new MarpokatError('upload cancelled', 'aborted'));
        signal?.addEventListener('abort', () => xhr.abort());
        xhr.send(data);
      });
    }
    return this.fetchImpl(url, { method: 'PUT', headers, body: data, signal }).then((res) => {
      if (!res.ok) throw new MarpokatError(`the storage refused the upload (${res.status})`, 'upload_failed', res.status);
      onProgress?.({ loaded: data.size, total: data.size });
    });
  }

  async react(messageId: string, emoji: string, on = true): Promise<void> {
    this.receive(await this.api<Message>(`/v1/messages/${messageId}/reactions/${encodeURIComponent(emoji)}`, { method: on ? 'PUT' : 'DELETE' }));
  }

  async edit(messageId: string, text: string): Promise<void> {
    this.receive(await this.api<Message>(`/v1/messages/${messageId}`, { method: 'PATCH', body: { text } }));
  }

  async remove(messageId: string): Promise<void> {
    await this.api(`/v1/messages/${messageId}`, { method: 'DELETE' }); // the tombstone arrives as message.deleted
  }

  /** Marks everything up to the newest message as read. */
  markRead(channelId: string): void {
    const ch = this.channels.get(channelId);
    const top = this.latest(channelId)?.seq ?? 0;
    if (!ch || !top) return;
    ch.membership.unread_count = 0;
    ch.membership.last_read_seq = Math.max(ch.membership.last_read_seq ?? 0, top);
    this.sendFrame('read.mark', { channel_id: channelId, seq: top }, 'r:' + channelId);
    this.changed();
  }

  /** Call on every keystroke: it sends at most every 3 seconds (spec 4.2). */
  typingStart(channelId: string): void {
    if (Date.now() - this.lastTyping < 3000) return;
    this.lastTyping = Date.now();
    this.sendFrame('typing.start', { channel_id: channelId });
  }

  typingStop(channelId: string): void {
    this.lastTyping = 0;
    this.sendFrame('typing.stop', { channel_id: channelId });
  }

  // ------------------------------------------------------------------ cache
  private async persistChannels(): Promise<void> {
    try { await this.opt.cache?.saveChannels([...this.channels.values()].map(({ cursor, loaded, hasOlder, ...c }) => c)); } catch { /* optimization */ }
  }

  private async persistMessages(channelId: string): Promise<void> {
    const held = this.messages.get(channelId);
    if (!held) return;
    try { await this.opt.cache?.saveMessages(channelId, [...held.values()].sort((a, b) => a.seq - b.seq)); } catch { /* optimization */ }
  }
}

function isVisible(): boolean {
  return typeof document === 'undefined' || document.visibilityState === 'visible';
}
