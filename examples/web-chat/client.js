// The protocol logic of a Marpokat client, with no DOM, so that it can run in a browser and in Node. It follows the
// rules of spec/events.md: auth in the first frame, ping every heartbeat_interval_ms, a cursor and gap rule per channel
// (section 6), reconnecting with full jitter and syncing afterwards (section 7), and idempotent sends. It is also the
// shape the SDK will take.

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

export class Chat {
  /**
   * @param {object} o
   * @param {string} o.baseUrl   e.g. https://api-dev-marpokat.rizkysiregar.my.id
   * @param {string} o.apiKey
   * @param {() => Promise<string>} o.getToken  gives a user token; called again when the last one has expired
   */
  constructor(o) {
    Object.assign(this, {
      WebSocketImpl: globalThis.WebSocket,
      fetchImpl: globalThis.fetch?.bind(globalThis),
      onUpdate() {}, onStatus() {}, onError() {}, log() {},
      backoffBase: 500, random: Math.random,
    }, o);
    this.baseUrl = o.baseUrl.replace(/\/+$/, '');
    this.me = null;
    this.channels = new Map();   // id -> channel as sent by the server, plus {messages: Map(seq -> message), cursor, loaded}
    this.online = new Map();     // external id -> bool
    this.typing = new Map();     // "channel:user" -> timer
    this.pending = new Map();    // client_msg_id -> {channelId, text, attachmentIds, replyToId}: sent, not acknowledged
    this.current = null;
    this.ready = false;
    this._ws = null; this._attempt = 0; this._stopped = true; this._ping = null; this._retry = null; this._token = null;
    this._lastTyping = 0; this._syncTimers = new Map();
  }

  // ------------------------------------------------------------ connection
  async connect() { this._stopped = false; await this._open(); }

  disconnect() {
    this._stopped = true; clearTimeout(this._retry); clearInterval(this._ping);
    if (this._ws) { this._ws.onclose = null; this._ws.close(1000); this._ws = null; }
    this.ready = false;
  }

  /** Closes the socket without a goodbye, as a dropped network would: the client must find its way back. */
  _drop() { this._ws?.close(4000); }

  async _open() {
    clearTimeout(this._retry);
    try { this._token = await this.getToken(); } catch (e) { this._stopped = true; this.onStatus(`no token: ${e.message}`); return; }
    this.onStatus('connecting');
    const ws = new this.WebSocketImpl(this.baseUrl.replace(/^http/, 'ws') + '/v1/ws');
    this._ws = ws;
    ws.onopen = () => this._send('auth', { api_key: this.apiKey, token: this._token, protocol_version: 1 }, 'auth');
    ws.onmessage = (e) => { const f = JSON.parse(e.data); this.log('<', f); this._frame(f); };
    ws.onclose = (e) => { if (ws === this._ws) this._closed(e.code); };
  }

  _send(type, data, id) {
    if (this._ws?.readyState !== 1) return false;
    const f = { type, data }; if (id) f.id = id;
    this.log('>', f); this._ws.send(JSON.stringify(f)); return true;
  }

  _closed(code) {
    clearInterval(this._ping); this._ws = null; this.ready = false;
    if (code === 4003 || code === 4005) { this._stopped = true; this.onStatus(`closed ${code}: the credentials or the protocol are not accepted`); return this.onUpdate(); }
    if (this._stopped) return;
    // 4004: the token ended, getToken gives a new one. 1012: the server restarts, so come back at once (section 7)
    const delay = code === 4004 || (code === 1012 && this._attempt === 0)
      ? 0 : this.random() * Math.min(30000, this.backoffBase * 2 ** this._attempt);
    this._attempt++;
    this.onStatus(`closed ${code}, reconnecting in ${Math.round(delay)} ms`);
    this._retry = setTimeout(() => this._open(), delay);
    this.onUpdate();
  }

  async _onAuthOk(d) {
    this.me = d.user; this._attempt = 0; this.ready = true;
    this._ping = setInterval(() => this._send('ping', {}), d.heartbeat_interval_ms);
    this.onStatus(`connected as ${d.user.name || d.user.external_id}`);
    try {
      await this.loadChannels();                                       // 7.4: what changed in the list
      if (this.current) await this.syncChannel(this.current);          // 7.5: the open channel, from its cursor
    } catch (e) { this.onError(e); }
    for (const [id, p] of this.pending) this._transmit(id, p);         // 7.6: the same client_msg_id: no duplicates
    this.onUpdate();
  }

