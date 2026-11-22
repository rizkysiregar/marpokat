import type { Channel, ChatCache, Message } from './types';

const KEEP_PER_CHANNEL = 50;

/**
 * IndexedDB cache: the channel list and the latest messages of each channel, so that the app shows something at once
 * and syncs afterwards. One database per app and user (`name`): another user never sees these. It only ever speeds up
 * the first paint; the server stays the truth, so every failure here is swallowed.
 */
export function indexedDbCache(name: string, idb: IDBFactory = globalThis.indexedDB): ChatCache {
  let db: Promise<IDBDatabase> | null = null;
  const open = () =>
    (db ??= new Promise<IDBDatabase>((resolve, reject) => {
      const req = idb.open(`marpokat:${name}`, 1);
      req.onupgradeneeded = () => {
        req.result.createObjectStore('channels', { keyPath: 'id' });
        req.result.createObjectStore('messages', { keyPath: 'channelId' });
      };
      req.onsuccess = () => resolve(req.result);
      req.onerror = () => reject(req.error);
    }));
  const run = async <T>(store: string, mode: IDBTransactionMode, f: (s: IDBObjectStore) => IDBRequest<T> | void): Promise<T | undefined> => {
    try {
      const d = await open();
      return await new Promise<T | undefined>((resolve, reject) => {
        const tx = d.transaction(store, mode);
        const req = f(tx.objectStore(store));
        tx.oncomplete = () => resolve(req ? req.result : undefined);
        tx.onerror = () => reject(tx.error);
        tx.onabort = () => reject(tx.error);
      });
    } catch {
      return undefined;
    }
  };
  return {
    async load() {
      const channels = (await run<Channel[]>('channels', 'readonly', (s) => s.getAll())) ?? [];
      const rows = (await run<{ channelId: string; messages: Message[] }[]>('messages', 'readonly', (s) => s.getAll())) ?? [];
      if (!channels.length) return null;
      return { channels, messages: Object.fromEntries(rows.map((r) => [r.channelId, r.messages])) };
    },
    async saveChannels(channels) {
      await run('channels', 'readwrite', (s) => {
        s.clear();
        for (const c of channels) s.put(c);
      });
    },
    async saveMessages(channelId, messages) {
      await run('messages', 'readwrite', (s) => {
        s.put({ channelId, messages: messages.slice(-KEEP_PER_CHANNEL) });
      });
    },
    async clear() {
      await run('channels', 'readwrite', (s) => void s.clear());
      await run('messages', 'readwrite', (s) => void s.clear());
    },
  };
}
