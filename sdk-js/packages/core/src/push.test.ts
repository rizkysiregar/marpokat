import { describe, expect, it } from 'vitest';
import { isMarpokatPush, parsePush } from './push';

const data = {
  marpokat: '1', type: 'message.new', channel_id: 'ch-1', channel_type: 'dm', channel_name: '', message_id: 'm-1',
  seq: '57', sender_id: 'u-1', sender_name: 'Budi', sender_avatar_url: 'https://a.test/b.png', text: 'halo',
  attachment_kind: '', unread_count: '3',
};

describe('parsePush', () => {
  it('reads a message push', () => {
    expect(parsePush(data)).toEqual({
      channelId: 'ch-1', channelType: 'dm', channelName: '', messageId: 'm-1', seq: 57, senderId: 'u-1', senderName: 'Budi',
      senderAvatarUrl: 'https://a.test/b.png', text: 'halo', attachmentKind: null, unreadCount: 3, title: 'Budi',
    });
  });

  it('titles a group push with the group name', () => {
    expect(parsePush({ ...data, channel_type: 'group', channel_name: 'Tim' })?.title).toBe('Tim');
    expect(parsePush({ ...data, channel_type: 'group', channel_name: ' ' })?.title).toBe('Budi');
  });

  it('is null for pushes that are not Marpokat message pushes, or lack what a notification needs', () => {
    expect(parsePush({ type: 'POST_LIKED', postId: 'p1' })).toBeNull();
    expect(parsePush({ ...data, type: 'other' })).toBeNull();
    expect(parsePush({ ...data, channel_id: ' ' })).toBeNull();
    expect(parsePush({ marpokat: '1', type: 'message.new' })).toBeNull();
    expect(parsePush(undefined)).toBeNull();
    expect(isMarpokatPush({ type: 'POST_LIKED' })).toBe(false);
  });

  it('falls back when numbers are missing', () => {
    const p = parsePush({ marpokat: '1', type: 'message.new', channel_id: 'c', message_id: 'm' });
    expect(p).toMatchObject({ seq: 0, unreadCount: 1, channelType: 'dm', senderAvatarUrl: null });
  });
});
