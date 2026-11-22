import { createContext, createElement, useCallback, useContext, useEffect, useRef, useSyncExternalStore, type ReactNode } from 'react';
import type { MarpokatClient, Message, PendingMessage, Snapshot, UploadInput, Attachment } from '@marpokat/core';

const Context = createContext<MarpokatClient | null>(null);

/** Gives the hooks below a client. The provider connects it and disconnects on unmount (strict mode safe). */
export function MarpokatProvider({ client, children, autoConnect = true }: { client: MarpokatClient; children?: ReactNode; autoConnect?: boolean }) {
  useEffect(() => {
    if (!autoConnect) return;
    void client.connect();
    return () => client.disconnect();
  }, [client, autoConnect]);
  return createElement(Context.Provider, { value: client }, children);
}

export function useClient(): MarpokatClient {
  const c = useContext(Context);
  if (!c) throw new Error('useClient must be used inside <MarpokatProvider>');
  return c;
}

/** The whole snapshot. Prefer the narrower hooks: this one re-renders on every change. */
export function useSnapshot(): Snapshot {
  const c = useClient();
  return useSyncExternalStore(c.subscribe, c.getSnapshot, c.getSnapshot);
}

const NONE: never[] = [];

export function useConnectionStatus() {
  const s = useSnapshot();
  return { status: s.status, detail: s.statusDetail };
}

export function useChannels() {
  const s = useSnapshot();
  return { channels: s.channels, online: s.online, loading: s.status !== 'connected' && s.channels.length === 0 };
}

export function useUnreadCount(channelId?: string): number {
  const s = useSnapshot();
  if (!channelId) return s.unreadTotal;
  return s.channels.find((c) => c.id === channelId)?.membership.unread_count ?? 0;
}

/** Opens a channel while the component is mounted: loads its history, syncs it and marks it read. */
export function useChannel(channelId: string | null | undefined) {
  const client = useClient();
  const s = useSnapshot();
  const error = useRef<Error | null>(null);
  useEffect(() => {
    if (!channelId || s.status !== 'connected') return;
    let live = true;
    client.openChannel(channelId).catch((e: Error) => { if (live) error.current = e; });
    return () => { live = false; client.closeChannel(channelId); };
    // reopen after every reconnect: the client then syncs from its cursor
  }, [client, channelId, s.status === 'connected']); // eslint-disable-line react-hooks/exhaustive-deps
  const channel = channelId ? s.channels.find((c) => c.id === channelId) : undefined;
  return { channel, loading: !channel || !channel.loaded, error: error.current };
}

export interface MessagesResult {
  messages: Message[];
  pending: PendingMessage[];
  hasOlder: boolean;
  loadOlder: () => Promise<void>;
}

export function useMessages(channelId: string | null | undefined): MessagesResult {
  const client = useClient();
  const s = useSnapshot();
  const id = channelId ?? '';
  const loadOlder = useCallback(async () => { if (id) await client.loadOlder(id); }, [client, id]);
  return {
    messages: s.messages[id] ?? NONE,
    pending: s.pending[id] ?? NONE,
    hasOlder: s.channels.find((c) => c.id === id)?.hasOlder ?? false,
    loadOlder,
  };
}

export function useSendMessage(channelId: string) {
  const client = useClient();
  return {
    send: useCallback((input: { text?: string; attachmentIds?: string[]; replyToId?: string | null }) => client.send(channelId, input), [client, channelId]),
    retry: useCallback((clientMsgId: string) => client.retry(clientMsgId), [client]),
    discard: useCallback((clientMsgId: string) => client.discard(clientMsgId), [client]),
    upload: useCallback((input: UploadInput): Promise<Attachment> => client.upload(input), [client]),
  };
}

export function useMessageActions() {
  const client = useClient();
  return {
    react: useCallback((messageId: string, emoji: string, on = true) => client.react(messageId, emoji, on), [client]),
    edit: useCallback((messageId: string, text: string) => client.edit(messageId, text), [client]),
    remove: useCallback((messageId: string) => client.remove(messageId), [client]),
  };
}

/** Who is typing in a channel, and `onInput` to call from the input's change handler (it throttles itself). */
export function useTyping(channelId: string) {
  const client = useClient();
  const s = useSnapshot();
  const stop = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => () => clearTimeout(stop.current), []);
  const onInput = useCallback(() => {
    client.typingStart(channelId);
    clearTimeout(stop.current);
    stop.current = setTimeout(() => client.typingStop(channelId), 4000);
  }, [client, channelId]);
  const me = s.me?.external_id;
  return { typing: (s.typing[channelId] ?? NONE).filter((u) => u !== me), onInput, stop: useCallback(() => client.typingStop(channelId), [client, channelId]) };
}

export type { MarpokatClient, Message, PendingMessage, Snapshot } from '@marpokat/core';
