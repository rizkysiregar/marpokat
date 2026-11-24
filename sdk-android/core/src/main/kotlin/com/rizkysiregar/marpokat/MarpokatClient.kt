package com.rizkysiregar.marpokat

import kotlinx.coroutines.CoroutineDispatcher
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Deferred
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.async
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.channels.BufferOverflow
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import kotlinx.serialization.builtins.ListSerializer
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.boolean
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.contentOrNull
import kotlinx.serialization.json.int
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.long
import kotlinx.serialization.json.put
import java.net.URLEncoder
import java.util.TreeMap
import java.util.UUID
import kotlin.math.min
import kotlin.math.pow

public class MarpokatOptions(
    /** e.g. https://api-marpokat.example.com */
    public val baseUrl: String,
    public val apiKey: String,
    public val tokenProvider: TokenProvider,
    public val cache: ChatCache? = null,
    public val http: HttpTransport = OkHttpTransport(),
    public val sockets: SocketFactory? = null,
    /** Everything the client does with its state runs here, one coroutine at a time. */
    public val dispatcher: CoroutineDispatcher = Dispatchers.Default,
    public val backoffBaseMs: Long = 500,
    public val random: () -> Double = { Math.random() },
    public val nowMs: () -> Long = { System.currentTimeMillis() },
    public val newId: () -> String = { UUID.randomUUID().toString() },
    public val pageSize: Int = 50,
)

private const val PROTOCOL_VERSION = 1
private const val MAX_BACKOFF_MS = 30_000.0
private const val MAX_PUSH_RETRY_MS = 300_000.0
/** A push token the server accepted is sent again after this long, on the next connection: servers trim old ones. */
private const val PUSH_TOKEN_REFRESH_MS = 24 * 60 * 60 * 1000L

/**
 * The Marpokat client: one WebSocket, the REST API and the state a chat screen needs. It keeps the rules of
 * spec/events.md: auth in the first frame, ping every heartbeat, a `rev_seq` cursor per channel with a gap rule,
 * reconnect with full jitter followed by sync, and idempotent sends (a message written offline goes out once the
 * connection is back, with the same `client_msg_id`).
 *
 * Observe [state]; every change makes a new [ChatState]. Calls are safe from any thread.
 */
@OptIn(ExperimentalCoroutinesApi::class)
public class MarpokatClient(private val options: MarpokatOptions) {
    private val baseUrl = options.baseUrl.trimEnd('/')
    private val sockets: SocketFactory = options.sockets ?: (options.http as? SocketFactory) ?: OkHttpTransport()
    private val loop = options.dispatcher.limitedParallelism(1)
    private val scope = CoroutineScope(SupervisorJob() + loop)

    private val _state = MutableStateFlow(ChatState())
    public val state: StateFlow<ChatState> = _state

    private val _incoming = MutableSharedFlow<IncomingMessage>(extraBufferCapacity = 64, onBufferOverflow = BufferOverflow.DROP_OLDEST)

    /**
     * Messages from other people as they arrive over the live connection, one event each: not your own, not system
     * messages, not deleted ones, not what a sync or a page of history brings back, and never the same message twice.
     * For an in-app banner or notification while the app is open (when it is closed, the server sends a push
     * instead). Nothing is replayed to a collector that starts late; the conversations themselves are in [state].
     */
    public val incoming: SharedFlow<IncomingMessage> = _incoming

    // ---- everything below is touched only on `loop`
    private var status = ConnectionStatus.Idle
    private var statusDetail = ""
    private var me: User? = null
    private val channels = LinkedHashMap<String, ChannelState>()
    private val messages = HashMap<String, TreeMap<Long, Message>>()
    private val pending = LinkedHashMap<String, PendingMessage>()
    private val typing = HashMap<String, MutableMap<String, Job>>()
    private val online = HashMap<String, Boolean>()
    private var current: String? = null
    private var socket: Socket? = null
    private var token = ""
    private var attempt = 0
    private var stopped = true
    private var pingJob: Job? = null
    private var retryJob: Job? = null
    private val syncJobs = HashMap<String, Job>()
    private val syncing = HashMap<String, Deferred<Unit>>()
    private var channelsLoading: Deferred<Unit>? = null
    private var lastTypingMs: Long? = null
    private var pushToken: String? = null // what the app asked to register
    private var pushTokenSentAt: Long? = null // when the server accepted pushToken, null while it has not
    private var pushJob: Job? = null
    private var pushAttempt = 0

