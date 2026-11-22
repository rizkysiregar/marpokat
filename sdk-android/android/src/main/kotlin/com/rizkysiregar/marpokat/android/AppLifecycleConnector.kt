package com.rizkysiregar.marpokat.android

import androidx.lifecycle.DefaultLifecycleObserver
import androidx.lifecycle.LifecycleOwner
import androidx.lifecycle.ProcessLifecycleOwner
import androidx.lifecycle.lifecycleScope
import com.rizkysiregar.marpokat.MarpokatClient
import kotlinx.coroutines.launch

/**
 * Keeps the connection open while the app is in the foreground and closes it when it goes to the background: a phone
 * that keeps a socket open all day drains its battery, and the server sends a push to a user without a connection.
 * Coming back to the foreground connects again and syncs what was missed.
 *
 * ```
 * AppLifecycleConnector.attach(client)   // in Application.onCreate, once the user is signed in
 * ```
 */
public object AppLifecycleConnector {
    /** Returns a function that stops following the app (sign out). The client is also disconnected then. */
    public fun attach(client: MarpokatClient, owner: LifecycleOwner = ProcessLifecycleOwner.get()): () -> Unit {
        val observer = object : DefaultLifecycleObserver {
            override fun onStart(owner: LifecycleOwner) {
                owner.lifecycleScope.launch { client.connect() }
            }

            override fun onStop(owner: LifecycleOwner) {
                owner.lifecycleScope.launch { client.disconnect() }
            }
        }
        owner.lifecycle.addObserver(observer)
        return {
            owner.lifecycle.removeObserver(observer)
            owner.lifecycleScope.launch { client.disconnect() }
        }
    }
}
