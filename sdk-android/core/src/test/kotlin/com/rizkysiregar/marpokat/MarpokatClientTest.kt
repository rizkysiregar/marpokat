package com.rizkysiregar.marpokat

import com.google.common.truth.Truth.assertThat
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.flow.toList
import kotlinx.coroutines.launch
import kotlinx.coroutines.test.StandardTestDispatcher
import kotlinx.coroutines.test.TestScope
import kotlinx.coroutines.test.advanceTimeBy
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.runTest
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.After
import org.junit.Before
import org.junit.Test
import java.net.URI

private const val CH = "ch-1"
private val me = """{"id":"u-alice","external_id":"alice","name":"Alice","deleted":false}"""
private val bob = """{"id":"u-bob","external_id":"bob","name":"Bob","deleted":false}"""

private fun msg(seq: Int, text: String = "t$seq", rev: Int = seq, sender: String = bob, extra: String = "") =
    """{"id":"m$seq","channel_id":"$CH","seq":$seq,"rev_seq":$rev,"type":"text","sender":$sender,"text":"$text","attachments":[],"reactions":{"counts":{},"mine":[]},"created_at":"2023-11-14T22:13:${(20 + seq).toString().padStart(2, '0')}Z"$extra}"""

private fun channel(unread: Int = 0) =
    """{"id":"$CH","type":"dm","member_count":2,"dm_peer":$bob,"last_seq":3,"last_message_seq":3,"frozen":false,"created_at":"2023-01-01T00:00:00Z","membership":{"role":"member","unread_count":$unread,"last_read_seq":0},"last_message":${msg(3)},"last_message_at":"2023-11-14T22:13:23Z"}"""

private class FakeSocket(val listener: SocketListener) : Socket {
    val sent = mutableListOf<JsonObject>()
    var closed: Int? = null
    override fun send(text: String): Boolean {
        sent += MarpokatJson.parseToJsonElement(text).jsonObject
        return true
    }
    override fun close(code: Int) { closed = code }
    fun frames(type: String) = sent.filter { it["type"]!!.jsonPrimitive.content == type }
    fun push(type: String, data: String, id: String? = null) =
        listener.onText("""{"type":"$type","data":$data${if (id != null) ""","id":"$id"""" else ""}}""")
}

private class Fake : HttpTransport, SocketFactory {
    val sockets = mutableListOf<FakeSocket>()
    val calls = mutableListOf<String>()
    val puts = mutableListOf<String>()
    var history = listOf(msg(1), msg(2), msg(3))
    var changes = listOf<String>()
    var changeRevs = listOf<Int>()
    var expireOnce = false
    var channels = listOf(channel())
    var tokensSeen = mutableListOf<String>()

    override fun open(url: String, listener: SocketListener): Socket =
        FakeSocket(listener).also { sockets += it; listener.onOpen() }

    override suspend fun request(method: String, url: String, headers: Map<String, String>, body: String?): HttpReply {
        val uri = URI(url)
        val key = "$method ${uri.rawPath}"
        calls += key + (uri.rawQuery?.let { "?$it" } ?: "")
        tokensSeen += headers["Authorization"]!!
        if (expireOnce && headers["Authorization"] == "Bearer old") {
            expireOnce = false
            return HttpReply(401, """{"error":{"code":"token_expired","message":"expired"}}""")
        }
        val q = (uri.rawQuery ?: "").split("&").filter { it.contains("=") }.associate { it.substringBefore("=") to it.substringAfter("=") }
        return when {
            key == "GET /v1/channels" -> HttpReply(200, """{"channels":[${channels.joinToString(",")}],"next_cursor":null}""")
            key == "GET /v1/presence" -> HttpReply(200, """{"users":[{"user_id":"bob","online":true}]}""")
            key == "GET /v1/channels/$CH/messages" -> {
                val before = q["before_seq"]?.toInt()
                val page = history.filter { m -> before == null || Regex("\"seq\":(\\d+)").find(m)!!.groupValues[1].toInt() < before }.takeLast(2)
                val first = Regex("\"seq\":(\\d+)").find(page.first())!!.groupValues[1].toInt()
                HttpReply(200, """{"messages":[${page.reversed().joinToString(",")}],"has_more":${first > 1}}""")
            }
            key == "GET /v1/channels/$CH/sync" -> {
                val since = q["since_rev"]!!.toInt()
                val list = changes.filterIndexed { i, _ -> changeRevs[i] > since }
                HttpReply(200, """{"messages":[${list.joinToString(",")}],"next_rev":${changeRevs.filter { it > since }.maxOrNull() ?: since},"has_more":false,"channel_last_seq":9}""")
            }
            key == "POST /v1/attachments" -> HttpReply(201, """{"attachment":{"id":"att1","kind":"image"},"upload_url":"https://files.test/put","upload_headers":{"Content-Type":"image/png"},"expires_at":""}""")
            key == "POST /v1/attachments/att1/complete" -> HttpReply(200, """{"id":"att1","kind":"image","status":"processing"}""")
            key == "POST /v1/devices" || key.startsWith("DELETE /v1/devices/") -> HttpReply(204, "")
            else -> HttpReply(404, """{"error":{"code":"not_found","message":"$key"}}""")
        }
    }