    // ------------------------------------------------------------------ connection
    /** Starts connecting and returns at once; follow [ChatState.status]. */
    public suspend fun connect(): Unit = withContext(loop) {
        if (!stopped) return@withContext
        stopped = false
        hydrateFromCache()
        open()
    }

    public suspend fun disconnect(): Unit = withContext(loop) {
        stopped = true
        retryJob?.cancel()
        pingJob?.cancel()
        syncJobs.values.forEach { it.cancel() }
        syncJobs.clear()
        socket?.let { s -> socket = null; s.close(1000) }
        setStatus(ConnectionStatus.Closed)
    }

    /** Forgets everything of this user (sign out). */
    public suspend fun reset() {
        disconnect()
        withContext(loop) {
            channels.clear(); messages.clear(); pending.clear(); online.clear()
            typing.values.forEach { m -> m.values.forEach { it.cancel() } }
            typing.clear()
            me = null; current = null; token = ""
            pushJob?.cancel(); pushJob = null; pushToken = null; pushTokenSentAt = null; pushAttempt = 0
            status = ConnectionStatus.Idle
            publish()
        }
        runCatching { options.cache?.clear() }
    }

    /** Stops the client for good, for example when the process ends. The state stays readable. */
    public fun close() {
        scope.cancel()
    }

    private suspend fun hydrateFromCache() {
        val cache = options.cache ?: return
        if (channels.isNotEmpty()) return
        val cached = runCatching { cache.load() }.getOrNull() ?: return
        cached.channels.forEach { channels[it.id] = ChannelState(it) }
        cached.messages.forEach { (id, list) -> messages[id] = TreeMap(list.associateBy { it.seq }) }
        publish()
    }

    private suspend fun open() {
        retryJob?.cancel()
        token = try {
            options.tokenProvider.token()
        } catch (e: kotlinx.coroutines.CancellationException) {
            throw e
        } catch (e: Exception) {
            stopped = true
            setStatus(ConnectionStatus.Closed, "no token: ${e.message}")
            return
        }
        setStatus(if (attempt == 0) ConnectionStatus.Connecting else ConnectionStatus.Reconnecting, statusDetail)
        lateinit var created: Socket
        created = sockets.open(baseUrl.replaceFirst("http", "ws") + "/v1/ws", object : SocketListener {
            override fun onOpen() {
                scope.launch {
                    if (socket === created) sendFrame("auth", buildJsonObject {
                        put("api_key", options.apiKey); put("token", token); put("protocol_version", PROTOCOL_VERSION)
                    }, "auth")
                }
            }
            override fun onText(text: String) {
                scope.launch { if (socket === created) onFrame(text) }
            }
            override fun onClosed(code: Int) {
                scope.launch { if (socket === created) onSocketClosed(code) }
            }
        })
        socket = created
    }

    private fun sendFrame(type: String, data: JsonObject, id: String? = null): Boolean {
        val frame = buildJsonObject {
            put("type", type)
            put("data", data)
            if (id != null) put("id", id)
        }
        return socket?.send(frame.toString()) ?: false
    }

    private fun onSocketClosed(code: Int) {
        pingJob?.cancel()
        socket = null
        if (code == 4003 || code == 4005) { // credentials or protocol not accepted: trying again cannot help
            stopped = true
            setStatus(ConnectionStatus.Closed, "closed $code: credentials or protocol not accepted")
            return
        }
        if (stopped) return
        // 4004: the token ended, the provider gives a new one. 1012: the server restarts: come back at once, once.
        val delayMs = if (code == 4004 || (code == 1012 && attempt == 0)) 0L
        else (options.random() * min(MAX_BACKOFF_MS, options.backoffBaseMs * 2.0.pow(attempt))).toLong()
        attempt++
        setStatus(ConnectionStatus.Reconnecting, "closed $code, retrying in $delayMs ms")
        retryJob = scope.launch {
            delay(delayMs)
            open()
        }
    }

