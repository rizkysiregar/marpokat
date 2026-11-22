import type { components } from './generated/api';

export type User = components['schemas']['User'];
export type Channel = components['schemas']['Channel'];
export type Message = components['schemas']['Message'];
export type Attachment = components['schemas']['Attachment'];
export type ReactionSummary = components['schemas']['ReactionSummary'];

/** A channel as the client keeps it: the server's view plus what the client knows about its history. */
export interface ChannelState extends Channel {
  /** Highest `rev_seq` applied: changes after it are fetched with sync. */
  cursor: number;
  /** True once the history was loaded at least once (the cursor is meaningful). */
  loaded: boolean;
  /** True when older messages exist that were not loaded. */
  hasOlder: boolean;
}

export type PendingStatus = 'sending' | 'failed';

/** A message the user sent that the server has not acknowledged. Shown after the real messages. */
export interface PendingMessage {
  clientMsgId: string;
  channelId: string;
  text: string;
  attachmentIds: string[];
  replyToId: string | null;
  status: PendingStatus;
  error?: { code: string; message: string };
  createdAt: number;
}

export type ConnectionStatus = 'idle' | 'connecting' | 'connected' | 'reconnecting' | 'closed';

export interface TokenProvider {
  (): Promise<string>;
}

export interface ClientOptions {
  /** e.g. https://api-marpokat.example.com */
  baseUrl: string;
  apiKey: string;
  /** Called to get a user token, and again when the last one expired. */
  tokenProvider: TokenProvider;
  /** Persists channels and the latest messages between page loads. */
  cache?: ChatCache;
  /** Injected for tests and for Node (`ws`). */
  WebSocketImpl?: typeof WebSocket;
  fetchImpl?: typeof fetch;
  /** Reconnect backoff base in ms (full jitter, capped at 30 s). */
  backoffBase?: number;
  random?: () => number;
  /** How many messages to load when a channel is opened. */
  pageSize?: number;
}

export interface ChatCache {
  load(): Promise<{ channels: Channel[]; messages: Record<string, Message[]> } | null>;
  saveChannels(channels: Channel[]): Promise<void>;
  saveMessages(channelId: string, messages: Message[]): Promise<void>;
  clear(): Promise<void>;
}

export interface UploadProgress {
  loaded: number;
  total: number;
}

export interface UploadInput {
  data: Blob;
  /** Defaults to `data.type`. */
  mime?: string;
  name?: string;
  onProgress?: (p: UploadProgress) => void;
  signal?: AbortSignal;
}

export interface Snapshot {
  /** Changes on every update: for `useSyncExternalStore` and memoization. */
  version: number;
  status: ConnectionStatus;
  statusDetail: string;
  me: User | null;
  /** Channels, most recently active first. */
  channels: ChannelState[];
  /** Messages of channels that were loaded, by channel id, in `seq` order. */
  messages: Record<string, Message[]>;
  pending: Record<string, PendingMessage[]>;
  /** Users that are typing, by channel id. */
  typing: Record<string, string[]>;
  online: Record<string, boolean>;
  unreadTotal: number;
}

export class MarpokatError extends Error {
  constructor(
    message: string,
    readonly code: string,
    readonly status?: number,
    readonly details?: Record<string, unknown>,
  ) {
    super(message);
    this.name = 'MarpokatError';
  }
}
