package com.rizkysiregar.marpokat.android

import com.google.common.truth.Truth.assertThat
import org.junit.Test

class MarpokatPushTest {
    private val dm = mapOf(
        "marpokat" to "1", "type" to "message.new", "channel_id" to "c1", "channel_type" to "dm", "channel_name" to "",
        "message_id" to "m1", "seq" to "57", "sender_id" to "u_456", "sender_name" to "Budi", "sender_avatar_url" to "https://x/a.png",
        "text" to "halo!", "attachment_kind" to "", "unread_count" to "3",
    )

    @Test fun readsTheFieldsOfADirectMessage() {
        val push = MarpokatPush.parse(dm)!!
        assertThat(push.channelId).isEqualTo("c1")
        assertThat(push.seq).isEqualTo(57)
        assertThat(push.unreadCount).isEqualTo(3)
        assertThat(push.senderAvatarUrl).isEqualTo("https://x/a.png")
        assertThat(push.attachmentKind).isNull()
        assertThat(push.title).isEqualTo("Budi")
    }

    @Test fun aGroupIsTitledWithItsName() {
        val push = MarpokatPush.parse(dm + mapOf("channel_type" to "group", "channel_name" to "Weekend trip", "attachment_kind" to "image", "text" to ""))!!
        assertThat(push.title).isEqualTo("Weekend trip")
        assertThat(push.attachmentKind).isEqualTo("image")
    }

    @Test fun ignoresWhatIsNotAMarpokatMessagePush() {
        assertThat(MarpokatPush.isMarpokat(mapOf("type" to "promo"))).isFalse()
        assertThat(MarpokatPush.parse(mapOf("type" to "promo"))).isNull()
        assertThat(MarpokatPush.parse(dm - "marpokat")).isNull()
        assertThat(MarpokatPush.parse(dm + ("type" to "member.added"))).isNull()
    }

    @Test fun refusesAPushWithoutWhatANotificationNeeds() {
        assertThat(MarpokatPush.parse(dm - "channel_id")).isNull()
        assertThat(MarpokatPush.parse(dm - "message_id")).isNull()
    }

    @Test fun toleratesMissingOptionalFields() {
        val push = MarpokatPush.parse(mapOf("marpokat" to "1", "type" to "message.new", "channel_id" to "c", "message_id" to "m"))!!
        assertThat(push.unreadCount).isEqualTo(1)
        assertThat(push.text).isEmpty()
    }
}