    private suspend fun onAuthOk(data: JsonObject) {
        me = MarpokatJson.decodeFromJsonElement(User.serializer(), data.getValue("user"))
        attempt = 0
        pingJob?.cancel()
        val interval = data["heartbeat_interval_ms"]?.jsonPrimitive?.long ?: 25_000
        pingJob = scope.launch {
            while (isActive) {
                delay(interval)
                sendFrame("ping", JsonObject(emptyMap()))
            }
        }
        setStatus(ConnectionStatus.Connected)
        try {
            loadChannels() // what changed in the list (spec 7.4)
            current?.let { id -> if (channels[id]?.loaded == true) sync(id) } // the open channel from its cursor (7.5)
        } catch (e: MarpokatException) {
            setStatus(ConnectionStatus.Connected, "sync failed: ${e.message}")
        }
        pending.values.filter { it.status == PendingStatus.Sending }.forEach { transmit(it) } // 7.6: same client_msg_id
        // a token still waiting for its retry goes now that the server is reachable; one already accepted goes again
        // once a day, so the server keeps it as a device in use, not on every reconnect
        val sentAt = pushTokenSentAt
        if (pushToken != null && (sentAt == null || options.nowMs() - sentAt >= PUSH_TOKEN_REFRESH_MS)) {
            pushJob?.cancel()
            pushAttempt = 0
            pushJob = scope.launch { sendPushToken() }
        }
    }

    // ------------------------------------------------------------------ REST
    private suspend fun api(method: String, path: String, body: JsonObject? = null, retried: Boolean = false): String {
        // before the first connection there is no token yet: a request without one would only be refused
        if (token.isEmpty()) token = options.tokenProvider.token()
        val headers = buildMap {
            put("X-Api-Key", options.apiKey)
            put("Authorization", "Bearer $token")
            put("Accept", "application/json")
        }
        val reply = options.http.request(method, baseUrl + path, headers, body?.toString())
        if (reply.status in 200..299) return reply.body
        val error = runCatching { MarpokatJson.parseToJsonElement(reply.body).jsonObject["error"]?.jsonObject }.getOrNull()
        val code = error?.get("code")?.jsonPrimitive?.contentOrNull ?: "http_${reply.status}"
        if (reply.status == 401 && code == "token_expired" && !retried) {
            token = options.tokenProvider.token()
            return api(method, path, body, retried = true)
        }
        throw MarpokatException(error?.get("message")?.jsonPrimitive?.contentOrNull ?: "HTTP ${reply.status}", code, reply.status)
    }

    private inline fun <reified T> decode(text: String, serializer: kotlinx.serialization.KSerializer<T>): T =
        MarpokatJson.decodeFromString(serializer, text)

    private fun messageOf(text: String): Message = MarpokatJson.decodeFromString(Message.serializer(), text)

    // ------------------------------------------------------------------ channels, history, sync
    private fun putChannel(c: Channel) {
        val old = channels[c.id]
        channels[c.id] = ChannelState(c, old?.cursor ?: 0, old?.loaded ?: false, old?.hasOlder ?: true)
        if (old == null && c.lastMessage != null) storeMessage(c.id, c.lastMessage)
    }

    /** Loads the channel list. Calls that overlap share one request. */
    public suspend fun loadChannels(): Unit = withContext(loop) {
        val running = channelsLoading ?: scope.async { fetchChannels() }.also { job ->
            channelsLoading = job
            job.invokeOnCompletion { scope.launch { if (channelsLoading === job) channelsLoading = null } }
        }
        running.await()
    }

