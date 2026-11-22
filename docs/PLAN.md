# MARPOKAT: Development Plan

Marpokat adalah **chat as a service**: satu platform chat multi-tenant (API + realtime gateway + SDK) yang bisa dipakai
aplikasi mana pun yang butuh fitur chat. Tenant pertama adalah **Narrativ** (Android + web), tapi Narrativ memakai API publik
yang sama dengan tenant lain nanti, tanpa jalan pintas khusus.

Dokumen ini acuan utama: keputusan arsitektur, tech stack, desain data dan protokol, pekerjaan infra, integrasi Narrativ,
serta urutan milestone beserta progress tiap item.

- **Status:** M0 selesai (dev live di `api-dev-marpokat.rizkysiregar.my.id`). M1 berjalan: sisi Marpokat selesai, berikutnya tenant dev dan integrasi Narrativ
- **Dibuat:** 2026-11-22
- **Target MVP:** chat service yang robust untuk 100–1000 user aktif. DM, group, media, reaction. Belum untuk dijual
- **Repo terkait:** `marpokat` (monorepo ini), `narrativ-backend`, `narrativ-android`, `narrativ-web-client`, `homelabs-gitops`
- **Lokasi dokumen:** `marpokat/docs/PLAN.md`

**Cara update progress:** centang `[x]` saat item selesai dan isi kolom _Ref_ (atau tulis di akhir baris) dengan nomor PR
atau commit. Status milestone di tabel ringkasan ikut diperbarui. Item yang batal dicoret (`~~...~~`) dan diberi alasan,
tidak dihapus. Keputusan baru ditambahkan ke bagian 2 dengan nomor berikutnya, dan perubahannya dicatat di bagian 15.

---

## 1. Ringkasan progress

| Milestone | Isi                                                        | Status   |
| --------- | ---------------------------------------------------------- | -------- |
| M0        | Fondasi: repo, spec, skema, skeleton Go, CI/CD, deploy dev | ✅ Selesai |
| M1        | Tenant dan autentikasi                                     | 🟡 Berjalan |
| M2        | Messaging inti (DM, group, history, sync, outbox)          | ⚪ Belum |
| M3        | Realtime gateway (WebSocket, fan-out, typing, read)        | ⚪ Belum |
| M4        | Worker, push notification, retry dan DLQ                   | ⚪ Belum |
| M5        | Media dan reaction, edit/hapus pesan                       | ⚪ Belum |
| M6        | SDK JS + chat di Narrativ web                              | ⚪ Belum |
| M7        | SDK Android + chat di Narrativ Android                     | ⚪ Belum |
| M8        | Hardening dan launch prod                                  | ⚪ Belum |

Legenda: ⚪ belum, 🟡 berjalan, ✅ selesai, ⛔ terblokir

---

## 2. Keputusan arsitektur