    override suspend fun put(url: String, headers: Map<String, String>, body: ByteArray, onProgress: (Long, Long) -> Unit): Int {
        puts += "$url ${headers["Content-Type"]}"
        onProgress(body.size.toLong(), body.size.toLong())
        return 200
    }
}

@OptIn(ExperimentalCoroutinesApi::class)
class MarpokatClientTest {
    private lateinit var fake: Fake
    private lateinit var client: MarpokatClient
    private var tokens = ArrayDeque<String>()

    @Before fun setUp() { fake = Fake(); tokens = ArrayDeque() }
    @After fun tearDown() { if (::client.isInitialized) client.close() }

    private fun TestScope.newClient(cache: ChatCache? = null) = MarpokatClient(
        MarpokatOptions(
            baseUrl = "https://api.test", apiKey = "mpk_x", tokenProvider = { tokens.removeFirstOrNull() ?: "tok" },
            cache = cache, http = fake, sockets = fake, dispatcher = StandardTestDispatcher(testScheduler),
            backoffBaseMs = 100, random = { 1.0 }, nowMs = { testScheduler.currentTime }, newId = idGenerator(),
        ),
    ).also { client = it }

    // runTest ends by running all virtual time to the end; the ping loop never ends, so the client is closed first
    private fun runClientTest(body: suspend TestScope.() -> Unit) = runTest {
        try { body() } finally { if (::client.isInitialized) client.close() }
    }

    private fun idGenerator(): () -> String { var n = 0; return { "id${++n}" } }

    private suspend fun TestScope.start(cache: ChatCache? = null): FakeSocket {
        newClient(cache)
        client.connect(); runCurrent()
        val ws = fake.sockets.last()
        ws.push("auth.ok", """{"user":$me,"heartbeat_interval_ms":25000}"""); runCurrent()
        return ws
    }

    private val state get() = client.state.value

    @Test fun authenticatesInTheFirstFrameAndLoadsChannelsAndPresence() = runClientTest {
        val ws = start()
        val auth = ws.sent[0]
        assertThat(auth["type"]!!.jsonPrimitive.content).isEqualTo("auth")
        assertThat(auth["data"]!!.jsonObject["api_key"]!!.jsonPrimitive.content).isEqualTo("mpk_x")
        assertThat(auth["data"]!!.jsonObject["protocol_version"]!!.jsonPrimitive.content).isEqualTo("1")
        assertThat(state.status).isEqualTo(ConnectionStatus.Connected)
        assertThat(state.me?.externalId).isEqualTo("alice")
        assertThat(state.channels.map { it.id }).containsExactly(CH)
        assertThat(state.online["bob"]).isTrue()
    }

    @Test fun pingsAtTheIntervalTheServerAsksFor() = runClientTest {
        val ws = start()
        advanceTimeBy(50_100); runCurrent()
        assertThat(ws.frames("ping")).hasSize(2)
    }

    @Test fun reconnectsWithANewTokenAfter4004AndGivesUpOn4003() = runClientTest {
        tokens += listOf("t1", "t2")
        val ws = start()
        ws.listener.onClosed(4004); runCurrent()
        assertThat(fake.sockets).hasSize(2)
        assertThat(fake.sockets[1].sent[0]["data"]!!.jsonObject["token"]!!.jsonPrimitive.content).isEqualTo("t2")
        fake.sockets[1].listener.onClosed(4003); runCurrent()
        advanceTimeBy(60_000); runCurrent()
        assertThat(fake.sockets).hasSize(2)
        assertThat(state.status).isEqualTo(ConnectionStatus.Closed)
    }