    private suspend fun fetchChannels() {
        var cursor = ""
        val seen = HashSet<String>()
        do {
            val page = MarpokatJson.parseToJsonElement(
                api("GET", "/v1/channels?limit=100" + if (cursor.isNotEmpty()) "&cursor=${enc(cursor)}" else ""),
            ).jsonObject
            for (c in page.getValue("channels").jsonArray) {
                val channel = MarpokatJson.decodeFromJsonElement(Channel.serializer(), c)
                putChannel(channel); seen += channel.id
            }
            cursor = page["next_cursor"]?.takeIf { it !is JsonNull }?.jsonPrimitive?.contentOrNull ?: ""
        } while (cursor.isNotEmpty())
        channels.keys.filter { it !in seen }.forEach { channels.remove(it); messages.remove(it) } // removed while away
        val peers = channels.values.mapNotNull { it.channel.dmPeer?.externalId }.take(100)
        if (peers.isNotEmpty()) { // presence events only tell about changes: ask who is online now
            val users = MarpokatJson.parseToJsonElement(api("GET", "/v1/presence?user_ids=${enc(peers.joinToString(","))}"))
                .jsonObject.getValue("users").jsonArray
            for (u in users) online[u.jsonObject.getValue("user_id").jsonPrimitive.content] = u.jsonObject.getValue("online").jsonPrimitive.boolean
        }
        publish()
        persistChannels()
    }

    /** Makes a channel the open one: loads its latest messages, syncs it and marks it read. */
    public suspend fun openChannel(id: String): Unit = withContext(loop) {
        current = id
        if (id !in channels) loadChannels() // the screen may open a channel before the list arrived
        val state = channels[id] ?: throw MarpokatException("unknown channel", "not_found")
        val page = MarpokatJson.parseToJsonElement(api("GET", "/v1/channels/$id/messages?limit=${options.pageSize}")).jsonObject
        val list = MarpokatJson.decodeFromJsonElement(ListSerializer(Message.serializer()), page.getValue("messages"))
        list.forEach { storeMessage(id, it) }
        val hasMore = page["has_more"]?.jsonPrimitive?.boolean ?: false
        if (!state.loaded) { // the history starts the cursor at the newest revision it holds
            channels[id] = state.copy(cursor = maxOf(state.cursor, list.maxOfOrNull { it.revSeq } ?: 0), loaded = true, hasOlder = hasMore)
        } else {
            sync(id)
        }
        markReadNow(id)
        publish()
        persistMessages(id)
    }

    public suspend fun closeChannel(id: String): Unit = withContext(loop) { if (current == id) current = null }

    /** Loads the page before the oldest message held. Returns whether more remain. */
    public suspend fun loadOlder(id: String): Boolean = withContext(loop) {
        val held = messages[id]
        val state = channels[id]
        if (state == null || held.isNullOrEmpty()) return@withContext false
        val page = MarpokatJson.parseToJsonElement(
            api("GET", "/v1/channels/$id/messages?limit=${options.pageSize}&before_seq=${held.firstKey()}"),
        ).jsonObject
        MarpokatJson.decodeFromJsonElement(ListSerializer(Message.serializer()), page.getValue("messages")).forEach { storeMessage(id, it) }
        val more = page["has_more"]?.jsonPrimitive?.boolean ?: false
        channels[id]?.let { channels[id] = it.copy(hasOlder = more) }
        publish()
        more
    }

    /** Spec section 6: everything that changed after the cursor, until the server has no more. */
    public suspend fun sync(id: String): Unit = withContext(loop) {
        val running = syncing[id] ?: scope.async { runSync(id) }.also { job ->
            syncing[id] = job
            job.invokeOnCompletion { scope.launch { if (syncing[id] === job) syncing.remove(id) } }
        }
        running.await()
    }