  // ------------------------------------------------------------ REST
  async api(path, { method = 'GET', body } = {}, retried = false) {
    const res = await this.fetchImpl(this.baseUrl + path, {
      method,
      headers: { 'X-Api-Key': this.apiKey, Authorization: 'Bearer ' + this._token, ...(body ? { 'Content-Type': 'application/json' } : {}) },
      body: body ? JSON.stringify(body) : undefined,
    });
    const text = res.status === 204 ? '' : await res.text();
    const json = text ? JSON.parse(text) : null;
    if (res.ok) return json;
    if (res.status === 401 && json?.error?.code === 'token_expired' && !retried) { this._token = await this.getToken(); return this.api(path, { method, body }, true); }
    throw Object.assign(new Error(json?.error?.message || res.statusText), { status: res.status, code: json?.error?.code, details: json?.error?.details });
  }

  // ------------------------------------------------------------ channels, history, sync
  _put(c) {
    const old = this.channels.get(c.id);
    const ch = Object.assign(c, { messages: old?.messages || new Map(), cursor: old?.cursor || 0, loaded: old?.loaded || false });
    if (!old && c.last_message) ch.messages.set(c.last_message.seq, c.last_message);
    this.channels.set(c.id, ch);
    return ch;
  }

  async loadChannels() {
    let cursor = '';
    do {
      const r = await this.api('/v1/channels?limit=100' + (cursor ? '&cursor=' + encodeURIComponent(cursor) : ''));
      for (const c of r.channels) this._put(c);
      cursor = r.next_cursor;
    } while (cursor);
    const peers = [...this.channels.values()].filter((c) => c.dm_peer).map((c) => c.dm_peer.external_id);
    if (peers.length) { // the presence events only tell about changes: ask who is online now
      const p = await this.api('/v1/presence?user_ids=' + encodeURIComponent(peers.slice(0, 100).join(',')));
      for (const u of p.users) this.online.set(u.user_id, u.online);
    }
    this.onUpdate();
  }

  async openChannel(id) {
    this.current = id;
    const ch = this.channels.get(id);
    const r = await this.api(`/v1/channels/${id}/messages?limit=50`);
    for (const m of r.messages) ch.messages.set(m.seq, m);
    if (!ch.loaded) { // never synced: the history starts the cursor at the newest revision it holds
      ch.cursor = Math.max(ch.cursor, ...[...ch.messages.values()].map((m) => m.rev_seq));
      ch.loaded = true;
    }
    ch.membership.unread_count = 0;
    this._read(ch);
    this.onUpdate();
  }

  _read(ch) {
    const top = Math.max(0, ...[...ch.messages.keys()]);
    if (top) this._send('read.mark', { channel_id: ch.id, seq: top }, 'r' + Date.now());
  }

  /** Spec section 6: everything that changed after the cursor, until the server has no more. */
  async syncChannel(id) {
    const ch = this.channels.get(id); if (!ch) return;
    for (;;) {
      const r = await this.api(`/v1/channels/${id}/sync?since_rev=${ch.cursor}&limit=100`);
      for (const m of r.messages) this._store(ch, m);
      ch.cursor = r.next_rev;
      if (!r.has_more) break;
    }
    this.onUpdate();
  }

  _scheduleSync(ch) {
    clearTimeout(this._syncTimers.get(ch.id));
    this._syncTimers.set(ch.id, setTimeout(() => this.syncChannel(ch.id).catch((e) => this.onError(e)), 100));
  }

  /** The newest state of a message wins: events can arrive in another order than they happened. */
  _store(ch, m) {
    const cur = ch.messages.get(m.seq);
    if (cur && cur.rev_seq >= m.rev_seq) return false;
    ch.messages.set(m.seq, m);
    return true;
  }

  /** The gap rule: a change with the next revision moves the cursor; a later one means something was missed, so
   *  apply it and sync; an older one has been applied already. */
  _revision(ch, rev) {
    if (rev <= ch.cursor) return false;
    if (rev === ch.cursor + 1) ch.cursor = rev; else this._scheduleSync(ch);
    return true;
  }

  _receive(m) {
    const ch = this.channels.get(m.channel_id);
    if (!ch) { this.loadChannels().catch((e) => this.onError(e)); return; }
    if (!this._revision(ch, m.rev_seq)) return;
    this._store(ch, m);
    ch.last_message = m;
    const mine = m.sender && m.sender.external_id === this.me?.external_id;
    if (m.type !== 'system' && !mine && ch.id !== this.current && !m.deleted_at) ch.membership.unread_count++;
    if (ch.id === this.current && !mine && document_visible()) this._read(ch);
    this.onUpdate();
  }

