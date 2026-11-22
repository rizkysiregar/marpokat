package com.rizkysiregar.marpokat

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonElement

/** The JSON settings every part of the SDK uses: new fields the server adds never break an old app. */
public val MarpokatJson: Json = Json {
    ignoreUnknownKeys = true
    explicitNulls = false
    coerceInputValues = true
    encodeDefaults = true
}

@Serializable
public data class User(
    val id: String = "",
    @SerialName("external_id") val externalId: String,
    val name: String = "",
    @SerialName("avatar_url") val avatarUrl: String? = null,
    val deleted: Boolean = false,
)

@Serializable
public data class Membership(
    val role: String = "member",
    @SerialName("last_read_seq") val lastReadSeq: Long = 0,
    @SerialName("unread_count") val unreadCount: Int = 0,
    @SerialName("muted_until") val mutedUntil: String? = null,
)

@Serializable
public data class ReactionSummary(
    val counts: Map<String, Int> = emptyMap(),
    val mine: List<String> = emptyList(),
)

@Serializable
public data class Attachment(
    val id: String,
    val kind: String,
    val mime: String = "",
    val size: Long = 0,
    @SerialName("file_name") val fileName: String? = null,
    val width: Int? = null,
    val height: Int? = null,
    @SerialName("duration_ms") val durationMs: Int? = null,
    val blurhash: String? = null,
    val status: String = "pending",
    val url: String? = null,
    @SerialName("thumbnail_url") val thumbnailUrl: String? = null,
) {
    val isReady: Boolean get() = status == "ready"
}

@Serializable
public data class MessagePreview(
    val id: String,
    val seq: Long = 0,
    @SerialName("sender_id") val senderId: String? = null,
    val text: String? = null,
    @SerialName("has_attachments") val hasAttachments: Boolean = false,
    val deleted: Boolean = false,
)

@Serializable
public data class Message(
    val id: String,
    @SerialName("channel_id") val channelId: String,
    val seq: Long,
    @SerialName("rev_seq") val revSeq: Long,
    @SerialName("client_msg_id") val clientMsgId: String? = null,
    /** `text`, `media` or `system`. */
    val type: String = "text",
    val sender: User? = null,
    val text: String? = null,
    val attachments: List<Attachment> = emptyList(),
    @SerialName("reply_to") val replyTo: MessagePreview? = null,
    val reactions: ReactionSummary = ReactionSummary(),
    val metadata: Map<String, JsonElement> = emptyMap(),
    @SerialName("created_at") val createdAt: String = "",
    @SerialName("edited_at") val editedAt: String? = null,
    @SerialName("deleted_at") val deletedAt: String? = null,
) {
    val isDeleted: Boolean get() = deletedAt != null
    val isSystem: Boolean get() = type == "system"
}

@Serializable
public data class Channel(
    val id: String,
    /** `dm` or `group`. */
    val type: String,
    val name: String? = null,
    @SerialName("avatar_url") val avatarUrl: String? = null,
    @SerialName("member_count") val memberCount: Int = 0,
    @SerialName("dm_peer") val dmPeer: User? = null,
    @SerialName("last_seq") val lastSeq: Long = 0,
    @SerialName("last_message_seq") val lastMessageSeq: Long = 0,
    @SerialName("last_message") val lastMessage: Message? = null,
    @SerialName("last_message_at") val lastMessageAt: String? = null,
    val frozen: Boolean = false,
    @SerialName("created_at") val createdAt: String = "",
    val membership: Membership = Membership(),
) {
    /** The name to show: the group's name, or the other person for a DM. */
    val title: String
        get() = name?.takeIf { it.isNotBlank() } ?: dmPeer?.name?.takeIf { it.isNotBlank() } ?: dmPeer?.externalId ?: id
}

/** A channel as the client keeps it: what the server said plus what the client knows about its history. */
public data class ChannelState(
    val channel: Channel,
    /** Highest `rev_seq` applied: changes after it are fetched with sync. */
    val cursor: Long = 0,
    /** True once the history was loaded at least once (the cursor means something). */
    val loaded: Boolean = false,
    /** True when older messages exist that were not loaded. */
    val hasOlder: Boolean = true,
) {
    val id: String get() = channel.id
}

public enum class PendingStatus { Sending, Failed }

/** A message the user sent that the server has not acknowledged yet. Shown after the real messages. */
public data class PendingMessage(
    val clientMsgId: String,
    val channelId: String,
    val text: String,
    val attachmentIds: List<String> = emptyList(),
    val replyToId: String? = null,
    val status: PendingStatus = PendingStatus.Sending,
    val errorCode: String? = null,
    val errorMessage: String? = null,
    val createdAtMs: Long = 0,
)

public enum class ConnectionStatus { Idle, Connecting, Connected, Reconnecting, Closed }

/** Everything a chat screen shows. Each change makes a new value. */
public data class ChatState(
    val status: ConnectionStatus = ConnectionStatus.Idle,
    val statusDetail: String = "",
    val me: User? = null,
    /** Most recently active first. */
    val channels: List<ChannelState> = emptyList(),
    /** Messages of the channels that were loaded, in `seq` order. */
    val messages: Map<String, List<Message>> = emptyMap(),
    val pending: Map<String, List<PendingMessage>> = emptyMap(),
    /** External ids of the users typing, by channel. */
    val typing: Map<String, List<String>> = emptyMap(),
    val online: Map<String, Boolean> = emptyMap(),
) {
    val unreadTotal: Int get() = channels.sumOf { it.channel.membership.unreadCount }
    public fun channel(id: String): ChannelState? = channels.firstOrNull { it.id == id }
}

public data class UploadProgress(val loaded: Long, val total: Long)

/** The server refused a call, or could not be reached (`status` 0). */
public class MarpokatException(
    message: String,
    public val code: String,
    public val status: Int = 0,
    cause: Throwable? = null,
) : RuntimeException(message, cause)

/** The tenant's own backend gives user tokens; the SDK calls this at connect time and again when one ran out. */
public fun interface TokenProvider {
    public suspend fun token(): String
}

/** Keeps channels and the latest messages between app starts. The server stays the truth; failures here are ignored. */
public interface ChatCache {
    public suspend fun load(): CachedChat?
    public suspend fun saveChannels(channels: List<Channel>)
    public suspend fun saveMessages(channelId: String, messages: List<Message>)
    public suspend fun clear()
}

public data class CachedChat(val channels: List<Channel>, val messages: Map<String, List<Message>>)