| #   | Keputusan                                                                                                                                         | Alasan                                                                                                                                                                         |
| --- | ------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| D1  | **Multi-tenant sejak hari pertama.** Setiap data punya `tenant_id`, Narrativ adalah tenant biasa                                                  | Tujuan produk adalah SaaS. Memasang multi-tenancy belakangan jauh lebih mahal daripada sejak awal                                                                              |
| D2  | **Backend Go**, satu binary dengan subcommand `api`, `worker`, `migrate` (modular monolith)                                                       | Koneksi WebSocket ringan, image kecil, sudah dipakai di `narrativ-media-worker`. Satu codebase, deploy tetap sederhana, bisa dipecah nanti                                     |
| D3  | **PostgreSQL sebagai sumber kebenaran.** Bukan MongoDB                                                                                            | Data chat relasional (member, channel, unread). Butuh transaksi ACID untuk seq + pesan + outbox. Postgres sudah ada di infra VM                                                |
| D4  | **Redis Pub/Sub untuk fan-out realtime** antar pod gateway (best-effort)                                                                          | Latensi milidetik. Event yang hilang ditutup oleh mekanisme sync (D8), jadi tidak perlu durable                                                                                |
| D5  | **RabbitMQ untuk job durable** (push, webhook, media) dengan ack manual, retry bertingkat lewat DLX + TTL, dan DLQ. Bukan Kafka                   | Skala 100–1000 user tidak butuh throughput atau replay log Kafka. RabbitMQ sudah ada dan dikenal                                                                               |
| D6  | **Transactional outbox**: event ditulis ke tabel `outbox` dalam transaksi yang sama dengan pesan, relay mem-publish ke RabbitMQ                   | Tidak ada event yang hilang walau broker atau pod mati. Broker bisa ditukar (Kafka/NATS) tanpa mengubah logika inti                                                            |
| D7  | Relay outbox memakai **polling `FOR UPDATE SKIP LOCKED`** (±200ms), bukan `LISTEN/NOTIFY`                                                          | `LISTEN/NOTIFY` tidak bisa lewat pgbouncer mode transaction. `SKIP LOCKED` aman untuk banyak replika worker                                                                    |
| D8  | **Satu counter per channel (`channels.last_seq`)**: pesan mendapat `seq` (urutan, immutable) dan `rev_seq` (nomor perubahan terakhir)            | Sync cukup satu query `rev_seq > cursor` dan mencakup pesan baru, edit, hapus, serta reaction. Client mendeteksi celah dari `rev_seq` yang lompat                              |
| D9  | **Idempotensi kirim pesan** lewat `client_msg_id` (UNIQUE per channel + sender)                                                                   | Retry dari client tidak membuat pesan dobel                                                                                                                                    |
| D10 | **Auth client: JWT ditandatangani `api_secret` tenant** (`sub` = external user id). Marpokat tidak mengurus login/password                        | Pola standar chat SaaS (Stream, Sendbird). Tenant menjamin identitas user, Marpokat hanya memverifikasi                                                                        |
| D11 | **Aturan bisnis "siapa boleh chat siapa" ada di tenant.** Config tenant `client_can_create_dm` / `client_can_create_group`; Narrativ: DM `false` | Marpokat tetap generik. Narrativ membuat DM lewat server API setelah mengecek blokir/privasi                                                                                   |
| D12 | **Media lewat presigned URL MinIO** (upload dan download langsung, tidak lewat server)                                                            | Server tidak menanggung bandwidth file. Pola yang sama dengan Narrativ                                                                                                         |
| D13 | **Push token didaftarkan SDK langsung ke Marpokat** (`POST /devices`), push dikirim memakai kredensial FCM milik tenant (tersimpan terenkripsi)   | Tiap tenant punya project Firebase sendiri. Terpisah dari push Narrativ yang sudah ada                                                                                         |
| D14 | **Monorepo** `marpokat`: `server/`, `spec/`, `sdk-js/`, `sdk-android/`, `examples/`, `docs/`                                                      | Perubahan protokol, server, dan SDK sering harus terjadi bersamaan                                                                                                             |
| D15 | **Kontrak ditulis dulu** (`spec/openapi.yaml` + `spec/events.md`), kode menyusul                                                                  | Kontrak adalah produk yang sebenarnya. SDK dan tenant lain bergantung padanya                                                                                                  |
| D16 | **Protokol berversi sejak awal**: path `/v1`, field `protocol_version` di pesan `auth` WebSocket                                                  | Begitu ada client terpasang di HP user, breaking change jadi mahal                                                                                                             |
| D17 | **Backend dulu sampai M5, baru SDK.** SDK JS boleh mulai paralel sejak M3 sebagai alat uji gateway                                                | Protokol stabil dulu sebelum dibungkus SDK                                                                                                                                     |
| D18 | **Tanpa UI kit di MVP.** Layar chat dibuat di Narrativ memakai SDK                                                                                | Komponen yang terbukti bagus diangkat jadi UI kit nanti                                                                                                                        |
| D19 | **Tanpa developer console web di MVP.** Tenant dikelola lewat CLI `marpokat tenant ...`                                                           | Hanya ada satu tenant. Console baru berguna saat ada tenant eksternal                                                                                                          |
| D20 | **Redis: pakai container prod yang ada (port 6380) dengan prefix `marpokat:`** untuk key dan channel Pub/Sub                                       | Tidak menambah container. Pub/Sub Redis tidak terpisah per DB index, jadi prefix wajib. Pindah ke container sendiri kalau beban/RAM menuntut                                    |
| D21 | Host: `api-marpokat.rizkysiregar.my.id` (prod), `api-dev-marpokat.rizkysiregar.my.id` (dev). App gitops `marpokat` / `marpokat-dev`             | Ikut pola `api-narrativ` / `api-dev-narrativ` dan `narrativ` / `narrativ-dev`                                                                                                  |
| D22 | **Jeda shutdown di aplikasi** (`SHUTDOWN_DELAY`), bukan `preStop: sleep`: saat SIGTERM `/readyz` langsung gagal, server tetap melayani selama jeda, lalu berhenti | Image distroless tidak punya shell atau `sleep`. Hasilnya sama: ingress berhenti merutekan sebelum koneksi ditutup                                                             |
| D23 | **Dua port**: publik `8080` (REST + WS, lewat ingress) dan management `9090` (`/healthz`, `/readyz`, `/metrics`, tidak diekspos)                 | Metrics dan detail dependency tidak terbuka ke internet. `/healthz` publik tetap ada untuk cek tunnel                                                                          |
| D24 | **Readiness memeriksa dependency** (Postgres, Redis, RabbitMQ untuk worker), liveness tidak                                                       | Infra VM bermasalah membuat pod tidak menerima traffic, bukan restart berulang                                                                                                 |
| D25 | **Migrasi goose tanpa lock**, dijalankan sebagai Job PreSync Argo CD                                                                               | Locker goose memakai session advisory lock yang tidak bisa lewat pgbouncer mode transaction. Hanya satu Job yang jalan                                                        |
| D26 | **Spec dalam bahasa Inggris** (`openapi.yaml`, `events.md`); PLAN tetap bahasa Indonesia                                                           | Spec adalah dokumen untuk developer tenant, termasuk pihak luar nanti                                                                                                          |
| D27 | **Kolom `jsonb` dipetakan ke `string` di sqlc**, bukan `[]byte`                                                                                   | Dengan simple protocol (D7, pgbouncer) pgx mengirim `[]byte` sebagai `bytea` dan `jsonb` menolaknya. Ditemukan oleh integration test M1                                        |
| D28 | **User harus sudah di-upsert sebelum token-nya dipakai.** Token valid untuk user yang tidak ada dijawab `401`. Tenant meng-upsert user tepat sebelum menerbitkan token | Marpokat tidak membuat user sendiri, jadi data profil selalu datang dari tenant. Narrativ melakukannya di endpoint token (NV-03)                                               |
| D29 | **Rotasi secret langsung berlaku** (tanpa masa tenggang), cache auth 30 detik                                                                     | Sederhana untuk satu tenant. Kalau nanti ada tenant eksternal, tambahkan secret sebelumnya dengan masa berlaku                                                                 |
| D30 | **Format kredensial:** api key `mpk_` + 32 hex, api secret `mps_` + 43 karakter base64url (256 bit), webhook secret `mpw_`. Secret dipakai apa adanya sebagai kunci HMAC | Prefix membuat kredensial mudah dikenali (dan mudah dideteksi secret scanner). Tenant cukup menempelkan string secret ke library JWT mereka                                    |