    @Test fun backsOffWithJitterAndStartsOverAfterASuccess() = runClientTest {
        val ws = start()
        ws.listener.onClosed(1006); runCurrent()
        advanceTimeBy(99); runCurrent()
        assertThat(fake.sockets).hasSize(1) // random()=1: the first wait is the full 100 ms
        advanceTimeBy(2); runCurrent()
        assertThat(fake.sockets).hasSize(2)
        fake.sockets[1].listener.onClosed(1006); runCurrent()
        advanceTimeBy(150); runCurrent()
        assertThat(fake.sockets).hasSize(2) // the second wait is 200 ms
        advanceTimeBy(100); runCurrent()
        assertThat(fake.sockets).hasSize(3)
        fake.sockets[2].push("auth.ok", """{"user":$me,"heartbeat_interval_ms":25000}"""); runCurrent()
        fake.sockets[2].listener.onClosed(1006); runCurrent()
        advanceTimeBy(101); runCurrent()
        assertThat(fake.sockets).hasSize(4) // back to the first step
    }

    @Test fun asksForANewTokenAndRepeatsARequestThatFailedWithTokenExpired() = runClientTest {
        tokens += listOf("old", "fresh")
        fake.expireOnce = true
        start()
        assertThat(fake.calls.filter { it.startsWith("GET /v1/channels?") }).hasSize(2)
        assertThat(state.channels).hasSize(1)
    }

    @Test fun opensAChannelWithHistoryInOrderAndMarksItRead() = runClientTest {
        val ws = start()
        client.openChannel(CH); runCurrent()
        assertThat(state.messages[CH]!!.map { it.seq }).containsExactly(2L, 3L).inOrder() // the fake serves 2 per page
        val mark = ws.frames("read.mark").single()["data"]!!.jsonObject
        assertThat(mark["seq"]!!.jsonPrimitive.content).isEqualTo("3")
    }

    @Test fun pagesBackWithBeforeSeqAndStopsWhenThereIsNoMore() = runClientTest {
        start(); client.openChannel(CH); runCurrent()
        assertThat(client.loadOlder(CH)).isFalse()
        assertThat(state.messages[CH]!!.map { it.seq }).containsExactly(1L, 2L, 3L).inOrder()
        assertThat(fake.calls).contains("GET /v1/channels/$CH/messages?limit=50&before_seq=2")
    }

    @Test fun appliesTheNextRevisionAtOnceAndIgnoresAStaleOne() = runClientTest {
        val ws = start(); client.openChannel(CH); runCurrent()
        ws.push("message.new", """{"message":${msg(4)}}"""); runCurrent()
        ws.push("message.new", """{"message":${msg(4, "stale")}}"""); runCurrent()
        assertThat(state.messages[CH]!!.last().text).isEqualTo("t4")
        assertThat(fake.calls.filter { it.contains("/sync") }).isEmpty()
    }

    @Test fun syncsWhenARevisionIsSkippedAndEndsWithEverythingOnce() = runClientTest {
        val ws = start(); client.openChannel(CH); runCurrent()
        fake.changes = listOf(msg(4), msg(5), msg(6)); fake.changeRevs = listOf(4, 5, 6)
        ws.push("message.new", """{"message":${msg(6)}}"""); runCurrent() // 4 and 5 were missed
        advanceTimeBy(200); runCurrent()
        assertThat(state.messages[CH]!!.map { it.seq }).containsExactly(2L, 3L, 4L, 5L, 6L).inOrder()
        assertThat(fake.calls.filter { it.contains("/sync") }).hasSize(1)
    }

    @Test fun catchesUpAfterAReconnectFromTheCursor() = runClientTest {
        val ws = start(); client.openChannel(CH); runCurrent()
        ws.listener.onClosed(1006); runCurrent()
        fake.changes = listOf(msg(4), msg(3, "edited", rev = 5)); fake.changeRevs = listOf(4, 5)
        advanceTimeBy(101); runCurrent()
        fake.sockets[1].push("auth.ok", """{"user":$me,"heartbeat_interval_ms":25000}"""); runCurrent()
        assertThat(state.messages[CH]!!.map { it.text }).containsExactly("t2", "edited", "t4").inOrder()
        assertThat(fake.calls.any { it.contains("since_rev=3") }).isTrue()
    }

    @Test fun countsUnreadForOthersInChannelsThatAreNotOpen() = runClientTest {
        val ws = start()
        ws.push("message.new", """{"message":${msg(4)}}"""); runCurrent()
        ws.push("message.new", """{"message":${msg(5, sender = me)}}"""); runCurrent()
        assertThat(state.unreadTotal).isEqualTo(1)
        client.openChannel(CH); runCurrent()
        assertThat(state.unreadTotal).isEqualTo(0)
    }

