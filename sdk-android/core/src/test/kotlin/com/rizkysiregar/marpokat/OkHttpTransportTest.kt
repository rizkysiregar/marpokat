package com.rizkysiregar.marpokat

import com.google.common.truth.Truth.assertThat
import kotlinx.coroutines.runBlocking
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import org.junit.After
import org.junit.Before
import org.junit.Test
import java.util.concurrent.CountDownLatch
import java.util.concurrent.LinkedBlockingQueue
import java.util.concurrent.TimeUnit

class OkHttpTransportTest {
    private lateinit var server: MockWebServer
    private val transport = OkHttpTransport()

    @Before fun start() { server = MockWebServer().also { it.start() } }
    @After fun stop() { server.shutdown() }

    @Test fun sendsMethodHeadersAndBodyAndReadsTheAnswer() = runBlocking {
        server.enqueue(MockResponse().setBody("""{"ok":true}"""))
        val reply = transport.request("POST", server.url("/v1/x").toString(), mapOf("X-Api-Key" to "k", "Authorization" to "Bearer t"), """{"a":1}""")
        val seen = server.takeRequest()
        assertThat(reply.status).isEqualTo(200)
        assertThat(reply.body).isEqualTo("""{"ok":true}""")
        assertThat(seen.method).isEqualTo("POST")
        assertThat(seen.getHeader("X-Api-Key")).isEqualTo("k")
        assertThat(seen.getHeader("Content-Type")).startsWith("application/json")
        assertThat(seen.body.readUtf8()).isEqualTo("""{"a":1}""")
    }

    @Test fun anEmptyPostAndANoContentAnswerWork() = runBlocking {
        server.enqueue(MockResponse().setResponseCode(204))
        val reply = transport.request("POST", server.url("/v1/attachments/a/complete").toString(), emptyMap(), null)
        assertThat(reply.status).isEqualTo(204)
        assertThat(server.takeRequest().bodySize).isEqualTo(0)
    }

    @Test fun errorStatusesAreRepliesNotExceptions() = runBlocking {
        server.enqueue(MockResponse().setResponseCode(401).setBody("""{"error":{"code":"token_expired"}}"""))
        assertThat(transport.request("GET", server.url("/x").toString(), emptyMap(), null).status).isEqualTo(401)
    }

    @Test fun anUnreachableServerIsAMarpokatExceptionWithStatusZero() = runBlocking {
        val url = server.url("/x").toString()
        server.shutdown()
        val e = runCatching { transport.request("GET", url, emptyMap(), null) }.exceptionOrNull() as MarpokatException
        assertThat(e.status).isEqualTo(0)
        assertThat(e.code).isEqualTo("network")
    }

    @Test fun putsTheBytesWithTheGivenContentTypeAndReportsProgress() = runBlocking {
        server.enqueue(MockResponse().setResponseCode(200))
        val progress = mutableListOf<Long>()
        val bytes = ByteArray(300_000) { it.toByte() }
        val status = transport.put(server.url("/put").toString(), mapOf("Content-Type" to "image/png"), bytes) { sent, total ->
            assertThat(total).isEqualTo(bytes.size.toLong()); progress += sent
        }
        val seen = server.takeRequest()
        assertThat(status).isEqualTo(200)
        assertThat(seen.method).isEqualTo("PUT")
        assertThat(seen.getHeader("Content-Type")).isEqualTo("image/png")
        assertThat(seen.bodySize).isEqualTo(bytes.size.toLong())
        assertThat(progress.last()).isEqualTo(bytes.size.toLong())
        assertThat(progress).isInOrder()
    }

    @Test fun carriesTextFramesBothWaysAndReportsTheCloseCode() {
        val received = LinkedBlockingQueue<String>()
        server.enqueue(MockResponse().withWebSocketUpgrade(object : WebSocketListener() {
            override fun onMessage(webSocket: WebSocket, text: String) { webSocket.send("echo:$text") }
            override fun onOpen(webSocket: WebSocket, response: Response) { }
            override fun onClosing(webSocket: WebSocket, code: Int, reason: String) { webSocket.close(code, null) }
        }))
        val opened = CountDownLatch(1)
        val closed = LinkedBlockingQueue<Int>()
        val socket = transport.open(server.url("/v1/ws").toString().replace("http", "ws"), object : SocketListener {
            override fun onOpen() { opened.countDown() }
            override fun onText(text: String) { received += text }
            override fun onClosed(code: Int) { closed += code }
        })
        assertThat(opened.await(5, TimeUnit.SECONDS)).isTrue()
        socket.send("hello")
        assertThat(received.poll(5, TimeUnit.SECONDS)).isEqualTo("echo:hello")
        socket.close(1000)
        assertThat(closed.poll(5, TimeUnit.SECONDS)).isEqualTo(1000)
    }
}