---

## 3. Tech stack

### Server (Go)

| Kebutuhan         | Pilihan                                                                                         |
| ----------------- | ----------------------------------------------------------------------------------------------- |
| Bahasa            | Go 1.26 (sama dengan `narrativ-media-worker`)                                                   |
| HTTP              | `net/http` (routing bawaan Go 1.22+), middleware sendiri                                        |
| WebSocket         | `github.com/coder/websocket`                                                                    |
| Postgres          | `pgx/v5` + `sqlc` (SQL ditulis tangan, kode Go type-safe digenerate)                            |
| Migrasi           | `goose` (dijalankan oleh subcommand `migrate`, sebagai Argo PreSync Job)                        |
| Redis             | `go-redis/v9`                                                                                   |
| RabbitMQ          | `amqp091-go`                                                                                    |
| Object storage    | `minio-go/v7`                                                                                   |
| Push              | `firebase.google.com/go/v4` (FCM, Android + Web Push)                                           |
| JWT               | `golang-jwt/jwt/v5`                                                                             |
| ID                | UUIDv7 (`google/uuid`), urut berdasarkan waktu                                                  |
| Config            | env var + `caarlos0/env`                                                                        |
| Logging           | `log/slog` JSON                                                                                 |
| Metrics / tracing | `prometheus/client_golang`, OpenTelemetry → Zipkin infra VM                                     |
| Testing           | `testing` + `testcontainers-go` (Postgres, Redis, RabbitMQ asli), `k6` untuk load test WS       |
| Kualitas          | `golangci-lint`, `go vet`, `sqlc vet`                                                           |

⚠️ pgbouncer berjalan di mode transaction: pgx memakai `default_query_exec_mode=simple_protocol` (atau `cache_describe`)
supaya prepared statement tidak bentrok antar koneksi.

### SDK

| SDK                | Pilihan                                                                                                        |
| ------------------ | -------------------------------------------------------------------------------------------------------------- |
| `@marpokat/core`   | TypeScript strict, tanpa dependency framework. WebSocket native, fetch, IndexedDB (cache, akhir MVP)            |
| `@marpokat/react`  | Hooks di atas core (`useChannels`, `useMessages`, `useSendMessage`). Cocok dengan Zustand + React Query        |
| `marpokat-android` | Kotlin, OkHttp WebSocket, Coroutines + `StateFlow`, kotlinx.serialization, Room (cache + antrean kirim)        |
| Distribusi         | GitHub Packages (npm + Maven), private. Group Maven `com.rizkysiregar.marpokat`                                 |

### Infra

| Komponen          | Pilihan                                                                     |
| ----------------- | --------------------------------------------------------------------------- |
| Database          | PostgreSQL infra VM lewat pgbouncer (6432)                                  |
| Cache / Pub/Sub   | Redis infra VM (prefix `marpokat:`, D20)                                    |
| Broker            | RabbitMQ infra VM, vhost `/marpokat` dan `/marpokat-dev`                    |
| Object storage    | MinIO infra VM, bucket `marpokat-media` dan `marpokat-media-dev`            |
| Runtime           | Cluster k8s homelab, ingress-nginx, Cloudflare Tunnel                       |
| CI/CD             | Jenkins → GHCR (`ghcr.io/rizkysiregar/marpokat-server`) → Argo CD (gitops)  |
| Observability     | Prometheus (scrape annotation), Zipkin, log JSON                            |

---

## 4. Arsitektur

```
 Android SDK ─┐                                   Tenant backend (mis. Narrativ, Spring)
 Web SDK  ────┤ wss://api-marpokat.../v1/ws         │ server API (api_secret)
              │ https://api-marpokat.../v1/*        │ - upsert user, buat DM/group
              ▼                                     ▼ - generate user token
   Cloudflare Tunnel → ingress-nginx → ┌────────────────────────────┐
                                       │ marpokat api (Go) x N pod  │
                                       │ REST + WebSocket gateway   │
                                       └───┬─────────────┬──────────┘
                    1 transaksi:           │             │ publish event
                    seq + message + outbox │             ▼
                                       ┌───▼──────┐  ┌────────────────┐
                                       │ Postgres │  │ Redis          │──► semua pod gateway
                                       │ (outbox) │  │ pub/sub, pres. │    → client online
                                       └───┬──────┘  │ rate limit     │
                       poll SKIP LOCKED    │         └────────────────┘
                                       ┌───▼────────────────────────────┐
                                       │ marpokat worker (Go)           │
                                       │ - outbox relay → RabbitMQ      │
                                       │ - push consumer → FCM          │
                                       │ - webhook consumer → tenant    │
                                       │ - media consumer → thumbnail   │
                                       └───┬────────────────────────────┘
                                           ▼
                                RabbitMQ (/marpokat) + DLQ      MinIO (marpokat-media)
```

---

## 5. Struktur repo

