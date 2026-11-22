package com.rizkysiregar.marpokat

import com.google.common.truth.Truth.assertThat
import kotlinx.coroutines.delay
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import org.junit.Assume.assumeTrue
import org.junit.Test

/**
 * Two clients against a live Marpokat, like two phones. Runs only when the environment names a server:
 * MP_URL, MP_KEY, MP_TOKEN_ALICE, MP_TOKEN_BOB and MP_CHANNEL (a DM of the two); see examples/web-chat/test.mjs.
 */
class LiveServerTest {
    private fun env(name: String): String? = System.getenv(name)?.takeIf { it.isNotBlank() }

    private fun client(token: String) = MarpokatClient(
        MarpokatOptions(baseUrl = env("MP_URL")!!, apiKey = env("MP_KEY")!!, tokenProvider = { token }, backoffBaseMs = 40),
    )

    private suspend fun until(what: String, ms: Long = 15_000, cond: () -> Boolean) {
        try { withTimeout(ms) { while (!cond()) delay(25) } } catch (e: Exception) { throw AssertionError("timed out: $what") }
    }

    @Test fun twoClientsTalkReactEditReplyUploadDeleteAndRecover() = runBlocking {
        assumeTrue("MP_URL not set", env("MP_URL") != null)
        val ch = env("MP_CHANNEL")!!
        val alice = client(env("MP_TOKEN_ALICE")!!)
        val bob = client(env("MP_TOKEN_BOB")!!)
        try {
            alice.connect(); bob.connect()
            until("connected") { alice.state.value.status == ConnectionStatus.Connected && bob.state.value.status == ConnectionStatus.Connected }
            alice.openChannel(ch); bob.openChannel(ch)
            fun view(c: MarpokatClient) = c.state.value.messages[ch].orEmpty()
            fun find(c: MarpokatClient, text: String) = view(c).firstOrNull { it.text == text }

            val id = alice.send(ch, "hello from android")
            until("bob receives") { find(bob, "hello from android") != null }
            until("ack clears pending") { alice.state.value.pending[ch].orEmpty().none { it.clientMsgId == id } && find(alice, "hello from android") != null }
            assertThat(view(alice).count { it.text == "hello from android" }).isEqualTo(1)
            val first = find(alice, "hello from android")!!

            bob.react(first.id, "👍")
            until("reaction") { find(alice, "hello from android")!!.reactions.counts["👍"] == 1 }
            assertThat(find(bob, "hello from android")!!.reactions.mine).containsExactly("👍")
            assertThat(find(alice, "hello from android")!!.reactions.mine).isEmpty()

            alice.edit(first.id, "hello (edited)")
            until("edit") { find(bob, "hello (edited)")?.editedAt != null }

            bob.send(ch, "reply", replyToId = first.id)
            until("reply") { find(alice, "reply")?.replyTo?.id == first.id }

            val png = java.util.Base64.getDecoder().decode("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
            var progress: UploadProgress? = null
            val att = alice.upload(png, "image/png", "dot.png") { progress = it }
            assertThat(att.status).isEqualTo("processing")
            assertThat(progress?.loaded).isEqualTo(png.size.toLong())
            alice.send(ch, "photo", attachmentIds = listOf(att.id))
            until("thumbnail ready for bob") { find(bob, "photo")?.attachments?.firstOrNull()?.isReady == true }
            assertThat(find(bob, "photo")!!.attachments.first().thumbnailUrl).isNotNull()

            alice.remove(first.id)
            until("tombstone") { view(bob).firstOrNull { it.id == first.id }?.isDeleted == true }
            alice.disconnect(); alice.connect()
            until("alice back") { alice.state.value.status == ConnectionStatus.Connected }
            bob.send(ch, "after reconnect")
            until("alice has it") { find(alice, "after reconnect") != null }
            assertThat(view(alice).count { it.text == "after reconnect" }).isEqualTo(1)
        } finally {
            alice.close(); bob.close()
        }
    }
}