    private suspend fun runSync(id: String) {
        val first = channels[id]
        if (first == null || !first.loaded) return // never loaded: opening it loads the history, and the cursor starts there
        while (true) {
            val state = channels[id] ?: return
            val page = MarpokatJson.parseToJsonElement(api("GET", "/v1/channels/$id/sync?since_rev=${state.cursor}&limit=100")).jsonObject
            MarpokatJson.decodeFromJsonElement(ListSerializer(Message.serializer()), page.getValue("messages")).forEach { storeMessage(id, it) }
            val next = page.getValue("next_rev").jsonPrimitive.long
            channels[id] = (channels[id] ?: return).let { it.copy(cursor = maxOf(it.cursor, next)) }
            if (page["has_more"]?.jsonPrimitive?.boolean != true) break
        }
        latest(id)?.let { last -> channels[id]?.let { channels[id] = it.copy(channel = it.channel.copy(lastMessage = last)) } }
        publish()
        persistMessages(id)
    }

    private fun scheduleSync(id: String) {
        syncJobs[id]?.cancel()
        syncJobs[id] = scope.launch {
            delay(100)
            runCatching { sync(id) }
        }
    }

    /** The newest state of a message wins: events can arrive in another order than they happened. */
    private fun storeMessage(channelId: String, m: Message): Boolean {
        val held = messages.getOrPut(channelId) { TreeMap() }
        val cur = held[m.seq]
        if (cur != null && cur.revSeq >= m.revSeq) return false
        held[m.seq] = m
        return true
    }

    private fun latest(channelId: String): Message? = messages[channelId]?.takeIf { it.isNotEmpty() }?.lastEntry()?.value

    /** The gap rule: the next revision moves the cursor, a later one means something was missed (apply it and sync),
     *  an older one was applied already. */
    private fun revision(id: String, rev: Long): Boolean {
        val state = channels[id] ?: return false
        if (rev <= state.cursor) return false
        if (rev == state.cursor + 1) channels[id] = state.copy(cursor = rev) else scheduleSync(id)
        return true
    }

    private fun receive(m: Message, isNew: Boolean = false) {
        if (channels[m.channelId] == null) {
            // a conversation this client does not know yet (someone just started it): load it, then announce
            scope.launch {
                runCatching { loadChannels() }
                if (isNew) announce(m)
            }
            return
        }
        if (!revision(m.channelId, m.revSeq)) return
        // a channel that is not open has no cursor yet, so the same message can pass revision() twice: only the
        // first copy that is stored is announced
        val stored = storeMessage(m.channelId, m)
        if (isNew && stored) announce(m)
        val state = channels.getValue(m.channelId)
        val mine = m.sender?.externalId == me?.externalId
        var channel = state.channel
        latest(m.channelId)?.let { channel = channel.copy(lastMessage = it, lastMessageAt = it.createdAt) }
        if (!m.isSystem && !mine && m.channelId != current && !m.isDeleted && isNew) {
            channel = channel.copy(membership = channel.membership.copy(unreadCount = channel.membership.unreadCount + 1))
        }
        channels[m.channelId] = state.copy(channel = channel)
        if (m.channelId == current && !mine) markReadNow(m.channelId)
        publish()
        scope.launch { persistMessages(m.channelId) }
    }

    /** Tells [incoming] about a live message from someone else. */
    private fun announce(m: Message) {
        if (m.isSystem || m.isDeleted || m.sender?.externalId == me?.externalId) return
        val channel = channels[m.channelId]?.channel ?: return
        _incoming.tryEmit(IncomingMessage(m, channel, isOpen = m.channelId == current))
    }