```
marpokat/
├── server/
│   ├── cmd/marpokat/          # main: subcommand api | worker | migrate | tenant
│   ├── internal/
│   │   ├── tenant/            # app, api key, config, kredensial FCM (terenkripsi)
│   │   ├── auth/              # verifikasi JWT user & server
│   │   ├── user/
│   │   ├── channel/           # DM, group, member, role
│   │   ├── message/           # kirim, edit, hapus, history, sync
│   │   ├── reaction/
│   │   ├── media/             # presigned upload/download, validasi
│   │   ├── device/            # push token
│   │   ├── realtime/          # WS hub, session, Redis fan-out, presence, typing
│   │   ├── outbox/            # writer + relay
│   │   ├── push/              # consumer FCM
│   │   ├── webhook/           # consumer + signature HMAC
│   │   └── platform/          # db, redis, amqp, minio, config, log, metrics, http middleware
│   ├── migrations/            # SQL goose
│   ├── queries/               # SQL untuk sqlc
│   ├── Dockerfile
│   └── Jenkinsfile
├── spec/
│   ├── openapi.yaml           # kontrak REST
│   └── events.md              # kontrak event WebSocket (+ JSON Schema)
├── sdk-js/                    # packages/core, packages/react
├── sdk-android/               # library Kotlin
├── examples/                  # sample app minimal (web)
└── docs/
    └── PLAN.md
```

---

## 6. Data model

```sql
tenants          (id, name, api_key UNIQUE, api_secret_enc, config jsonb, fcm_credentials_enc,
                  webhook_url, webhook_secret_enc, created_at, disabled_at)
users            (tenant_id, id uuid, external_id, name, avatar_url, metadata jsonb, created_at, updated_at, deleted_at,
                  UNIQUE (tenant_id, external_id))
channels         (tenant_id, id uuid, type 'dm'|'group', name, avatar_url, dm_key UNIQUE NULL,
                  last_seq bigint, last_message_seq bigint, last_message_at, created_by, created_at, frozen_at)
channel_members  (channel_id, user_id, tenant_id, role 'owner'|'admin'|'member', last_read_seq bigint,
                  muted_until, joined_at, left_at, PRIMARY KEY (channel_id, user_id))
messages         (channel_id, seq bigint, id uuidv7 UNIQUE, tenant_id, sender_id, client_msg_id,
                  type 'text'|'media'|'system', text, attachments jsonb, reply_to_id,
                  reaction_counts jsonb, rev_seq bigint, edited_at, deleted_at, created_at,
                  PRIMARY KEY (channel_id, seq), UNIQUE (channel_id, sender_id, client_msg_id),
                  INDEX (channel_id, rev_seq))
message_reactions(message_id, user_id, emoji, created_at, PRIMARY KEY (message_id, user_id, emoji))
attachments      (id, tenant_id, uploader_id, object_key, mime, size, width, height, duration_ms,
                  thumb_key, blurhash, status 'pending'|'ready'|'failed', created_at)
devices          (tenant_id, user_id, platform 'android'|'web', push_token UNIQUE, updated_at)
outbox           (id bigserial, tenant_id, topic, event_id uuid, payload jsonb, created_at, published_at, attempts)
```

Aturan desain:

- **`dm_key`** = `{tenant_id}:{min(userA,userB)}:{max(userA,userB)}` dengan UNIQUE: dua user dijamin hanya punya satu DM.
- **`last_seq`** dinaikkan untuk setiap event yang mengubah channel (pesan, edit, hapus, reaction). `seq` pesan baru = nilai
  baru itu; `rev_seq` pesan yang diubah = nilai baru itu (D8).
- **`last_message_seq`** hanya naik saat ada pesan baru. Unread = jumlah pesan bukan milik sendiri dengan
  `seq > last_read_seq` (di MVP cukup `last_message_seq - last_read_seq` sebagai perkiraan, dibatasi `99+`).
- **Semua query membawa `tenant_id`.** Repository tidak menyediakan fungsi tanpa `tenantID`, dan ada test isolasi tenant.
- Pesan yang dihapus adalah soft delete: `deleted_at` diisi, `text`/`attachments` dikosongkan, `rev_seq` naik.
- Secret tenant (api secret, kredensial FCM, webhook secret) dienkripsi AES-GCM dengan master key dari env.

---

## 7. Protokol

### Autentikasi

- **Server API** (dipanggil backend tenant): header `X-Api-Key` + `Authorization: Bearer <server JWT>` yang
  ditandatangani `api_secret` (claim `server: true`, `exp` pendek).
- **Client** (SDK): `api_key` + user token JWT `{sub: external_id, app: api_key, iat, exp}` yang dibuat backend tenant.
  SDK menerima `tokenProvider` dan memanggilnya ulang saat token kedaluwarsa.

### REST client (`/v1`)

```
GET    /me
GET    /channels?updated_since=&cursor=              daftar channel user, urut last_message_at
POST   /channels                                     buat group/DM (kalau config tenant mengizinkan, D11)
GET    /channels/{id}
PATCH  /channels/{id}                                nama / avatar
POST   /channels/{id}/members                        DELETE /channels/{id}/members/{userId}
GET    /channels/{id}/messages?before_seq=&limit=    history (scroll ke atas)
GET    /channels/{id}/sync?since_rev=&limit=         perubahan sejak cursor (D8)
POST   /channels/{id}/messages                       fallback kirim pesan (idempotent client_msg_id)
PATCH  /messages/{id}                                DELETE /messages/{id}
PUT    /messages/{id}/reactions/{emoji}              DELETE /messages/{id}/reactions/{emoji}
POST   /channels/{id}/read                           {seq}
POST   /attachments                                  → {attachment_id, upload_url}
POST   /attachments/{id}/complete
POST   /devices                                      DELETE /devices/{token}
```

### Server API (`/v1/server`)

```
PUT    /server/users/{externalId}                    upsert user
DELETE /server/users/{externalId}                    hapus user (anonimisasi)
POST   /server/channels/dm                           {user_a, user_b} → buat atau ambil DM
POST   /server/channels                              buat group + member
POST   /server/channels/{id}/members                 DELETE /server/channels/{id}/members/{externalId}
POST   /server/channels/{id}/freeze                  bekukan channel (mis. setelah blokir)
POST   /server/channels/{id}/messages                system message
```

