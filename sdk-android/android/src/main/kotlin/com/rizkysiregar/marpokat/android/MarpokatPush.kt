package com.rizkysiregar.marpokat.android

/** What a Marpokat push says. FCM data values are all strings; this is the typed form of spec/events.md section 11. */
public data class PushMessage(
    val channelId: String,
    /** `dm` or `group`. */
    val channelType: String,
    val channelName: String,
    val messageId: String,
    val seq: Long,
    val senderId: String,
    val senderName: String,
    val senderAvatarUrl: String?,
    /** The first 200 characters, empty for media. */
    val text: String,
    /** `image`, `video` or `file` when the message carries an attachment. */
    val attachmentKind: String?,
    val unreadCount: Int,
) {
    /** The title of a notification: the group's name, or the sender for a DM. */
    public val title: String
        get() = if (channelType == "group" && channelName.isNotBlank()) channelName else senderName
}

/**
 * Helpers for `FirebaseMessagingService`:
 *
 * ```
 * override fun onMessageReceived(message: RemoteMessage) {
 *     val push = MarpokatPush.parse(message.data) ?: return super.onMessageReceived(message) // not ours
 *     // build the notification from `push`, open `push.channelId` when it is tapped
 * }
 * override fun onNewToken(token: String) { scope.launch { client.registerPushToken(token) } }
 * ```
 */
public object MarpokatPush {
    /** The extra of the notification's intent the app can use to open the right conversation. */
    public const val EXTRA_CHANNEL_ID: String = "marpokat.channel_id"

    /** True for pushes sent by Marpokat: apps with their own pushes tell them apart with this. */
    public fun isMarpokat(data: Map<String, String>): Boolean = data["marpokat"] != null

    /** The push, or null when it is not a Marpokat message push (or lacks what a notification needs). */
    public fun parse(data: Map<String, String>): PushMessage? {
        if (!isMarpokat(data) || data["type"] != "message.new") return null
        val channelId = data["channel_id"]?.takeIf { it.isNotBlank() } ?: return null
        val messageId = data["message_id"]?.takeIf { it.isNotBlank() } ?: return null
        return PushMessage(
            channelId = channelId,
            channelType = data["channel_type"] ?: "dm",
            channelName = data["channel_name"].orEmpty(),
            messageId = messageId,
            seq = data["seq"]?.toLongOrNull() ?: 0,
            senderId = data["sender_id"].orEmpty(),
            senderName = data["sender_name"].orEmpty(),
            senderAvatarUrl = data["sender_avatar_url"]?.takeIf { it.isNotBlank() },
            text = data["text"].orEmpty(),
            attachmentKind = data["attachment_kind"]?.takeIf { it.isNotBlank() },
            unreadCount = data["unread_count"]?.toIntOrNull() ?: 1,
        )
    }
}
