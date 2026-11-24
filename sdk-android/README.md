# Marpokat Android SDK

| Artifact | What |
| -------- | ---- |
| `com.rizkysiregar.marpokat:marpokat-core` | Kotlin/JVM client: WebSocket, reconnect, sync, pending messages, uploads. No Android dependency |
| `com.rizkysiregar.marpokat:marpokat-android` | Room cache, foreground/background connector, FCM payload helper. Depends on core |

```kotlin
val client = MarpokatClient(
    MarpokatOptions(
        baseUrl = "https://api-marpokat.example.com",
        apiKey = "mpk_...",
        // Ask your own backend for a user token; called again when the last one expired.
        tokenProvider = { api.chatToken().token },
        cache = RoomChatCache(context, userId),   // optional: shows the last state at once
    ),
)
AppLifecycleConnector.attach(client)               // connect in the foreground, disconnect in the background

client.state.collect { state -> /* channels, messages, pending, typing, online, unreadTotal */ }
client.openChannel(id); client.send(id, "hi"); client.react(messageId, "👍")
```

While the app is open, `client.incoming` (a `SharedFlow<IncomingMessage>`) gives one event per message from someone
else as it arrives live: not your own, not system messages, not what a sync brings back, never twice. Use it for an
in-app banner; `isOpen` says the conversation is on screen already. When the app is closed the server sends a push
instead (below).

```kotlin
client.incoming.collect { e -> if (!e.isOpen) showBanner(e.channel, e.message) }
```

Push (FCM): register the token with `client.registerPushToken(token)` on login and in `onNewToken`, remove it on
logout with `unregisterPushToken`. In `onMessageReceived` use `MarpokatPush.parse(message.data)` to get a typed
`PushMessage` (`null` for pushes that are not Marpokat's); `push.channelId` is what the notification opens.

The client keeps the rules of `spec/events.md`: auth in the first frame, ping, `rev_seq` cursor with gap detection,
reconnect with full jitter followed by sync, idempotent sends (a message written offline goes out once, with the same
`client_msg_id`).

```bash
export JAVA_HOME=<JDK 21>
./gradlew :core:test :android:testDebugUnitTest :android:assembleRelease
# against a live server (see examples/web-chat/test.mjs for how to get the tokens)
MP_URL=... MP_KEY=... MP_TOKEN_ALICE=... MP_TOKEN_BOB=... MP_CHANNEL=... ./gradlew :core:test --tests '*LiveServerTest*'
```

## Publishing (GitHub Packages, Maven)

```bash
./gradlew publishAllPublicationsToGitHubPackagesRepository -Pgpr.user=rizkysiregar -Pgpr.token=<PAT with write:packages>
```

Apps read it with a Maven repository `https://maven.pkg.github.com/rizkysiregar/marpokat` and credentials
(`read:packages`). The version is `version` in the root `build.gradle.kts`.