### WebSocket (`/v1/ws`)

Amplop JSON: `{ "type": "...", "id": "<request id, opsional>", "data": { ... } }`

```
client → server:  auth {token, protocol_version}, ping,
                  message.send {channel_id, client_msg_id, text, attachments, reply_to_id},
                  typing.start / typing.stop {channel_id}, read.mark {channel_id, seq}
server → client:  auth.ok {user, server_time}, pong,
                  ack {id, message} | error {id, code, message},
                  message.new, message.updated, message.deleted,
                  reaction.added, reaction.removed, read.updated,
                  typing, presence, channel.updated, channel.added, channel.removed,
                  member.added, member.removed
```

- Setiap event channel membawa `channel_id` dan `rev_seq`.
- Client mengirim `ping` setiap 25 detik (Cloudflare memutus koneksi idle sekitar 100 detik). Server menutup koneksi
  yang tidak ping selama 60 detik.
- Saat shutdown, server menutup koneksi dengan close code `1012` (service restart) supaya client langsung reconnect.
- Kode error dan batas (panjang teks, ukuran file, rate limit) didefinisikan di `spec/events.md`.

---

## 8. Alur utama

### Kirim pesan

1. SDK menyimpan pesan lokal dengan status `pending` dan `client_msg_id`, lalu mengirim `message.send`.
2. Gateway memvalidasi: user member channel, channel tidak dibekukan, lolos rate limit (token bucket Redis).
3. Satu transaksi Postgres: `UPDATE channels SET last_seq = last_seq + 1, last_message_seq = last_seq + 1 ... RETURNING`,
   `INSERT messages`, `INSERT outbox (topic 'message.created')`. Kalau `client_msg_id` sudah ada, pesan lama dikembalikan.
4. Setelah commit: `ack` ke pengirim, `PUBLISH marpokat:{tenant}:ch:{channel}`, lalu semua pod mengirim `message.new`
   ke member yang online.
5. Worker relay mem-publish outbox ke exchange `marpokat.events`, diteruskan ke queue `push` dan `webhook`.

### Reconnect dan sync

1. SDK connect + `auth`, lalu `GET /channels?updated_since=<terakhir>`.
2. Untuk channel yang di-cache atau sedang dibuka: `GET /channels/{id}/sync?since_rev=<cursor>` sampai habis.
3. Kalau event realtime datang dengan `rev_seq` yang lompat, SDK memanggil sync untuk channel itu.

### Media

1. `POST /attachments {mime, size}`: server memvalidasi tipe dan batas (gambar 10MB, video 50MB, file 25MB), membuat baris
   `pending`, dan memberi presigned PUT URL.
2. Client upload langsung ke MinIO, lalu `POST /attachments/{id}/complete`.
3. Worker media membuat thumbnail + blurhash (logika `narrativ-media-worker` dipakai ulang), status menjadi `ready`.
4. Pesan dikirim dengan `attachments: [attachment_id]`. URL download adalah presigned GET berumur pendek.

### Reaction

`PUT /messages/{id}/reactions/{emoji}`: insert `ON CONFLICT DO NOTHING`, update `reaction_counts`, naikkan `rev_seq`,
outbox + publish `reaction.added`. Payload pesan memuat `{emoji: count}` dan daftar reaction milik user sendiri.

### Push notification

Consumer push mengambil member yang offline (tidak ada presence) dan tidak mute, mengambil device token, lalu mengirim
lewat FCM memakai kredensial tenant. Payload: `channel_id`, `message_id`, nama pengirim, cuplikan teks (untuk deep link).

---

## 9. Peta reliabilitas

| Titik gagal                                         | Penanganan                                                                                         |
| --------------------------------------------------- | -------------------------------------------------------------------------------------------------- |
| Koneksi putus sebelum `ack`                         | SDK retry dengan `client_msg_id` yang sama, server mengembalikan pesan yang sudah ada               |
| Pod mati setelah commit, sebelum publish Redis      | Penerima mendeteksi celah `rev_seq` atau sync saat reconnect                                       |
| RabbitMQ down                                       | Event aman di tabel `outbox`, relay lanjut setelah broker pulih                                    |
| FCM / webhook gagal sementara                       | Retry queue bertingkat (10s → 1m → 10m lewat DLX + TTL), lalu `*.dlq` + metric dan alert           |
| Token FCM tidak valid                               | Hapus dari `devices`                                                                               |
| Consumer crash di tengah job                        | Ack manual, pesan di-redeliver                                                                     |
| Event duplikat (outbox at-least-once)               | Consumer dedup berdasarkan `event_id` (key Redis dengan TTL)                                       |
| Dua user membuat DM bersamaan                       | UNIQUE `dm_key`, yang kalah mengambil channel yang sudah ada                                       |
| Restart / deploy pod gateway                        | Close code `1012`, SDK reconnect dengan backoff + jitter, lalu sync                                |
| Tabel outbox membesar                               | Job pembersih menghapus baris `published_at` lebih dari 7 hari                                     |

---

## 10. Pekerjaan infra dan gitops