    // ------------------------------------------------------------------ frames
    private suspend fun onFrame(text: String) {
        val frame = runCatching { MarpokatJson.parseToJsonElement(text).jsonObject }.getOrNull() ?: return
        val type = frame["type"]?.jsonPrimitive?.contentOrNull ?: return
        val data = frame["data"] as? JsonObject ?: JsonObject(emptyMap())
        val id = frame["id"]?.jsonPrimitive?.contentOrNull
        when (type) {
            "auth.ok" -> onAuthOk(data)
            "ack" -> data["message"]?.let {
                val m = MarpokatJson.decodeFromJsonElement(Message.serializer(), it)
                pending.remove(m.clientMsgId)
                receive(m) // our own message: never unread
            }
            "error" -> {
                val cid = id?.takeIf { it.startsWith("m:") }?.substring(2)
                val p = cid?.let { pending[it] }
                if (p != null) { // a message the server refuses will never be accepted: keep it visible, as failed
                    pending[p.clientMsgId] = p.copy(
                        status = PendingStatus.Failed,
                        errorCode = data["code"]?.jsonPrimitive?.contentOrNull, errorMessage = data["message"]?.jsonPrimitive?.contentOrNull,
                    )
                    publish()
                }
            }
            "message.new" -> receive(MarpokatJson.decodeFromJsonElement(Message.serializer(), data.getValue("message")), isNew = true)
            "message.updated", "message.deleted" -> receive(MarpokatJson.decodeFromJsonElement(Message.serializer(), data.getValue("message")))
            "reaction.added", "reaction.removed" -> {
                val channelId = data.getValue("channel_id").jsonPrimitive.content
                if (!revision(channelId, data.getValue("rev_seq").jsonPrimitive.long)) return
                val messageId = data.getValue("message_id").jsonPrimitive.content
                val rev = data.getValue("rev_seq").jsonPrimitive.long
                val held = messages[channelId]
                val msg = held?.values?.firstOrNull { it.id == messageId }
                if (held != null && msg != null && rev > msg.revSeq) {
                    held[msg.seq] = msg.copy(revSeq = rev, reactions = MarpokatJson.decodeFromJsonElement(ReactionSummary.serializer(), data.getValue("reactions")))
                }
                publish()
            }
            "read.updated" -> {
                val channelId = data.getValue("channel_id").jsonPrimitive.content
                if (data.getValue("user_id").jsonPrimitive.content == me?.externalId) {
                    channels[channelId]?.let { s ->
                        val read = maxOf(s.channel.membership.lastReadSeq, data.getValue("last_read_seq").jsonPrimitive.long)
                        channels[channelId] = s.copy(channel = s.channel.copy(membership = s.channel.membership.copy(lastReadSeq = read, unreadCount = unreadOf(s, read))))
                        publish()
                    }
                }
            }
            "typing" -> {
                val channelId = data.getValue("channel_id").jsonPrimitive.content
                val userId = data.getValue("user_id").jsonPrimitive.content
                val users = typing.getOrPut(channelId) { HashMap() }
                users.remove(userId)?.cancel()
                if (data.getValue("typing").jsonPrimitive.boolean) {
                    users[userId] = scope.launch { delay(6000); users.remove(userId); publish() }
                }
                publish()
            }
            "presence" -> {
                online[data.getValue("user_id").jsonPrimitive.content] = data.getValue("online").jsonPrimitive.boolean
                publish()
            }
            "channel.added", "channel.updated" -> {
                putChannel(MarpokatJson.decodeFromJsonElement(Channel.serializer(), data.getValue("channel")))
                publish(); persistChannels()
            }
            "channel.removed" -> {
                val channelId = data.getValue("channel_id").jsonPrimitive.content
                channels.remove(channelId); messages.remove(channelId)
                if (current == channelId) current = null
                publish()
            }
            else -> Unit // member.* and unknown events: ignored, new event types never break old apps
        }
    }

    private fun unreadOf(s: ChannelState, read: Long): Int {
        val held = messages[s.id] ?: return minOf(s.channel.membership.unreadCount, maxOf(0L, s.channel.lastMessageSeq - read).toInt())
        return held.values.count { it.seq > read && !it.isSystem && !it.isDeleted && it.sender?.externalId != me?.externalId }
    }

    // ------------------------------------------------------------------ what a user does
    /** Queues a message. It is sent now or, when offline, after the next connect, with the same `client_msg_id`. */
    public suspend fun send(
        channelId: String,
        text: String = "",
        attachmentIds: List<String> = emptyList(),
        replyToId: String? = null,
    ): String = withContext(loop) {
        val p = PendingMessage(options.newId(), channelId, text, attachmentIds, replyToId, createdAtMs = options.nowMs())
        pending[p.clientMsgId] = p
        transmit(p)
        publish()
        p.clientMsgId
    }

