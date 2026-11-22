import { act, cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { createElement } from 'react';
import { MarpokatProvider, useChannels, useMessages, useSendMessage, useUnreadCount } from './index';
import type { MarpokatClient, Snapshot } from '@rizkysiregar/marpokat-core';

afterEach(cleanup);

function fakeClient(initial: Partial<Snapshot>) {
  let snap = { version: 1, status: 'connected', statusDetail: '', me: null, channels: [], messages: {}, pending: {}, typing: {}, online: {}, unreadTotal: 0, ...initial } as Snapshot;
  const listeners = new Set<() => void>();
  const client = {
    subscribe: (l: () => void) => { listeners.add(l); return () => listeners.delete(l); },
    getSnapshot: () => snap,
    connect: vi.fn(async () => {}), disconnect: vi.fn(), send: vi.fn(() => 'id1'), openChannel: vi.fn(async () => {}), closeChannel: vi.fn(),
  } as unknown as MarpokatClient;
  return { client, set(p: Partial<Snapshot>) { snap = { ...snap, ...p, version: snap.version + 1 }; listeners.forEach((l) => l()); } };
}

const ch = (id: string, unread: number) => ({ id, type: 'dm', membership: { unread_count: unread } }) as any;

describe('hooks', () => {
  it('connects on mount, disconnects on unmount, and re-renders when the snapshot changes', () => {
    const f = fakeClient({ channels: [ch('a', 2)], unreadTotal: 2 });
    function View() {
      const { channels } = useChannels();
      return createElement('div', null, `${channels.length} channels, ${useUnreadCount()} unread, a=${useUnreadCount('a')}`);
    }
    const { unmount } = render(createElement(MarpokatProvider, { client: f.client }, createElement(View)));
    expect(f.client.connect).toHaveBeenCalledTimes(1);
    expect(screen.getByText('1 channels, 2 unread, a=2')).toBeTruthy();
    act(() => f.set({ channels: [ch('a', 0), ch('b', 5)], unreadTotal: 5 }));
    expect(screen.getByText('2 channels, 5 unread, a=0')).toBeTruthy();
    unmount();
    expect(f.client.disconnect).toHaveBeenCalledTimes(1);
  });

  it('gives messages and pending of one channel with stable empty arrays', () => {
    const f = fakeClient({ messages: { a: [{ id: 'm1', text: 'hi' } as any] }, pending: { a: [{ clientMsgId: 'p1', text: 'wait' } as any] } });
    const seen: unknown[] = [];
    function View({ id }: { id: string }) {
      const r = useMessages(id);
      seen.push(r.messages);
      return createElement('div', null, `${r.messages.length}/${r.pending.length}`);
    }
    const { rerender } = render(createElement(MarpokatProvider, { client: f.client }, createElement(View, { id: 'a' })));
    expect(screen.getByText('1/1')).toBeTruthy();
    rerender(createElement(MarpokatProvider, { client: f.client }, createElement(View, { id: 'zzz' })));
    rerender(createElement(MarpokatProvider, { client: f.client }, createElement(View, { id: 'zzz' })));
    expect(screen.getByText('0/0')).toBeTruthy();
    expect(seen.at(-1)).toBe(seen.at(-2)); // the same empty array: memoized children do not re-render
  });

  it('sends through the client', () => {
    const f = fakeClient({});
    let send!: ReturnType<typeof useSendMessage>['send'];
    function View() { send = useSendMessage('a').send; return null; }
    render(createElement(MarpokatProvider, { client: f.client }, createElement(View)));
    expect(send({ text: 'hello' })).toBe('id1');
    expect(f.client.send).toHaveBeenCalledWith('a', { text: 'hello' });
  });

  it('refuses to be used without a provider', () => {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {});
    expect(() => render(createElement(() => { useChannels(); return null; }))).toThrow(/MarpokatProvider/);
    spy.mockRestore();
  });
});