| ID     | Item                                                                                                                         | Lokasi              | Dibutuhkan | Status | Ref |
| ------ | ---------------------------------------------------------------------------------------------------------------------------- | ------------------- | ---------- | ------ | --- |
| INF-01 | Repo GitHub `marpokat` (private) + GHCR image `marpokat-server`                                                              | GitHub              | M0         | [x]    | b93975e |
| INF-02 | Postgres dev: role + database `marpokat_dev` (owner role tersebut). Alias pgbouncer tidak perlu: entri `*` sudah mengarah ke postgres-dev | infra VM | M0 | [x] | 2026-11-22, migrasi 00001 sudah jalan |
| INF-03 | RabbitMQ: vhost `/marpokat-dev` + user `marpokat_dev` dengan izin configure/write/read                                       | infra VM            | M0         | [x]    | 2026-11-22 |
| INF-04 | MinIO: bucket `marpokat-media-dev` + user `marpokat-dev` dengan policy terbatas bucket tersebut (selesai 2026-11-22). Sisa: aturan CORS untuk origin web | infra VM | M5 | [ ] | bucket + user selesai |
| INF-05 | Redis dev: pakai `infra-redis` (6379, tanpa password) dengan prefix `marpokat:` (D20)                                        | infra VM            | M0         | [x]    | tidak ada yang perlu dibuat |
| INF-06 | `homelabs-gitops/apps/marpokat-dev`: namespace, configmap, deployment `api` (2 replika) + `worker`, service, ingress, Job migrate (hook Sync, wave 0), kustomization, README langkah setup | gitops | M0 | [x] | gitops 87d6161, 2df4348 |
| INF-07 | Ingress WebSocket: `proxy-read-timeout` dan `proxy-send-timeout` `3600`                                                      | gitops ingress      | M3         | [x]    | ikut INF-06 |
| INF-08 | Cloudflare Tunnel hostname `api-dev-marpokat.rizkysiregar.my.id` ke origin ingress-nginx                                     | Cloudflare          | M0         | [x]    | 2026-11-22 |
| INF-09 | Secret `marpokat-secrets` di namespace dev (DB, Redis, RabbitMQ, MinIO, master key enkripsi), `ghcr-pull`                    | kubectl             | M0         | [x]    | 2026-11-22 |
| INF-10 | Jenkinsfile: test (gofmt, vet, test), build image, push GHCR, update tag di `apps/marpokat-dev`. Pipeline Jenkins dibuat dengan Script Path `server/Jenkinsfile` | `server/Jenkinsfile`| M0 | [x] | build pertama b93975eec3a5 |
| INF-11 | Argo CD Application `marpokat-dev` (auto-sync)                                                                               | Argo CD             | M0         | [x]    | 2026-11-22 |
| INF-12 | Prod: database `marpokat` di `infra-postgres-prod`, vhost `/marpokat`, bucket `marpokat-media`, Redis prod 6380 prefix        | infra VM            | M8         | [ ]    |     |
| INF-13 | `apps/marpokat` (prod, manual sync, promosi tag lewat PR) + hostname `api-marpokat.rizkysiregar.my.id` + Argo Application    | gitops + Cloudflare | M8         | [ ]    |     |
| INF-14 | Dashboard Grafana (koneksi WS, pesan/detik, latensi kirim, panjang queue, isi DLQ) + alert DLQ tidak kosong                  | monitoring          | M8         | [ ]    |     |
| INF-15 | Backup harian database `marpokat` dan uji restore                                                                            | infra VM            | M8         | [ ]    |     |

---

## 11. Integrasi Narrativ (tenant #1)

| ID    | Item                                                                                                                                   | Lokasi                   | Dibutuhkan | Status | Ref |
| ----- | -------------------------------------------------------------------------------------------------------------------------------------- | ------------------------ | ---------- | ------ | --- |
| NV-01 | Tenant `narrativ-dev` dibuat lewat CLI, config `client_can_create_dm: false`, `client_can_create_group: true`                          | marpokat CLI             | M1         | [ ]    |     |
| NV-02 | `MarpokatClient` di backend (server JWT, upsert user, buat DM/group, freeze channel), config `MARPOKAT_*` di configmap + secret        | narrativ-backend         | M1         | [ ]    |     |
| NV-03 | `POST /api/v1/chat/token`: user token Marpokat untuk user yang login                                                                   | narrativ-backend         | M1         | [ ]    |     |
| NV-04 | Upsert user ke Marpokat saat register dan update profil (async lewat RabbitMQ Narrativ), plus backfill sekali untuk user lama          | narrativ-backend         | M2         | [ ]    |     |
| NV-05 | `POST /api/v1/chat/dm/{userId}`: cek blokir/privasi, lalu buat atau ambil DM di Marpokat                                               | narrativ-backend         | M2         | [ ]    |     |
| NV-06 | Saat user memblokir user lain: freeze channel DM keduanya                                                                              | narrativ-backend         | M5         | [ ]    |     |
| NV-07 | Kredensial FCM project Firebase Narrativ (dev, lalu prod) disimpan di tenant                                                           | marpokat CLI             | M4         | [ ]    |     |
| NV-08 | Layar chat web: daftar channel, ruang chat, kirim media, reaction, unread badge, tombol "Kirim pesan" di profil                        | narrativ-web-client      | M6         | [ ]    |     |
| NV-09 | Layar chat Android (Compose): sama dengan NV-08, plus deep link dari push                                                               | narrativ-android         | M7         | [ ]    |     |
| NV-10 | Hapus akun Narrativ juga menghapus/anonimkan user di Marpokat                                                                          | narrativ-backend         | M8         | [ ]    |     |

---

## 12. Milestone

### M0: Fondasi ✅

