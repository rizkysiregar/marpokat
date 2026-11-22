package com.rizkysiregar.marpokat

import kotlinx.coroutines.suspendCancellableCoroutine
import okhttp3.Call
import okhttp3.Callback
import okhttp3.MediaType.Companion.toMediaTypeOrNull
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import okio.Buffer
import okio.BufferedSink
import okio.ForwardingSink
import okio.buffer
import java.io.IOException
import java.util.concurrent.TimeUnit
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException

public class HttpReply(public val status: Int, public val body: String)

/** What the client needs from HTTP. The default is OkHttp; tests give a fake. */
public interface HttpTransport {
    public suspend fun request(method: String, url: String, headers: Map<String, String>, body: String?): HttpReply

    /** Sends the bytes of a file with a PUT, reporting how much went out. */
    public suspend fun put(
        url: String,
        headers: Map<String, String>,
        body: ByteArray,
        onProgress: (Long, Long) -> Unit,
    ): Int
}

public interface SocketListener {
    public fun onOpen()
    public fun onText(text: String)
    public fun onClosed(code: Int)
}

public interface Socket {
    public fun send(text: String): Boolean
    public fun close(code: Int)
}

/** What the client needs from WebSocket. */
public fun interface SocketFactory {
    public fun open(url: String, listener: SocketListener): Socket
}

public class OkHttpTransport(
    private val client: OkHttpClient = OkHttpClient.Builder()
        .connectTimeout(10, TimeUnit.SECONDS)
        .readTimeout(30, TimeUnit.SECONDS)
        .pingInterval(0, TimeUnit.SECONDS) // the protocol has its own ping frame
        .build(),
) : HttpTransport, SocketFactory {

    override suspend fun request(method: String, url: String, headers: Map<String, String>, body: String?): HttpReply {
        val payload: RequestBody? = when {
            body != null -> body.toRequestBody(JSON)
            method == "POST" || method == "PUT" || method == "PATCH" -> ByteArray(0).toRequestBody(null)
            else -> null
        }
        val request = Request.Builder().url(url).method(method, payload).apply { headers.forEach { (k, v) -> header(k, v) } }.build()
        return client.newCall(request).await { HttpReply(it.code, it.body.string()) }
    }

    override suspend fun put(url: String, headers: Map<String, String>, body: ByteArray, onProgress: (Long, Long) -> Unit): Int {
        val mime = headers.entries.firstOrNull { it.key.equals("Content-Type", true) }?.value?.toMediaTypeOrNull()
        val raw = body.toRequestBody(mime)
        val counting = object : RequestBody() {
            override fun contentType() = raw.contentType()
            override fun contentLength() = raw.contentLength()
            override fun writeTo(sink: BufferedSink) {
                var written = 0L
                val forwarding = object : ForwardingSink(sink) {
                    override fun write(source: Buffer, byteCount: Long) {
                        super.write(source, byteCount)
                        written += byteCount
                        onProgress(written, contentLength())
                    }
                }.buffer()
                raw.writeTo(forwarding)
                forwarding.flush()
            }
        }
        val request = Request.Builder().url(url).put(counting).apply {
            headers.filterKeys { !it.equals("Content-Type", true) }.forEach { (k, v) -> header(k, v) }
        }.build()
        return client.newCall(request).await { it.code }
    }

    override fun open(url: String, listener: SocketListener): Socket {
        val ws = client.newWebSocket(Request.Builder().url(url).build(), object : WebSocketListener() {
            override fun onOpen(webSocket: WebSocket, response: Response) = listener.onOpen()
            override fun onMessage(webSocket: WebSocket, text: String) = listener.onText(text)
            override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
                webSocket.close(code, null)
            }
            override fun onClosed(webSocket: WebSocket, code: Int, reason: String) = listener.onClosed(code)
            override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) = listener.onClosed(1006)
        })
        return object : Socket {
            override fun send(text: String) = ws.send(text)
            override fun close(code: Int) {
                ws.close(code, null)
            }
        }
    }

    private companion object {
        val JSON = "application/json".toMediaTypeOrNull()
    }
}

private suspend fun <T> Call.await(read: (Response) -> T): T = suspendCancellableCoroutine { cont ->
    cont.invokeOnCancellation { cancel() }
    enqueue(object : Callback {
        override fun onFailure(call: Call, e: IOException) {
            cont.resumeWithException(MarpokatException(e.message ?: "network error", "network", 0, e))
        }

        override fun onResponse(call: Call, response: Response) {
            response.use {
                try {
                    cont.resume(read(it))
                } catch (e: IOException) {
                    cont.resumeWithException(MarpokatException(e.message ?: "network error", "network", 0, e))
                }
            }
        }
    })
}
