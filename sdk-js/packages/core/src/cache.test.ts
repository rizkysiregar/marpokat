import 'fake-indexeddb/auto';
import { describe, expect, it } from 'vitest';
import { indexedDbCache } from './cache';

describe('indexedDbCache', () => {
  it('keeps channels and the latest messages, separately per name', async () => {
    const a = indexedDbCache('app-alice'), b = indexedDbCache('app-bob');
    await a.saveChannels([{ id: 'c1' } as any, { id: 'c2' } as any]);
    await a.saveMessages('c1', Array.from({ length: 80 }, (_, i) => ({ seq: i + 1 }) as any));
    const loaded = await a.load();
    expect(loaded!.channels.map((c) => c.id)).toEqual(['c1', 'c2']);
    expect(loaded!.messages.c1).toHaveLength(50);
    expect(loaded!.messages.c1![0]!.seq).toBe(31); // the newest 50
    expect(await b.load()).toBeNull();
    await a.saveChannels([{ id: 'c3' } as any]);
    expect((await a.load())!.channels.map((c) => c.id)).toEqual(['c3']);
    await a.clear();
    expect(await a.load()).toBeNull();
  });
});