- [x] Inisialisasi git (branch `main`), README, LICENSE, `.editorconfig`, `.gitignore`
- [x] Commit awal + repo GitHub `marpokat` (INF-01, private)
- [x] `spec/openapi.yaml` draft v1: semua endpoint bagian 7, schema error seragam, batas v1. Lolos `redocly lint` (`spec/redocly.yaml`)
- [x] `spec/events.md` draft v1: amplop, lifecycle, heartbeat, request, event, ordering/gap/sync, reconnect, kode error, close code, batas, payload push dan webhook
- [x] Migrasi awal skema bagian 6 (`00001_init.sql`), ditambah `attachments.file_name` dan status `processing`. Diuji up → down → up lewat pgbouncer transaction mode
- [x] Skeleton Go: `cmd/marpokat` dengan subcommand `api`, `worker`, `migrate up|down|status`, `version`; config env per subcommand; `slog` JSON; `/healthz`, `/readyz`, `/metrics` di port management; graceful shutdown dengan jeda drain (D22)
- [x] Koneksi platform: pgx (simple protocol), go-redis (helper key prefix), RabbitMQ (reconnect otomatis dengan backoff + jitter), MinIO (cek bucket), semua tampil di `/readyz`. Diuji: RabbitMQ dimatikan → worker 503, dinyalakan lagi → 200 tanpa restart
- [x] Middleware HTTP: request id, recover panic, log request, metrics per route pattern, error JSON seragam, 404 JSON
- [x] Setup `sqlc` (via Docker, output `internal/store`), `golangci-lint` v2 (via Docker, 0 issue), `Makefile`, `docker-compose.yml` (Postgres + pgbouncer, Redis, RabbitMQ, MinIO)
- [x] Dockerfile (multi-stage, distroless, UID 10001), image 27 MB, `migrate` dari container berhasil
- [x] Unit test: config, httpx, health, rabbitmq, redisx, drain shutdown (`go test -race` hijau)
- [x] Infra dev: INF-02, INF-03, INF-05, INF-08. Diuji dari laptop: migrate, api, dan worker ke infra VM dev semua `ok`, MinIO terbuka dengan key terbatas
- [x] Secret cluster INF-09 (`marpokat-secrets`, `ghcr-pull`)
- [x] Jenkinsfile (INF-10), gitops `apps/marpokat-dev` (INF-06), Argo Application (INF-11). Catatan: Job migrate awalnya PreSync, gagal `CreateContainerConfigError` karena ConfigMap belum ada saat sync pertama; diganti hook Sync dengan sync-wave
- [x] **Selesai jika:** `https://api-dev-marpokat.rizkysiregar.my.id/healthz` menjawab 200 lewat Cloudflare dan migrasi jalan otomatis (2026-11-22, versi `b93975eec3a5`)

### M1: Tenant dan autentikasi 🟡

- [x] Modul `tenant`: model, enkripsi secret (`platform/secretbox`, AES-256-GCM dengan konteks per kolom), config jsonb, cache api key 30 detik (`tenant.Resolver`, juga cache negatif 10 detik)
- [x] CLI `marpokat tenant create|list|show|rotate-secret|set-config|set-fcm|set-webhook|disable|enable|token`
- [x] Middleware server auth (`X-Api-Key` + server JWT, maks 5 menit) dan user auth (user JWT + user harus ada), pesan error yang jelas (algoritma, signature, kedaluwarsa)
- [x] `PUT/DELETE /v1/server/users/{external_id}`, `GET /v1/me`. Hapus user = anonimisasi + hapus device + keluar dari semua channel (satu transaksi)
- [x] Semua query user lewat sqlc dengan `tenant_id` + integration test (testcontainers, `make test-integration`): lifecycle user, isolasi tenant, kegagalan auth, rotasi secret, tenant nonaktif, CLI
- [ ] Tenant Narrativ dev (NV-01), NV-02, NV-03
- [ ] **Selesai jika:** Narrativ dev bisa upsert user dan memberi token, dan token itu bisa memanggil `GET /me`

### M2: Messaging inti ⚪

- [ ] Channel: buat DM (server API, `dm_key`), buat group, detail, update, tambah/keluarkan member, role owner/admin/member
- [ ] `GET /channels` (urut `last_message_at`, `updated_since`, cursor)
- [ ] Kirim pesan REST: transaksi seq + message + outbox, idempotensi `client_msg_id`, validasi panjang teks
- [ ] `GET /messages?before_seq=` dan `GET /sync?since_rev=`
- [ ] System message (member masuk/keluar, channel dibuat)
- [ ] `POST /channels/{id}/read`, perhitungan unread
- [ ] Integration test dengan testcontainers untuk semua alur di atas
- [ ] NV-04, NV-05
- [ ] **Selesai jika:** dua user Narrativ dev bisa DM dan group lewat REST, history dan sync benar, test hijau

### M3: Realtime ⚪

- [ ] WS endpoint: handshake `auth`, `protocol_version`, ping/pong, timeout, batas ukuran frame
- [ ] Hub per pod: session per user (multi device), subscribe Redis per tenant, routing event ke member channel
- [ ] `message.send` via WS + `ack`/`error`, publish setelah commit
- [ ] Event `message.new`, `channel.added/updated/removed`, `member.added/removed`, `read.updated`
- [ ] Typing (ephemeral, throttle) dan presence (Redis TTL + heartbeat)
- [ ] Rate limit per user dan per tenant (token bucket Redis)
- [ ] Graceful shutdown dengan close code `1012`
- [ ] INF-07
- [ ] Prototipe client JS minimal (`examples/`) untuk uji manual
- [ ] **Selesai jika:** dua browser chat realtime lewat 2 replika pod, kill satu pod lalu chat tetap jalan setelah reconnect + sync

### M4: Worker dan push ⚪