    /** Sends a failed message again. The id is the same, so it cannot be duplicated if the first try did arrive. */
    public suspend fun retry(clientMsgId: String): Unit = withContext(loop) {
        val p = pending[clientMsgId] ?: return@withContext
        val again = p.copy(status = PendingStatus.Sending, errorCode = null, errorMessage = null)
        pending[clientMsgId] = again
        transmit(again)
        publish()
    }

    public suspend fun discard(clientMsgId: String): Unit = withContext(loop) {
        if (pending.remove(clientMsgId) != null) publish()
    }

    private fun transmit(p: PendingMessage) {
        sendFrame("message.send", buildJsonObject {
            put("channel_id", p.channelId); put("client_msg_id", p.clientMsgId); put("text", p.text)
            put("attachment_ids", JsonArray(p.attachmentIds.map(::JsonPrimitive)))
            put("reply_to_id", p.replyToId?.let(::JsonPrimitive) ?: JsonNull)
        }, "m:${p.clientMsgId}")
    }

    /** The three steps of an upload: ask for a link, send the file to the storage (with progress), confirm. */
    public suspend fun upload(
        data: ByteArray,
        mime: String,
        fileName: String? = null,
        onProgress: (UploadProgress) -> Unit = {},
    ): Attachment {
        val created = MarpokatJson.parseToJsonElement(withContext(loop) {
            api("POST", "/v1/attachments", buildJsonObject {
                put("mime", mime); put("size", data.size.toLong()); if (fileName != null) put("file_name", fileName)
            })
        }).jsonObject
        val id = created.getValue("attachment").jsonObject.getValue("id").jsonPrimitive.content
        val headers = created.getValue("upload_headers").jsonObject.mapValues { it.value.jsonPrimitive.content }
        val status = options.http.put(created.getValue("upload_url").jsonPrimitive.content, headers, data) { sent, total ->
            onProgress(UploadProgress(sent, total))
        }
        if (status !in 200..299) throw MarpokatException("the storage refused the upload ($status)", "upload_failed", status)
        return withContext(loop) { MarpokatJson.decodeFromString(Attachment.serializer(), api("POST", "/v1/attachments/$id/complete")) }
    }

    public suspend fun react(messageId: String, emoji: String, on: Boolean = true): Unit = withContext(loop) {
        receive(messageOf(api(if (on) "PUT" else "DELETE", "/v1/messages/$messageId/reactions/${enc(emoji)}")))
    }

    public suspend fun edit(messageId: String, text: String): Unit = withContext(loop) {
        receive(messageOf(api("PATCH", "/v1/messages/$messageId", buildJsonObject { put("text", text) })))
    }

    public suspend fun remove(messageId: String): Unit = withContext(loop) {
        api("DELETE", "/v1/messages/$messageId") // the tombstone arrives as message.deleted
        Unit
    }

    /** Marks everything up to the newest message as read. */
    public suspend fun markRead(channelId: String): Unit = withContext(loop) { markReadNow(channelId) }

    private fun markReadNow(channelId: String) {
        val state = channels[channelId] ?: return
        val top = latest(channelId)?.seq ?: return
        channels[channelId] = state.copy(channel = state.channel.copy(
            membership = state.channel.membership.copy(unreadCount = 0, lastReadSeq = maxOf(state.channel.membership.lastReadSeq, top)),
        ))
        sendFrame("read.mark", buildJsonObject { put("channel_id", channelId); put("seq", top) }, "r:$channelId")
        publish()
    }

    /** Call on every keystroke: it sends at most every 3 seconds (spec 4.2). */
    public suspend fun typingStart(channelId: String): Unit = withContext(loop) {
        if (lastTypingMs?.let { options.nowMs() - it < 3000 } == true) return@withContext
        lastTypingMs = options.nowMs()
        sendFrame("typing.start", buildJsonObject { put("channel_id", channelId) })
    }

    public suspend fun typingStop(channelId: String): Unit = withContext(loop) {
        lastTypingMs = null
        sendFrame("typing.stop", buildJsonObject { put("channel_id", channelId) })
    }

