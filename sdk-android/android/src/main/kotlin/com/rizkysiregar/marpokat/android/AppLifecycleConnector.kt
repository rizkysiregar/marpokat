package com.rizkysiregar.marpokat.android

import android.os.Handler
import android.os.Looper
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
    /**
     * Returns a function that stops following the app (sign out). The client is also disconnected then.
     *
     * Safe to call from any thread: lifecycle observers can only be added and removed on the main thread, so the
     * work is posted there (in order, so a stop right after attach still stops it).
     */
    public fun attach(client: MarpokatClient, owner: LifecycleOwner? = null): () -> Unit {
        val main = Handler(Looper.getMainLooper())
        var observer: DefaultLifecycleObserver? = null
        var target: LifecycleOwner? = null
        val install = Runnable {
            val lifecycleOwner = owner ?: ProcessLifecycleOwner.get()
            val created = object : DefaultLifecycleObserver {
                override fun onStart(owner: LifecycleOwner) {
                    owner.lifecycleScope.launch { client.connect() }
                }

                override fun onStop(owner: LifecycleOwner) {
                    owner.lifecycleScope.launch { client.disconnect() }
                }
            }
            lifecycleOwner.lifecycle.addObserver(created)
            observer = created
            target = lifecycleOwner
        }
        if (Looper.myLooper() == Looper.getMainLooper()) install.run() else main.post(install)
        return {
            main.post {
                observer?.let { target?.lifecycle?.removeObserver(it) }
                target?.lifecycleScope?.launch { client.disconnect() }
            }
        }
    }
}