    @Test fun appliesAReactionEventOnlyIfItIsNewer() = runClientTest {
        val ws = start(); client.openChannel(CH); runCurrent()
        ws.push("reaction.added", """{"channel_id":"$CH","rev_seq":4,"message_id":"m3","user_id":"bob","emoji":"👍","reactions":{"counts":{"👍":1},"mine":[]}}"""); runCurrent()
        assertThat(state.messages[CH]!!.last().reactions.counts).containsExactly("👍", 1)
    }

    @Test fun showsTypingUntilItStopsOrSixSecondsPass() = runClientTest {
        val ws = start()
        ws.push("typing", """{"channel_id":"$CH","user_id":"bob","typing":true}"""); runCurrent()
        assertThat(state.typing[CH]).containsExactly("bob")
        advanceTimeBy(6100); runCurrent()
        assertThat(state.typing[CH]).isNull()
    }

    @Test fun removesAChannelWhenTheServerSaysSo() = runClientTest {
        val ws = start()
        ws.push("channel.removed", """{"channel_id":"$CH"}"""); runCurrent()
        assertThat(state.channels).isEmpty()
    }

    @Test fun keepsAMessagePendingUntilTheAckThenShowsItOnce() = runClientTest {
        val ws = start(); client.openChannel(CH); runCurrent()
        val id = client.send(CH, "hi")
        assertThat(state.pending[CH]).hasSize(1)
        val data = ws.frames("message.send").single()["data"]!!.jsonObject
        assertThat(data["client_msg_id"]!!.jsonPrimitive.content).isEqualTo(id)
        assertThat(data["text"]!!.jsonPrimitive.content).isEqualTo("hi")
        val own = msg(4, "hi", sender = me, extra = ""","client_msg_id":"$id"""")
        ws.push("ack", """{"message":$own}""", "m:$id"); runCurrent()
        ws.push("message.new", """{"message":$own}"""); runCurrent()
        assertThat(state.pending[CH]).isNull()
        assertThat(state.messages[CH]!!.count { it.text == "hi" }).isEqualTo(1)
    }

    @Test fun sendsAMessageWrittenOfflineWhenTheConnectionIsBackWithTheSameId() = runClientTest {
        val ws = start()
        ws.listener.onClosed(1006); runCurrent()
        val id = client.send(CH, "offline")
        assertThat(state.pending[CH]!!.single().status).isEqualTo(PendingStatus.Sending)
        advanceTimeBy(101); runCurrent()
        fake.sockets[1].push("auth.ok", """{"user":$me,"heartbeat_interval_ms":25000}"""); runCurrent()
        val ids = fake.sockets[1].frames("message.send").map { it["data"]!!.jsonObject["client_msg_id"]!!.jsonPrimitive.content }
        assertThat(ids).containsExactly(id)
    }

    @Test fun marksARefusedMessageAsFailedAndRetrySendsTheSameIdAgain() = runClientTest {
        val ws = start()
        val id = client.send(CH, "x")
        ws.push("error", """{"code":"forbidden","message":"channel is frozen"}""", "m:$id"); runCurrent()
        val failed = state.pending[CH]!!.single()
        assertThat(failed.status).isEqualTo(PendingStatus.Failed)
        assertThat(failed.errorCode).isEqualTo("forbidden")
        client.retry(id)
        assertThat(ws.frames("message.send")).hasSize(2)
        assertThat(state.pending[CH]!!.single().status).isEqualTo(PendingStatus.Sending)
        client.discard(id)
        assertThat(state.pending[CH]).isNull()
    }

    @Test fun doesNotSendTypingMoreThanEveryThreeSeconds() = runClientTest {
        val ws = start()
        client.typingStart(CH); client.typingStart(CH)
        assertThat(ws.frames("typing.start")).hasSize(1)
        advanceTimeBy(3100)
        client.typingStart(CH)
        assertThat(ws.frames("typing.start")).hasSize(2)
    }

    @Test fun uploadsInThreeStepsWithProgress() = runClientTest {
        start()
        var last: UploadProgress? = null
        val a = client.upload(ByteArray(10), "image/png", "a.png") { last = it }
        assertThat(a.id).isEqualTo("att1")
        assertThat(fake.calls).containsAtLeast("POST /v1/attachments", "POST /v1/attachments/att1/complete")
        assertThat(fake.puts).containsExactly("https://files.test/put image/png")
        assertThat(last).isEqualTo(UploadProgress(10, 10))
    }

    @Test fun waitsForTheChannelListWhenAChannelIsOpenedBeforeItArrives() = runClientTest {
        newClient(); client.connect(); runCurrent()
        fake.sockets.last().push("auth.ok", """{"user":$me,"heartbeat_interval_ms":25000}""")
        client.openChannel(CH); runCurrent()
        assertThat(state.messages[CH]).hasSize(2)
        assertThat(fake.calls.filter { it.contains("/sync") }).isEmpty()
        assertThat(fake.calls.filter { it.startsWith("GET /v1/channels?") }).hasSize(1)
    }

    @Test fun registersAndRemovesPushTokens() = runClientTest {
        start()
        client.registerPushToken("fcm token/1"); client.unregisterPushToken("fcm token/1")
        assertThat(fake.calls).containsAtLeast("POST /v1/devices", "DELETE /v1/devices/fcm%20token%2F1")
    }

    @Test fun showsCachedChannelsAndMessagesBeforeTheServerAnswers() = runClientTest {
        val cache = object : ChatCache {
            override suspend fun load() = CachedChat(
                listOf(MarpokatJson.decodeFromString(Channel.serializer(), channel())),
                mapOf(CH to listOf(msg(1), msg(2), msg(3)).map { MarpokatJson.decodeFromString(Message.serializer(), it) }),
            )
            override suspend fun saveChannels(channels: List<Channel>) {}
            override suspend fun saveMessages(channelId: String, messages: List<Message>) {}
            override suspend fun clear() {}
        }
        newClient(cache); client.connect(); runCurrent()
        assertThat(state.channels).hasSize(1)
        assertThat(state.messages[CH]).hasSize(3)
        assertThat(state.status).isEqualTo(ConnectionStatus.Connecting)
    }

    private fun TestScope.collectIncoming(): MutableList<IncomingMessage> {
        val got = mutableListOf<IncomingMessage>()
        backgroundScope.launch { client.incoming.toList(got) }
        runCurrent()
        return got
    }

    @Test fun announcesLiveMessagesFromOthersOnceAndNothingElse() = runClientTest {
        val ws = start()
        val got = collectIncoming()
        ws.push("message.new", """{"message":${msg(4)}}"""); runCurrent()
        ws.push("message.new", """{"message":${msg(4)}}"""); runCurrent()                  // the same message again
        ws.push("message.new", """{"message":${msg(5, sender = me)}}"""); runCurrent()      // my own, from another device
        ws.push("message.new", """{"message":${msg(6, extra = ",\"type\":\"system\"").replace("\"type\":\"text\",", "")}}"""); runCurrent()
        ws.push("message.updated", """{"message":${msg(4, "edited", rev = 7)}}"""); runCurrent() // an edit is not new
        assertThat(got.map { it.message.id }).containsExactly("m4")
        assertThat(got.single().channel.id).isEqualTo(CH)
        assertThat(got.single().isOpen).isFalse()
    }

    @Test fun saysWhenTheConversationIsOpenAndSkipsWhatASyncBringsBack() = runClientTest {
        val ws = start(); client.openChannel(CH); runCurrent()
        val got = collectIncoming()
        fake.changes = listOf(msg(4), msg(5), msg(6)); fake.changeRevs = listOf(4, 5, 6)
        ws.push("message.new", """{"message":${msg(6)}}"""); runCurrent() // 4 and 5 were missed: they come by sync
        advanceTimeBy(200); runCurrent()
        assertThat(state.messages[CH]!!.map { it.seq }).containsAtLeast(4L, 5L, 6L)
        assertThat(got.map { it.message.id }).containsExactly("m6")
        assertThat(got.single().isOpen).isTrue()
    }

    @Test fun announcesTheFirstMessageOfAConversationThatWasNotLoadedYet() = runClientTest {
        val ws = start()
        val got = collectIncoming()
        fake.channels = listOf(channel(), channel().replace("\"id\":\"$CH\"", "\"id\":\"ch-2\""))
        ws.push("message.new", """{"message":${msg(1).replace("\"channel_id\":\"$CH\"", "\"channel_id\":\"ch-2\"")}}"""); runCurrent()
        assertThat(got.map { it.channel.id }).containsExactly("ch-2")
        assertThat(state.channel("ch-2")).isNotNull()
    }
}
