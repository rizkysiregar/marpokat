package com.rizkysiregar.marpokat.android

import android.content.Context
import androidx.room.ColumnInfo
import androidx.room.Dao
import androidx.room.Database
import androidx.room.Entity
import androidx.room.PrimaryKey
import androidx.room.Query
import androidx.room.Room
import androidx.room.RoomDatabase
import androidx.room.Transaction
import androidx.room.Upsert
import com.rizkysiregar.marpokat.CachedChat
import com.rizkysiregar.marpokat.Channel
import com.rizkysiregar.marpokat.ChatCache
import com.rizkysiregar.marpokat.MarpokatJson
import com.rizkysiregar.marpokat.Message
import kotlinx.serialization.builtins.ListSerializer

private const val KEEP_PER_CHANNEL = 50

@Entity(tableName = "channels")
internal data class ChannelRow(@PrimaryKey val id: String, @ColumnInfo(name = "json") val json: String)

@Entity(tableName = "messages")
internal data class MessagesRow(@PrimaryKey @ColumnInfo(name = "channel_id") val channelId: String, @ColumnInfo(name = "json") val json: String)

@Dao
internal interface CacheDao {
    @Query("SELECT * FROM channels") suspend fun channels(): List<ChannelRow>
    @Query("SELECT * FROM messages") suspend fun messages(): List<MessagesRow>
    @Query("DELETE FROM channels") suspend fun clearChannels()
    @Query("DELETE FROM messages") suspend fun clearMessages()
    @Upsert suspend fun putChannels(rows: List<ChannelRow>)
    @Upsert suspend fun putMessages(row: MessagesRow)

    @Transaction
    suspend fun replaceChannels(rows: List<ChannelRow>) {
        clearChannels()
        putChannels(rows)
    }

    @Transaction
    suspend fun clearAll() {
        clearChannels()
        clearMessages()
    }
}

@Database(entities = [ChannelRow::class, MessagesRow::class], version = 1, exportSchema = false)
internal abstract class CacheDatabase : RoomDatabase() {
    abstract fun dao(): CacheDao
}

/**
 * Keeps the channel list and the latest 50 messages of each channel in a Room database, so that the screen shows
 * something at once after an app start and syncs afterwards. One database per [name] (use the user id): another account
 * never sees these rows. Rows hold the server's JSON, so a new field of the API needs no migration.
 */
public class RoomChatCache(context: Context, name: String) : ChatCache {
    private val dao = Room.databaseBuilder(context.applicationContext, CacheDatabase::class.java, "marpokat-$name.db")
        .fallbackToDestructiveMigration(true) // a cache: when the schema changes it is simply filled again
        .build()
        .dao()

    private val channelList = ListSerializer(Channel.serializer())
    private val messageList = ListSerializer(Message.serializer())

    override suspend fun load(): CachedChat? {
        val channels = dao.channels().mapNotNull { runCatching { MarpokatJson.decodeFromString(Channel.serializer(), it.json) }.getOrNull() }
        if (channels.isEmpty()) return null
        val messages = dao.messages().associate { row ->
            row.channelId to runCatching { MarpokatJson.decodeFromString(messageList, row.json) }.getOrDefault(emptyList())
        }
        return CachedChat(channels, messages)
    }

    override suspend fun saveChannels(channels: List<Channel>) {
        dao.replaceChannels(channels.map { ChannelRow(it.id, MarpokatJson.encodeToString(Channel.serializer(), it)) })
    }

    override suspend fun saveMessages(channelId: String, messages: List<Message>) {
        dao.putMessages(MessagesRow(channelId, MarpokatJson.encodeToString(messageList, messages.takeLast(KEEP_PER_CHANNEL))))
    }

    override suspend fun clear() {
        dao.clearAll()
    }
}