    // ------------------------------------------------------------------ push tokens
    /**
     * Call on login and whenever FCM gives the app a new token; calling it before [connect] is fine. The client keeps
     * the token: it is sent at once (with a token from the provider when there is no connection yet), retried with
     * backoff while the server cannot be reached, and sent again on the first connection of every day, so the
     * server keeps it as a device in use. Returns once the server accepted it or it is kept for later.
     *
     * @throws MarpokatException when the server refuses the token itself (400): it is not kept, sending it again
     * cannot help.
     */
    public suspend fun registerPushToken(pushToken: String): Unit = withContext(loop) {
        pushJob?.cancel()
        this@MarpokatClient.pushToken = pushToken
        pushTokenSentAt = null
        pushAttempt = 0
        sendPushToken(throwRefusal = true)
    }

    /** Call on logout, before [reset], so that the phone stops getting pushes for this user. */
    public suspend fun unregisterPushToken(pushToken: String): Unit = withContext(loop) {
        if (this@MarpokatClient.pushToken == pushToken) {
            pushJob?.cancel(); pushJob = null
            this@MarpokatClient.pushToken = null
            pushTokenSentAt = null
        }
        api("DELETE", "/v1/devices/${enc(pushToken)}")
        Unit
    }

    /** Sends [pushToken] once; when that fails for a reason that can pass, plans the next attempt. Runs on `loop`. */
    private suspend fun sendPushToken(throwRefusal: Boolean = false) {
        val wanted = pushToken ?: return
        try {
            api("POST", "/v1/devices", buildJsonObject { put("platform", "android"); put("push_token", wanted) })
            if (pushToken == wanted) {
                pushTokenSentAt = options.nowMs()
                pushAttempt = 0
            }
        } catch (e: kotlinx.coroutines.CancellationException) {
            throw e
        } catch (e: MarpokatException) {
            if (e.status == 400) { // the token itself is refused
                if (pushToken == wanted) pushToken = null
                if (throwRefusal) throw e
                return
            }
            // overloaded or down: try again soon. Refused credentials (401, 403): kept for the next connection, which
            // gets a token that works, rather than asking again and again with one that does not
            if (e.status == 429 || e.status >= 500) retryPushToken(wanted)
        } catch (e: Exception) { // network, or no token from the provider yet
            retryPushToken(wanted)
        }
    }

    private fun retryPushToken(wanted: String) {
        if (pushToken != wanted) return
        val delayMs = min(MAX_PUSH_RETRY_MS, options.backoffBaseMs * 2.0.pow(pushAttempt)).toLong()
        pushAttempt++
        pushJob = scope.launch {
            delay(delayMs)
            if (pushToken == wanted) sendPushToken()
        }
    }

    // ------------------------------------------------------------------ state and cache
    private fun setStatus(s: ConnectionStatus, detail: String = "") {
        status = s
        statusDetail = detail
        publish()
    }

    private fun publish() {
        val ordered = channels.values.sortedByDescending { it.channel.lastMessageAt ?: it.channel.createdAt }
        _state.value = ChatState(
            status = status,
            statusDetail = statusDetail,
            me = me,
            channels = ordered,
            messages = messages.mapValues { (_, m) -> m.values.toList() },
            pending = pending.values.groupBy { it.channelId }.mapValues { (_, l) -> l.sortedBy { it.createdAtMs } },
            typing = typing.mapValues { (_, m) -> m.keys.toList() }.filterValues { it.isNotEmpty() },
            online = online.toMap(),
        )
    }

    private suspend fun persistChannels() {
        runCatching { options.cache?.saveChannels(channels.values.map { it.channel }) }
    }

    private suspend fun persistMessages(channelId: String) {
        val held = messages[channelId] ?: return
        runCatching { options.cache?.saveMessages(channelId, held.values.toList()) }
    }

    private fun enc(s: String): String = URLEncoder.encode(s, "UTF-8").replace("+", "%20")
}