  // ------------------------------------------------------------ frames
  _frame(f) {
    const d = f.data || {};
    switch (f.type) {
      case 'auth.ok': return void this._onAuthOk(d);
      case 'ack': if (d.message) { this.pending.delete(d.message.client_msg_id); this._receive(d.message); } return;
      case 'error':
        if (f.id && this.pending.has(f.id.replace(/^m:/, ''))) this.pending.delete(f.id.replace(/^m:/, ''));
        return void this.onError(Object.assign(new Error(d.message), { code: d.code, details: d.details }));
      case 'message.new': case 'message.updated': case 'message.deleted': return void this._receive(d.message);
      case 'reaction.added': case 'reaction.removed': {
        const ch = this.channels.get(d.channel_id); if (!ch) return;
        const msg = [...ch.messages.values()].find((m) => m.id === d.message_id);
        if (!this._revision(ch, d.rev_seq)) return;
        if (msg && d.rev_seq > msg.rev_seq) ch.messages.set(msg.seq, { ...msg, rev_seq: d.rev_seq, reactions: d.reactions });
        return void this.onUpdate();
      }
      case 'typing': {
        const key = `${d.channel_id}:${d.user_id}`; clearTimeout(this.typing.get(key));
        if (d.typing) this.typing.set(key, setTimeout(() => { this.typing.delete(key); this.onUpdate(); }, 6000)); else this.typing.delete(key);
        return void this.onUpdate();
      }
      case 'presence': this.online.set(d.user_id, d.online); return void this.onUpdate();
      case 'channel.added': case 'channel.updated': this._put(d.channel); return void this.onUpdate();
      case 'channel.removed': this.channels.delete(d.channel_id); if (this.current === d.channel_id) this.current = null; return void this.onUpdate();
      default: // read.updated, member.*: nothing to keep in this example
    }
  }

  // ------------------------------------------------------------ what a user does
  /** Queues a message; it is sent now or, when offline, after the next connect, with the same client_msg_id. */
  sendMessage(channelId, { text = '', attachmentIds = [], replyToId = null } = {}) {
    const id = globalThis.crypto.randomUUID();
    const p = { channelId, text, attachmentIds, replyToId };
    this.pending.set(id, p);
    this._transmit(id, p);
    this.onUpdate();
    return id;
  }

  _transmit(clientMsgId, p) {
    this._send('message.send', {
      channel_id: p.channelId, client_msg_id: clientMsgId, text: p.text,
      attachment_ids: p.attachmentIds, reply_to_id: p.replyToId,
    }, 'm:' + clientMsgId);
  }

  /** The three steps of an upload: ask for a link, send the file to the storage, confirm. Resolves to the attachment. */
  async upload({ type, size, name, data }) {
    const c = await this.api('/v1/attachments', { method: 'POST', body: { mime: type, size, file_name: name } });
    const put = await this.fetchImpl(c.upload_url, { method: 'PUT', headers: c.upload_headers, body: data });
    if (!put.ok) throw new Error(`the storage refused the upload (${put.status})`);
    return this.api(`/v1/attachments/${c.attachment.id}/complete`, { method: 'POST' });
  }

  async react(messageId, emoji, on = true) {
    this._receive(await this.api(`/v1/messages/${messageId}/reactions/${encodeURIComponent(emoji)}`, { method: on ? 'PUT' : 'DELETE' }));
  }

  async edit(messageId, text) { this._receive(await this.api(`/v1/messages/${messageId}`, { method: 'PATCH', body: { text } })); }

  async remove(messageId) { await this.api(`/v1/messages/${messageId}`, { method: 'DELETE' }); }

  /** At most every 3 seconds while someone types (spec 4.2). */
  typingStart(channelId) {
    if (Date.now() - this._lastTyping < 3000) return;
    this._lastTyping = Date.now(); this._send('typing.start', { channel_id: channelId });
  }

  typingStop(channelId) { this._send('typing.stop', { channel_id: channelId }); }

  /** Messages of the open channel in order, with what is still waiting for its ack appended. */
  view(channelId = this.current) {
    const ch = this.channels.get(channelId);
    return ch ? [...ch.messages.values()].sort((a, b) => a.seq - b.seq) : [];
  }
}

function document_visible() { return typeof document === 'undefined' || document.visibilityState === 'visible'; }
export { sleep };
