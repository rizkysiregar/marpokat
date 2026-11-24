/** What a Marpokat push says. FCM data values are all strings; this is the typed form of spec/events.md section 11. */
export interface PushMessage {
  channelId: string;
  /** `dm` or `group`. */
  channelType: string;
  channelName: string;
  messageId: string;
  seq: number;
  senderId: string;
  senderName: string;
  senderAvatarUrl: string | null;
  /** The first 200 characters, empty for media. */
  text: string;
  /** `image`, `video` or `file` when the message carries an attachment. */
  attachmentKind: string | null;
  unreadCount: number;
  /** The title of a notification: the group's name, or the sender for a DM. */
  title: string;
}

/** True for pushes sent by Marpokat: apps with their own pushes tell them apart with this. */
export function isMarpokatPush(data: Record<string, string> | undefined | null): boolean {
  return data?.marpokat != null;
}

/**
 * The push, or null when it is not a Marpokat message push (or lacks what a notification needs). For the service
 * worker's `onBackgroundMessage`: Marpokat pushes are data-only, so the app shows the notification itself.
 * This module has no dependency on the client, so a service worker bundle stays small.
 */
export function parsePush(data: Record<string, string> | undefined | null): PushMessage | null {
  if (!data || !isMarpokatPush(data) || data.type !== 'message.new') return null;
  const channelId = data.channel_id?.trim();
  const messageId = data.message_id?.trim();
  if (!channelId || !messageId) return null;
  const channelType = data.channel_type || 'dm';
  const channelName = data.channel_name ?? '';
  const senderName = data.sender_name ?? '';
  const seq = Number(data.seq);
  const unread = Number.parseInt(data.unread_count ?? '', 10);
  return {
    channelId, channelType, channelName, messageId,
    seq: Number.isFinite(seq) ? seq : 0,
    senderId: data.sender_id ?? '',
    senderName,
    senderAvatarUrl: data.sender_avatar_url?.trim() || null,
    text: data.text ?? '',
    attachmentKind: data.attachment_kind?.trim() || null,
    unreadCount: Number.isFinite(unread) ? unread : 1,
    title: channelType === 'group' && channelName.trim() ? channelName : senderName,
  };
}