- [ ] Outbox relay (`SKIP LOCKED`, batch, publisher confirm RabbitMQ) + job pembersih outbox
- [ ] Topologi RabbitMQ: exchange `marpokat.events`, queue `push`, `webhook`, `media`, retry queue bertingkat, DLQ (dideklarasikan otomatis saat start)
- [ ] Dedup consumer berdasarkan `event_id`
- [ ] `POST/DELETE /devices`
- [ ] Consumer push: pilih penerima offline + tidak mute, kirim FCM (Android + Web Push), hapus token invalid
- [ ] NV-07
- [ ] Metrics: panjang queue, retry, DLQ, latensi relay
- [ ] **Selesai jika:** matikan RabbitMQ saat mengirim pesan, nyalakan lagi, push tetap terkirim tanpa duplikat

### M5: Media dan reaction ⚪

- [ ] INF-04
- [ ] `POST /attachments` + `complete`, validasi mime/ukuran, presigned PUT/GET
- [ ] Consumer media: thumbnail gambar/video + blurhash, status `ready`, event `message.updated`
- [ ] Pesan dengan attachment, validasi attachment milik pengirim dan berstatus `ready`
- [ ] Reaction: tambah/hapus, `reaction_counts`, event `reaction.added/removed`
- [ ] Edit dan hapus pesan (pengirim; admin group boleh hapus), event `message.updated/deleted`
- [ ] Reply (`reply_to_id`) dengan cuplikan pesan asal
- [ ] Job pembersih attachment `pending` yang tidak pernah dipakai
- [ ] NV-06
- [ ] **Selesai jika:** kirim foto + reaction + edit dari prototipe web, semuanya tampil benar setelah reconnect + sync

### M6: SDK JS + Narrativ web ⚪

- [ ] Scaffold `sdk-js` (workspace npm, TypeScript strict, Vitest, tsup), tipe di-generate dari `spec/openapi.yaml`
- [ ] `@marpokat/core`: `MarpokatClient.connect({apiKey, tokenProvider})`, reconnect backoff + jitter, ping, refresh token
- [ ] State channel dan pesan, antrean pesan pending + retry idempotent, deteksi celah `rev_seq` → sync
- [ ] Upload attachment (presigned) dengan progress
- [ ] `@marpokat/react`: `useChannels`, `useChannel`, `useMessages`, `useSendMessage`, `useTyping`, `useUnreadCount`
- [ ] Cache IndexedDB untuk daftar channel dan pesan terakhir
- [ ] Publish ke GitHub Packages
- [ ] NV-08
- [ ] **Selesai jika:** chat dipakai internal di Narrativ web dev

### M7: SDK Android + Narrativ Android ⚪

- [ ] Scaffold `sdk-android` (library module, Kotlin, kotlinx.serialization, OkHttp, Room)
- [ ] Koneksi WS + reconnect + ping, sadar lifecycle app (disconnect di background, reconnect di foreground)
- [ ] Repository channel/pesan dengan Room sebagai sumber UI (offline-first), antrean kirim, sync
- [ ] Upload attachment, reaction, read, typing
- [ ] Registrasi push token dan helper parsing payload push
- [ ] Publish ke GitHub Packages (Maven)
- [ ] NV-09
- [ ] **Selesai jika:** chat lintas web ↔ Android di Narrativ dev, push membuka ruang chat yang benar

### M8: Hardening dan launch prod ⚪

- [ ] Webhook ke tenant (event pesan, signature HMAC, retry + DLQ)
- [ ] Load test k6: 1000 koneksi WS serentak + 50 pesan/detik, catat p95 latensi kirim → terima
- [ ] Review keamanan: isolasi tenant, validasi input, batas ukuran, rate limit, rotasi secret
- [ ] INF-12, INF-13, INF-14, INF-15
- [ ] Tenant `narrativ` prod + kredensial FCM prod, NV-10
- [ ] Dokumentasi: `docs/` integrasi tenant (alur token, server API, event, SDK quickstart)
- [ ] **Selesai jika:** chat rilis ke user Narrativ prod

---

## 13. Di luar MVP

Developer console web, billing/metering, UI kit, thread, search full-text, E2E encryption, voice/video call, channel publik,
moderasi otomatis (filter kata, deteksi spam), pesan terjadwal, multi-region, iOS/Flutter SDK. Desain di atas tidak menutup
kemungkinan untuk semuanya.

---

## 14. Risiko dan catatan

- **Isolasi tenant.** Bug di sini berarti data satu tenant bocor ke tenant lain. Mitigasi: repository wajib `tenantID`,
  test isolasi khusus, review di M8.
- **Infra VM single point of failure.** Postgres/Redis mati berarti chat mati. Diterima untuk skala sekarang, dengan
  backup harian (INF-15).
- **Logika client SDK** (reconnect, deteksi celah, antrean pending) paling rawan bug. Alokasikan test lebih banyak di SDK
  daripada di server.
- **WebSocket di belakang Cloudflare.** Ping 25 detik dan timeout ingress panjang wajib, kalau tidak koneksi putus diam-diam.
- **Redis bersama prod Narrativ (D20).** Semua key dan channel wajib berawalan `marpokat:`. Pantau memori Redis.
- **Data pribadi (UU PDP).** Hapus akun di tenant harus menghapus/anonimkan data di Marpokat (NV-10). Tentukan retensi
  pesan sebelum prod.

---

## 15. Log perubahan dokumen

| Tanggal    | Perubahan                                                                      |
| ---------- | ------------------------------------------------------------------------------ |
| 2026-11-22 | Dokumen dibuat: keputusan D1–D21, tech stack, desain, infra, milestone M0–M8   |
| 2026-11-22 | M0 dimulai: D22–D26 ditambahkan, progress M0, INF-06/07/10                     |
| 2026-11-22 | M0 selesai: infra VM dev, repo, Jenkins, Argo CD; dev live                     |
