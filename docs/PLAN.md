# MARPOKAT: Development Plan

Marpokat adalah **chat as a service**: satu platform chat multi-tenant (API + realtime gateway + SDK) yang bisa dipakai
aplikasi mana pun yang butuh fitur chat. Tenant pertama adalah **Narrativ** (Android + web), tapi Narrativ memakai API publik
yang sama dengan tenant lain nanti, tanpa jalan pintas khusus.

Dokumen ini acuan utama: keputusan arsitektur, tech stack, desain data dan protokol, pekerjaan infra, integrasi Narrativ,
serta urutan milestone beserta progress tiap item.

- **Status:** pengembangan MVP selesai (2026-11-23): chat diuji di Narrativ web dan Android dev. Sekarang M8: menyiapkan prod (manifest `apps/marpokat` disiapkan, langkah infra INF-12 menunggu). Push (NV-07) belum diuji di dev
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
| M1        | Tenant dan autentikasi                                     | ✅ Selesai |
| M2        | Messaging inti (DM, group, history, sync, outbox)          | ✅ Selesai |
| M3        | Realtime gateway (WebSocket, fan-out, typing, read)        | ✅ Selesai |
| M4        | Worker, push notification, retry dan DLQ                   | 🟡 Berjalan |
| M5        | Media dan reaction, edit/hapus pesan                       | 🟡 Kode selesai, belum diuji di dev |
| M6        | SDK JS + chat di Narrativ web                              | ✅ Selesai (diuji di dev) |
| M7        | SDK Android + chat di Narrativ Android                     | 🟡 Chat web ↔ Android jalan di dev; push belum diuji |
| M9        | Website: portal pengguna + admin internal (Vue, frontend murni) | 🟡 Console API untuk browser selesai di Go (D69–D73); frontend Vue belum |
| M8        | Hardening dan launch prod                                  | 🟡 Hardening dev selesai, setup prod disiapkan |

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
| D31 | **Foto profil hasil upload Narrativ tidak dikirim ke Marpokat**, hanya URL eksternal (Google). `metadata.username` dikirim supaya app mengambil foto dari profil Narrativ | URL presigned MinIO Narrativ kedaluwarsa dalam 1 jam, sehingga avatar di chat akan rusak. Nanti bisa diganti endpoint avatar Narrativ yang stabil |
| D30 | **Format kredensial:** api key `mpk_` + 32 hex, api secret `mps_` + 43 karakter base64url (256 bit), webhook secret `mpw_`. Secret dipakai apa adanya sebagai kunci HMAC | Prefix membuat kredensial mudah dikenali (dan mudah dideteksi secret scanner). Tenant cukup menempelkan string secret ke library JWT mereka                                    |
| D32 | **Profil user di Marpokat disinkronkan saat token dan saat DM dibuka**, bukan lewat hook register/update profil (NV-04 versi awal). Backfill user lama tidak perlu | Setiap user yang bisa chat pasti meminta token atau dibuka DM-nya dulu, jadi tidak ada user "belum terdaftar". Kekurangannya: nama baru terlihat teman chat setelah user itu meminta token lagi. Hook update profil bisa ditambah nanti |
| D33 | **Daftar id ke query dikirim sebagai teks dipisah koma** (`string_to_array(...)::uuid[]`)                                                          | Driver Postgres dalam simple protocol (wajib karena pgbouncer, D7) tidak bisa meng-encode array uuid. Ditemukan oleh integration test M2                                       |
| D34 | **View `message_details`** (pesan + profil pengirim) untuk semua jalur baca pesan                                                                  | Satu bentuk hasil dari satu query, tanpa menyalin konversi di tiap query                                                                                                       |
| D35 | **Unread = jumlah pesan user lain setelah `last_read_seq`, tanpa pesan sistem, maksimal 100**. Mengirim pesan menandai channel terbaca sampai pesan itu. Member baru mulai "sudah membaca semuanya" | Pesan sistem tidak perlu membuat badge. Riwayat sebelum bergabung tidak menjadi unread. Sinkronisasi sendiri tetap lengkap karena memakai `rev_seq`                            |
| D36 | **Perubahan channel diumumkan sebagai pesan sistem** (`type: system`, `text: null`, `metadata.event`): `channel.created`, `channel.renamed`, `member.added`, `member.left`, `member.removed` | Ikut urutan, history, dan sync tanpa mekanisme tambahan. Teks dirangkai client dalam bahasa user                                                                               |
| D37 | **Peran `admin` ada di skema dan izin, tapi belum ada endpoint untuk mengangkat admin**                                                            | Di MVP cukup owner dan member. Endpoint promosi ditambah saat dibutuhkan                                                                                                       |
| D38 | **Pesan terkunci per channel lewat baris `channels`** (`UPDATE ... last_seq + 1`), commit selalu berurutan seq                                      | Pembaca tidak pernah melihat seq N+1 tanpa N, sehingga kursor sync aman. Biayanya: kirim ke satu channel diserialkan, cukup untuk skala target                                  |
| D39 | **Fan-out per user, bukan per channel**: satu topik Redis `marpokat:u:{tenant}:{user}`; pod hanya subscribe untuk user yang terkoneksi ke pod itu. Penerima ditentukan dari database saat event dipublikasikan | Menggantikan rencana awal (subscribe per channel). Keanggotaan selalu benar karena dibaca saat itu, tanpa cache keanggotaan di pod yang bisa basi. Biayanya satu PUBLISH per penerima, wajar untuk skala target |
| D40 | **Event realtime dikirim oleh service setelah commit lewat antarmuka `Notifier`**, best effort. Outbox (D6) tetap hanya untuk worker (push, webhook) di M4 | Realtime boleh hilang karena ada sync (D8). Pekerjaan yang wajib terkirim ada di outbox. Package `channel` dan `message` tidak mengenal `realtime` |
| D41 | **Langganan Redis dipantau**: klien subscriber sendiri dengan hook `OnConnect` (koneksi ulang berarti event di celah hilang) ditambah beat ke diri sendiri tiap 10 detik (koneksi setengah mati). Jika salah satu terjadi, semua sesi ditutup 1012 dan client sync | Pub/Sub yang tidak diterima hilang selamanya, dan library bisa menyambung ulang diam-diam. Lebih baik client reconnect + sync daripada pura-pura tidak ada yang hilang |
| D42 | **CORS `*` tanpa credentials** untuk REST, dan WebSocket menerima origin apa pun | Autentikasi lewat header (`X-Api-Key` + bearer) dan frame `auth`, tidak pernah cookie, jadi tidak ada otoritas ambien yang bisa disalahgunakan halaman lain. Wajib agar SDK web dari origin tenant bisa memanggil API |
| D43 | **Presence = sorted set Redis per user** (skor = batas waktu koneksi, diperbarui tiap ping, TTL 70 detik) plus `GET /v1/presence` untuk kondisi saat ini | Pod yang crash tidak sempat berpamitan: user-nya offline sendiri setelah TTL. Event `presence` hanya memberi tahu perubahan, jadi client yang baru konek perlu REST |
| D44 | **Rate limit token bucket di Redis (Lua, waktu dari Redis)**: kirim pesan 10 burst lalu 1/detik per user, 500 burst lalu 100/detik per app; 30 frame/detik per koneksi (lokal); maksimal 10 koneksi per user di semua pod. Redis mati: limiter membolehkan (fail open) | Satu user di dua pod tetap satu user. Limiter yang rusak tidak boleh mematikan chat |
| D45 | **Klien lambat ditutup dengan 1013** saat antrean 256 event penuh; typing kedaluwarsa sendiri di server (6 detik) dan diakhiri saat koneksi putus | Satu klien lambat tidak boleh menahan pod atau memakan memori. Client reconnect + sync |
| D46 | **Pod hanya ready jika langganan Redis-nya aktif** (`/readyz` memuat `realtime`) | Pod tanpa langganan akan menerima WebSocket yang tidak bisa dilayaninya |
| D47 | **Relay outbox**: polling 200 ms (D7), batch 100, `FOR UPDATE SKIP LOCKED`, event ditandai terkirim **hanya setelah RabbitMQ mengonfirmasi** (publisher confirm). At-least-once: crash di antara publish dan tanda bisa menerbitkan ulang, consumer yang menanganinya. Relay menunggu (tanpa menyentuh tabel) selama broker tidak terjangkau | Event tidak pernah hilang karena crash atau broker mati; duplikat murah ditangani, kehilangan tidak |
| D48 | **Topologi RabbitMQ** per work queue: exchange `events` (topic), `retry` (direct), `dlx`; queue kerja, tiga queue retry bertingkat (10 detik, 1 menit, 10 menit; TTL lalu dead letter kembali ke queue kerja), dan `*.dlq`. Pesan baru diakui hanya setelah penggantinya (retry/DLQ) dikonfirmasi broker. **Publisher ikut mendeklarasikan queue kerja** | RabbitMQ membuang pesan yang tidak punya queue terikat, padahal relay sudah menandainya terkirim: tanpa deklarasi oleh publisher, event di start pertama bisa hilang diam-diam. Ditemukan saat meninjau ulang, ada test-nya |
| D49 | **Penerima push**: member aktif selain pengirim, tidak mute, belum membaca pesan, **tanpa koneksi hidup di perangkat mana pun**, pesan tidak lebih tua dari 10 menit, bukan pesan sistem. Dedup per perangkat di Redis 24 jam | Yang sedang melihat chat tidak perlu dering. Pesan tua setelah backlog hanya membingungkan. Per perangkat supaya retry satu HP yang gagal tidak membunyikan HP lain dua kali |
| D50 | **FCM lewat HTTP v1 langsung** dengan service account milik tenant (`x/oauth2/jwt`), tanpa Firebase Admin SDK; pesan data-only (`android.priority HIGH`, `webpush Urgency high`, kedaluwarsa 1 jam). Klasifikasi: token tidak valid (404/UNREGISTERED/SENDER_ID_MISMATCH) → perangkat dihapus; 429/5xx/jaringan/401 → retry; selain itu (400 bukan soal token, kredensial rusak) → dilewati tanpa retry | Ringan, mudah diuji dengan Google palsu (token endpoint ikut `token_uri` di JSON). Kegagalan satu perangkat tidak boleh membawa seluruh event ke DLQ |
| D51 | **Perangkat**: `POST /v1/devices` memindahkan token ke user yang mendaftar terakhir, setiap user menyimpan 10 token terbaru, `DELETE` hanya menyentuh token milik sendiri dan selalu 204 | Ganti akun di satu HP, dan HP lama yang tidak pernah logout tidak menumpuk |
| D52 | **Metrics**: `marpokat_outbox_pending`, `outbox_published_total`, `outbox_lag_seconds`, `queue_messages{queue}` (termasuk `*.dlq`, dasar alert), `consumer_handled_total{result}`, `push_sent_total`, `push_failed_total{reason}`, `push_skipped_total{reason}` | Alert INF-14: DLQ tidak kosong, outbox pending terus naik |
| D53 | **Urutan shutdown worker**: job berhenti dulu (consumer berhenti menerima, yang sedang berjalan diselesaikan dan di-ack), baru koneksi RabbitMQ ditutup | Pesan yang sedang diproses saat pod berhenti tidak kembali ke antrean dan tidak diproses dua kali |
| D54 | **Lampiran**: `message_attachments` (UNIQUE `attachment_id`: satu upload hanya untuk satu pesan), status pending/processing/ready/failed, maks 10 per pesan. Upload via presigned PUT yang menandatangani Content-Type; ukuran diperiksa di `complete` lewat Stat (tidak cocok → failed + objek dihapus) | File tidak lewat API; batas ukuran tetap ditegakkan tanpa proxy |
| D55 | **Jenis dari mime**: image/jpeg,png,webp,gif,heic,heif; video/mp4,quicktime,webm,3gpp; selain itu (SVG termasuk) adalah `file` yang hanya diunduh dengan `content-disposition: attachment`. Batas 10/50/25 MB, gambar maks 64 juta piksel (dicek dari header sebelum decode) | SVG bisa memuat skrip; gambar raksasa kecil di disk bisa menghabiskan memori |
| D56 | **Thumbnail di worker**: gambar diproses di Go (EXIF auto-orient, JPEG 640px, blurhash 4x3); video lewat ffmpeg statis saja (poster + durasi dibaca dari banner ffmpeg, tanpa ffprobe agar image 186 MB, bukan 340 MB). Tanpa ffmpeg video disimpan tanpa poster; HEIC tanpa thumbnail | Satu image untuk api dan worker |
| D57 | **Pesan boleh dikirim saat lampiran masih `processing`**; saat siap pesan mendapat `rev_seq` baru dan `message.updated` (juga di channel yang dibekukan) | Pengirim tidak menunggu thumbnail |
| D58 | **Hapus pesan = tombstone**: teks, lampiran, dan reaction hilang, `seq` dan pengirim tetap, `rev_seq` baru agar sync membawanya; file dihapus pembersih setelah masa tenggang | Klien yang offline tetap tahu pesan itu dihapus |
| D59 | **Reaction**: emoji string 1-64 byte tanpa spasi/kontrol, maks 10 berbeda per user per pesan, idempoten (tidak mengubah `rev_seq`); `mine` dihitung per penerima di event dan respons | Aplikasi boleh punya emoji sendiri tanpa daftar tetap |
| D60 | **Reply**: cuplikan 200 karakter diambil saat dibaca (ikut edit/hapus pesan asal); membalas pesan yang baru dihapus diperbolehkan | Cuplikan tidak basi |
| D61 | **`GET /v1/server/channels/dm`** mencari DM tanpa membuatnya | Membekukan DM saat blokir tidak boleh memulai percakapan |
| D62 | ~~**Website = Laravel sebagai frontend saja**~~ Diganti D68 (2026-11-24). Yang tetap berlaku: website tanpa database sendiri; akun, sesi, proyek, kunci, dan audit disimpan di Postgres Marpokat dan diakses lewat **Console API** (`spec/console.yaml`) di Go | Satu pemilik data dan enkripsi; frontend bisa diganti tanpa migrasi data (terbukti saat Laravel diganti Vue) |
| D63 | ~~**Console API hanya di dalam cluster**, dijaga `X-Console-Key`~~ Diganti D69–D71 (2026-11-24): frontend murni tidak bisa menyimpan service key. Yang tetap berlaku: listener sendiri (`CONSOLE_ADDR`, Service `marpokat-console`), token sesi opaque (SHA-256 disimpan, idle 12 jam, maks 30 hari) | Semula: endpoint akun tidak terbuka ke internet. Sekarang dijaga cookie + cek origin + Turnstile + limit di Go |
| D64 | **Akun**: e-mail unik tanpa peduli huruf, argon2id (19 MiB, maks 3 hash bersamaan agar muat di pod 256 MiB), verifikasi e-mail wajib sebelum membuat proyek, `register` dan `forgot-password` menjawab sama untuk e-mail dikenal atau tidak, login dibatasi per e-mail dan per alamat, e-mail tidak dikenal dihitung selama hash yang salah | Tidak bisa dipakai untuk mengintip siapa punya akun |
| D65 | **Secret hanya tampil sekali** (dibuat atau dirotasi); rotasi, kredensial push, dan menonaktifkan proyek meminta password lagi (10 percobaan beruntun, lalu 1 per menit); proyek milik akun lain dijawab 404 | Sesi curian tidak cukup untuk mengambil alih kunci |
| D66 | **Platform admin** hanya lewat CLI (`marpokat account promote`), tidak ada API-nya; aksi admin tercatat di audit | Admin tidak bisa dibuat lewat celah API |
| D67 | **E-mail dikirim oleh Go** lewat SMTP (`MAIL_DRIVER=smtp`) atau dicetak ke log (`log`, hanya pengembangan), di latar belakang agar pendaftaran tidak menunggu server surat | Frontend tidak perlu tahu token verifikasi; link di e-mail (`CONSOLE_BASE_URL` + `/verify-email`, `/reset-password`) dibuka oleh portal yang meneruskan token ke API |
| D68 | **Website = dua aplikasi Vue yang murni frontend** (file statis, tanpa server Node/PHP, tanpa logika bisnis): **portal** untuk pengguna (landing, docs, getting started, daftar/masuk, proyek, akun) dan **admin** untuk internal (akun, proyek, statistik, audit). Repo `marpokat-web` menjadi monorepo pnpm dengan `apps/portal`, `apps/admin`, `packages/console-client`, `packages/theme`. Laravel dihentikan | Semua aturan bisnis, validasi otoritatif, captcha, dan limit ada di Go; frontend hanya tampilan, jadi mudah diganti dan murah di-host (nginx statis, read-only). Admin terpisah agar bisa dikunci dan di-deploy sendiri |
| D69 | **Console API dibuka ke browser lewat path yang sama dengan app-nya** (same-origin): Ingress tiap host web merutekan `/console/v1/*` ke Service `marpokat-console`, sisanya ke file statis app. Tanpa CORS. `X-Console-Key` dan `X-Client-IP` dihapus (Laravel tidak pernah menyala di dev, jadi tanpa masa transisi) | Same-origin berarti cookie `SameSite=Strict` bekerja dan tidak perlu membuka CORS ke origin lain |
| D70 | **Sesi browser = cookie `__Host-mp_session`** (`HttpOnly`, `Secure`, `SameSite=Strict`, `Path=/`) yang di-set Go saat `login` dan dihapus saat `logout`; token tidak pernah terlihat JavaScript. Perlindungan CSRF: setiap request yang mengubah data wajib membawa `Origin` portal (origin `CONSOLE_BASE_URL` + `CONSOLE_ALLOWED_ORIGINS`) atau admin (`CONSOLE_ADMIN_ORIGINS`) dan, bila ber-body, `Content-Type: application/json`. Semua jawaban `Cache-Control: no-store` | Token di `localStorage` bisa dicuri XSS; cookie `HttpOnly` tidak. SameSite + cek Origin menutup CSRF tanpa token tambahan |
| D71 | **Sesi punya scope `portal` atau `admin`** (kolom `console_sessions.scope`, migrasi 00005), ditentukan dari `Origin` saat login. Login dari origin admin (`CONSOLE_ADMIN_ORIGINS`) hanya untuk platform admin (selain itu 403, tanpa sesi). Endpoint `/admin/*` butuh sesi admin; sesi portal milik admin dijawab 403. Cookie host-only, jadi sesi portal dan admin terpisah. Host admin **publik seperti web biasa** (tanpa VPN atau Cloudflare Access, keputusan 2026-11-24): yang menjaga adalah server. Promosi admin tetap hanya CLI (D66) | Diikat ke sesi, bukan ke header saat dipakai: sesi portal admin yang bocor tidak cukup untuk mengelola platform |
| D72 | **Turnstile diverifikasi Go** di `register` dan `forgot-password` (`TURNSTILE_SECRET_KEY`; `400 captcha_failed`, `503 captcha_unavailable` karena gagal tertutup bila Cloudflare tak terjangkau; mati bila kosong untuk pengembangan). Dicek setelah validasi input dan limit, supaya salah ketik tidak menghabiskan token dan banjir request tidak diteruskan ke Cloudflare. Frontend hanya menampilkan widget dan mengirim `turnstile_token` | Secret captcha tidak boleh di browser; robot yang memanggil API langsung tetap terhalang |
| D73 | **Alamat pengunjung dibaca Go dari `CF-Connecting-IP`** hanya bila request datang dari proxy tepercaya (`CONSOLE_TRUSTED_PROXIES`, CIDR ingress; default rentang privat cluster), selain itu dari alamat koneksi. `X-Client-IP` dihapus bersama service key | Limit login/daftar dan audit tetap melihat orangnya, dan header tidak bisa dipalsukan dari luar |
| D74 | **Stack website**: Vue 3 + TypeScript + Vite. Portal pakai **Nuxt 4 mode statis** (`nuxt generate`: landing dan docs di-prerender untuk SEO, `/app/**` hanya di browser), Tailwind 4 + tema Marpokat (`--mp-*`), docs ditulis Markdown (Nuxt Content); Reka UI ditambah saat komponen headless pertama dibutuhkan. Admin pakai **Quasar** (SPA, komponen tabel/form/dialog lengkap) dengan warna brand dari token Marpokat, tanpa Tailwind. Bersama: client bertipe dari `spec/console.yaml` (`openapi-typescript` + `openapi-fetch`), TanStack Query untuk data server, Pinia untuk sesi, Zod + composable kecil `useForm` untuk form (validasi di browser hanya kenyamanan, Go tetap penentu; VeeValidate batal karena adapter Zod-nya masih Zod 3), Vitest; Playwright menyusul. Node 24, TypeScript 5.9 (vue-tsc belum mendukung TS 7) | Portal butuh SEO dan tampilan khas brand; admin butuh banyak tabel dan form cepat. Dua sistem styling tidak dicampur dalam satu app. Client yang di-generate dari spec membuat perubahan kontrak ketahuan saat compile |
| D75 | **Hapus proyek dengan masa tenggang 3 hari** (`CONSOLE_DELETION_GRACE`, minimal 1 jam). `DELETE /projects/{id}` (dengan password) langsung mematikan proyek dan mengisi `tenants.delete_after`; selama masa tenggang proyek bisa dipulihkan (tetap mati sampai dinyalakan), tidak dihitung dalam batas proyek, dan tidak bisa dinyalakan. Setelahnya worker (`tenant.Purger`, tiap 10 menit) menghapus semua file di MinIO dengan awalan `<tenant id>/`, lalu semua barisnya dalam satu transaksi (channel beserta member/pesan/reaction, lampiran, perangkat, outbox, user, tenant), terkunci `FOR UPDATE SKIP LOCKED`; pemilik mendapat `project_deleted` di log keamanannya | Salah klik tidak menghilangkan data pengguna pelanggan, tapi hak hapus (UU PDP) tetap terpenuhi. Gagal di tengah = rollback dan diulang ronde berikutnya |
| D76 | **Admin membaca platform secara luas**: detail akun (proyek, sesi, audit) dan "keluarkan dari semua sesi", detail proyek (pemilik, pemakaian, push), log audit seluruh platform (filter aksi/akun, kursor), pencarian akun (`?q=`, ILIKE dengan wildcard di-escape), deret harian 7/30/90 hari (akun baru, proyek baru, pesan, pengirim aktif; UTC) dan 5 proyek tersibuk. Pesan per hari dihitung dengan indeks BRIN `messages(created_at)`. Promote/demote lewat CLI ikut tercatat di audit | Admin cukup dari satu tempat tanpa membuka database. BRIN kecil dan murah ditulis karena pesan masuk berurutan waktu |
| D77 | **Perubahan tenant diumumkan ke semua pod** lewat satu topik kontrol Redis (`marpokat:ctl`, `tenant.changed`). Website (Console) mengumumkan saat proyek dimatikan, dihapus, secret dirotasi, atau pengaturan berubah. Setiap pod langsung membuang cache API key tenant itu (dan semua jawaban "key tidak dikenal", supaya proyek yang dinyalakan lagi langsung diterima); kalau tenant dimatikan, semua WebSocket-nya ditutup `4003` ("this app is switched off"), sehingga SDK berhenti reconnect. Diumumkan best effort: kalau Redis tidak terjangkau, TTL cache 30 detik tetap jadi batas. CLI `marpokat tenant` belum mengumumkan (tanpa Redis) | Proyek yang dimatikan atau dihapus berhenti total dalam hitungan milidetik (diukur: 6 ms dua pod di tes, 23 ms binary sungguhan), bukan 30 detik untuk REST dan sampai token habis untuk WebSocket |
| D78 | **Tidak ada environment dev/prod sebagai konsep**: pengguna membuat proyek terpisah untuk dev dan prod (key, data, push, pengaturan sendiri). Proyek boleh diberi **label** `development`, `staging`, atau `production` (atau kosong), yang tidak mengubah perilaku apa pun. Batas tetap 3 proyek per akun | Di chat tidak ada perilaku "sandbox" seperti di payment gateway; dua proyek sudah memberi pemisahan yang sama. Label membantu mengenali mana yang mana |
| D79 | **Kendali admin atas proyek**: *suspend* dengan alasan (kolom `suspended_at`/`suspended_reason`, terpisah dari switch off pemilik, sehingga pemilik tidak bisa membatalkannya; key ditolak dan WebSocket ditutup seketika lewat D77), *unsuspend*, *delete* dengan masa tenggang atau *delete now* (admin mengetik slug + password sendiri; worker menghapus semua data dalam ±1 menit, interval purge kini 1 menit), *restore*. Pemilik mendapat e-mail dan entri audit. Proyek milik admin lain tidak bisa disentuh (`target_is_admin`): demote dulu lewat CLI. Endpoint lama `/admin/projects/{id}/disable` deprecated | Admin punya kendali penuh tanpa membuka database, dan satu sesi admin yang bocor tidak bisa melumpuhkan admin lain. Kendali atas akun (suspend akun beserta proyeknya, hapus akun, hapus akun sendiri di portal) menyusul sebagai fase 2 |
| D80 | **Kendali atas akun.** Aturan hapus: dari **portal** (pemilik) proyek maupun akun **selalu** dengan masa tenggang 3 hari; dari **admin** proyek maupun akun **boleh pilih** langsung atau dengan masa tenggang. *Suspend akun* (admin, dengan alasan) memakai `accounts.disabled_at` + `suspended_reason`: tidak bisa login, semua sesi diakhiri, semua proyeknya di-suspend dengan `suspension_source = 'account'`; mencabutnya hanya melepas suspend yang berasal dari akun (suspend admin atas proyek tertentu tetap). *Hapus akun* (`accounts.delete_after`): proyeknya langsung berhenti; selama masa tenggang pemilik masih bisa login dan *restore* (tidak bisa membuat proyek); worker lalu menghapus semua proyeknya (data + file) dan akunnya (sesi, token, log audit ikut terhapus), e-mail bisa dipakai daftar lagi. Admin tidak bisa bertindak atas dirinya sendiri atau admin lain (demote lewat CLI dulu). Pemilik mendapat e-mail di setiap langkah | Pemilik punya hak hapus atas akunnya (UU PDP) dengan jaring pengaman; admin bisa bertindak cepat untuk penyalahgunaan atau permintaan hapus data. Sumber suspend dicatat agar dua alasan berbeda tidak saling membatalkan |

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
| `@rizkysiregar/marpokat-core`   | TypeScript strict, tanpa dependency framework. WebSocket native, fetch, IndexedDB (cache, akhir MVP)            |
| `@rizkysiregar/marpokat-react`  | Hooks di atas core (`useChannels`, `useMessages`, `useSendMessage`). Cocok dengan Zustand + React Query        |
| `marpokat-android` | Kotlin, OkHttp WebSocket, Coroutines + `StateFlow`, kotlinx.serialization, Room (cache + antrean kirim)        |
| Distribusi         | GitHub Packages (npm + Maven), private. Group Maven `com.rizkysiregar.marpokat`                                 |

### Website (repo `marpokat-web`, D68, D74)

| Bagian              | Pilihan                                                                                                   |
| ------------------- | --------------------------------------------------------------------------------------------------------- |
| Monorepo            | pnpm workspaces: `apps/portal`, `apps/admin`, `packages/console-client`, `packages/theme`                 |
| Bahasa / build      | TypeScript strict, Vue 3 (Composition API, `<script setup>`), Vite                                        |
| Portal              | Nuxt 4 mode statis (`nuxt generate`), Nuxt Content (docs Markdown), Tailwind 4, tema `--mp-*`             |
| Admin               | Quasar 2 (SPA, Vite), brand dari token Marpokat, tanpa Tailwind                                           |
| Data / state        | `openapi-fetch` + tipe dari `spec/console.yaml` (`openapi-typescript`), TanStack Query (Vue), Pinia       |
| Form                | Zod 4 + `useForm` (error field dari API ditampilkan di field-nya; hanya kenyamanan, Go yang menentukan)  |
| Tes                 | Vitest (unit), uji kontras tema WCAG; e2e Playwright di CI menyusul                                       |
| Kualitas            | ESLint (flat config) + Prettier, `vue-tsc`                                                                |
| Runtime             | Image nginx-unprivileged berisi file statis, read-only, UID 10001; header keamanan dan CSP di config nginx |

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

Website (D68–D73): dua host, masing-masing satu Ingress dengan dua path. Browser hanya bicara ke origin-nya sendiri.

```
 marpokat.rizkysiregar.my.id (portal)        admin-marpokat.rizkysiregar.my.id (admin, publik)       
   /            → portal (nginx, file statis)   /            → admin (nginx, file statis)
   /console/v1  ─┐                              /console/v1  ─┐
                 └──────────────┬───────────────────────────────┘
                                ▼
                  marpokat console (Go, CONSOLE_ADDR)
                  cookie __Host-mp_session, cek Origin, Turnstile,
                  limit, /admin/* hanya dari origin admin
                                ▼
                            Postgres
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
| INF-13 | `apps/marpokat` (prod, manual sync, promosi tag lewat PR) + hostname `api-marpokat.rizkysiregar.my.id` + Argo Application    | gitops + Cloudflare | M8         | [ ]    | manifest + README langkah setup disiapkan (gitops PR); sisa: Cloudflare, Argo app, sync pertama |
| INF-14 | Dashboard Grafana (koneksi WS, pesan/detik, latensi kirim, panjang queue, isi DLQ) + alert DLQ tidak kosong                  | monitoring          | M8         | [ ]    |     |
| INF-15 | Backup harian database `marpokat` dan uji restore                                                                            | infra VM            | M8         | [ ]    |     |

---

## 11. Integrasi Narrativ (tenant #1)

| ID    | Item                                                                                                                                   | Lokasi                   | Dibutuhkan | Status | Ref |
| ----- | -------------------------------------------------------------------------------------------------------------------------------------- | ------------------------ | ---------- | ------ | --- |
| NV-01 | Tenant `narrativ-dev` dibuat lewat CLI, config `client_can_create_dm: false`, `client_can_create_group: true`                          | marpokat CLI             | M1         | [x]    | 2026-11-22, api key `mpk_3d780ec2…` |
| NV-02 | `MarpokatClient` di backend (server JWT, upsert user; buat DM/group dan freeze menyusul di M2/M5), config `APP_MARPOKAT_*` di configmap + secret | narrativ-backend | M1 | [x] | narrativ-backend#80, gitops 090b2af |
| NV-03 | `POST /api/v1/chat/token`: upsert user lalu user token Marpokat + api key + base URL. Contract test jjwt ↔ Go lolos                     | narrativ-backend         | M1         | [x]    | narrativ-backend#80 |
| NV-04 | ~~Upsert user saat register/update profil + backfill~~ Diganti D32: upsert saat token dan saat DM dibuka. Hook update profil menyusul bila perlu | narrativ-backend | M2 | [x] | lewat NV-03 dan NV-05 |
| NV-05 | `POST /api/v1/chat/dm/{userId}`: cek diri sendiri dan blokir (user di sisi blokir dianggap tidak ada), lalu buat atau ambil DM di Marpokat | narrativ-backend | M2 | [x] | narrativ-backend#82 |
| NV-06 | Saat user memblokir user lain: freeze channel DM keduanya                                                                              | narrativ-backend         | M5         | [x]    | narrativ-backend#83 |
| NV-07 | Kredensial FCM project Firebase Narrativ (dev, lalu prod) disimpan di tenant: `kubectl -n narrativ-dev get secret narrativ-firebase-dev -o jsonpath='{.data.credentials\.json}' \| base64 -d \| kubectl -n marpokat-dev exec -i deploy/marpokat-api -- marpokat tenant set-fcm --slug narrativ-dev --file -` | marpokat CLI | M4 | [ ] |     |
| NV-08 | Layar chat web: daftar channel, ruang chat, kirim media, reaction, unread badge, tombol "Kirim pesan" di profil                        | narrativ-web-client      | M6         | [x]    | narrativ-web-client#16 |
| NV-09 | Layar chat Android (Compose): sama dengan NV-08, plus deep link dari push                                                               | narrativ-android         | M7         | [x]    | narrativ-android#59; deep link dari push menunggu NV-07 |
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

### M1: Tenant dan autentikasi ✅

- [x] Modul `tenant`: model, enkripsi secret (`platform/secretbox`, AES-256-GCM dengan konteks per kolom), config jsonb, cache api key 30 detik (`tenant.Resolver`, juga cache negatif 10 detik)
- [x] CLI `marpokat tenant create|list|show|rotate-secret|set-config|set-fcm|set-webhook|disable|enable|token`
- [x] Middleware server auth (`X-Api-Key` + server JWT, maks 5 menit) dan user auth (user JWT + user harus ada), pesan error yang jelas (algoritma, signature, kedaluwarsa)
- [x] `PUT/DELETE /v1/server/users/{external_id}`, `GET /v1/me`. Hapus user = anonimisasi + hapus device + keluar dari semua channel (satu transaksi)
- [x] Semua query user lewat sqlc dengan `tenant_id` + integration test (testcontainers, `make test-integration`): lifecycle user, isolasi tenant, kegagalan auth, rotasi secret, tenant nonaktif, CLI
- [x] Build M1 live di dev (`551135ce7405`). Catatan: push ke `main` tidak memicu Jenkins otomatis, build dijalankan manual (perlu webhook atau Poll SCM)
- [x] Tenant Narrativ dev (NV-01)
- [x] NV-02, NV-03 (narrativ-backend#80)
- [x] **Selesai jika:** Narrativ dev bisa upsert user dan memberi token, dan token itu bisa memanggil `GET /me`. Diuji end-to-end di dev (login → `POST /chat/token` → `GET /v1/me` 200; api key salah → 401)

### M2: Messaging inti ✅

- [x] Channel: DM (server API, `dm_key`, satu per pasangan, aman saat dibuat bersamaan), group (client dan server API), detail, ubah nama/avatar, tambah/keluarkan/keluar member, owner pindah otomatis saat keluar, group berakhir saat anggota terakhir keluar, freeze/unfreeze. Peran owner/admin/member (D37)
- [x] `GET /channels`: urut aktivitas terbaru, `last_message`, `dm_peer`, unread, cursor paging, `updated_since` (termasuk channel yang ditinggalkan)
- [x] Kirim pesan REST: satu transaksi seq + pesan + outbox, idempotensi `client_msg_id` (termasuk kiriman bersamaan), validasi teks 4000 karakter. Lampiran dan reply ditolak dengan pesan jelas sampai M5
- [x] `GET /messages?before_seq=` dan `GET /sync?since_rev=` (kursor = `rev_seq` pesan terakhir yang dikembalikan, D38)
- [x] Pesan sistem untuk perubahan channel (D36) dan `POST /server/channels/{id}/messages`
- [x] `POST /channels/{id}/read` (hanya maju, dijepit sampai akhir channel) dan perhitungan unread (D35)
- [x] Integration test (testcontainers, `-race` hijau): DM, unread, group dan izin, riwayat pengumuman, paging dan `updated_since`, isolasi tenant (13 operasi lintas tenant semuanya 404), pengiriman paralel (seq 1..N tanpa celah, satu pesan per `client_msg_id`, satu DM per pasangan). Diuji juga lewat pgbouncer lokal
- [x] NV-04 (disederhanakan, D32), NV-05 (narrativ-backend#82; contract test Java ↔ Go lolos)
- [x] Build M2 live di dev (`eadceb835267`) dan narrativ-backend#82 di-merge (`d82154d6ab86`)
- [x] **Selesai jika:** dua user Narrativ dev bisa DM dan group lewat REST, history dan sync benar, test hijau. Diuji end-to-end di dev dengan dua akun (27 pemeriksaan lolos): buka DM dari dua sisi, kirim dan kirim ulang, unread, history, sync, read, balas, token palsu ditolak, group (buat, ganti nama, izin member, owner pindah saat keluar, group berakhir)

### M3: Realtime ✅

- [x] WS endpoint `/v1/ws`: auth di frame pertama (timeout 10 detik → 4001), `protocol_version`, ping/pong, idle 60 detik → 4008, batas frame 64 KB → 1009, token kedaluwarsa menutup koneksi tepat saat `exp` (4004), maksimal 10 koneksi per user di semua pod (4029)
- [x] Hub per pod: sesi per user (multi device), satu langganan Redis per user yang terkoneksi, event tidak dikirim balik ke koneksi pengirim (D39)
- [x] `message.send` via WS dengan `ack`/`error` per `id`, idempotent, publish setelah commit (D40). Kirim lewat REST ikut diumumkan ke semua perangkat pengirim
- [x] Event `message.new`, `channel.added/updated/removed`, `member.added/removed`, `read.updated`, termasuk pengumuman pesan sistem, dengan urutan yang menjaga client tidak menerima pesan untuk channel yang belum dikenalnya
- [x] Typing (throttle 1/detik per channel, kedaluwarsa 6 detik, berakhir saat koneksi putus) dan presence (D43, `GET /v1/presence`)
- [x] Rate limit per user, per app, per koneksi (D44), REST membalas `429` dengan `Retry-After`
- [x] Shutdown rapi: `/readyz` gagal, jeda drain, lalu semua WebSocket ditutup 1012 (`RegisterOnShutdown`)
- [x] Pemantauan langganan Redis (D41), readiness (D46), metrics `marpokat_ws_*`
- [x] CORS (D42)
- [x] INF-07 (timeout ingress 3600 detik, sudah di `apps/marpokat-dev/ingress.yaml`)
- [x] Prototipe klien web `examples/web-chat` (sekaligus acuan aturan client di spec) dan spec `events.md` / `openapi.yaml` diselaraskan
- [x] Test (`-race`, 3 kali berulang hijau): autentikasi (13 kasus), pengiriman lintas dua pod, typing dan read, presence (termasuk kedaluwarsa tanpa disconnect), event channel, rate limit, heartbeat, token, batas koneksi, pod hilang, Redis terputus, rate limiter Redis (tepat 10 dari 60 serentak), CORS. Diuji juga dengan dua proses `marpokat api` sungguhan lewat pgbouncer: pesan lintas proses 13 ms, SIGTERM → jeda drain 2 detik → 1012, sync, CORS preflight
- [x] Build M3 live di dev (`f42db1467dc8`)
- [x] **Selesai jika:** dua browser chat realtime lewat 2 replika pod, kill satu pod lalu chat tetap jalan setelah reconnect + sync. Lolos sebagai test otomatis (`TestWebSocketSurvivesAPodGoingAway`) dan dengan proses sungguhan. Di dev diuji lewat `wss` melalui Cloudflare dengan dua akun: pesan realtime 61 ms, typing, read, presence online/offline, CORS. (`kubectl delete pod` belum dicoba dari laptop karena cluster tidak terjangkau)

### M4: Worker dan push 🟡

- [x] Relay outbox (D47) + pembersih outbox (event terkirim lebih dari 7 hari dihapus, per 5000 baris)
- [x] Topologi RabbitMQ (D48): exchange, queue kerja `push`, retry bertingkat, DLQ, dideklarasikan otomatis oleh publisher dan consumer. Queue `webhook` dan `media` menyusul bersama consumer-nya (M8, M5): queue tanpa consumer hanya menumpuk
- [x] Dedup per perangkat (D49) dan consumer yang tahan putusnya broker (menyambung ulang, mendeklarasikan ulang, lanjut)
- [x] `POST/DELETE /v1/devices` (D51)
- [x] Consumer push (D49, D50): hanya member offline yang tidak mute dan belum membaca, Android dan Web Push, token tidak valid dihapus
- [x] Metrics (D52) dan statistik panjang queue
- [x] Test (`-race`, 3 kali berulang hijau, RabbitMQ/Postgres/Redis sungguhan lewat testcontainers, Google palsu): push ke member offline dengan semua aturan penerima dan isi payload, tanpa kredensial dilewati, token tidak valid dihapus, retry hanya untuk yang gagal (perangkat lain tidak diulang), DLQ setelah retry habis, event yang dipublikasikan sebelum consumer ada tidak hilang, event lama dilewati, event yang sama dua kali dikirim sekali, relay urut dan mencatat kegagalan, pembersih, API perangkat, FCM (kode status → klasifikasi, token akses di-cache dan diganti setelah 401)
- [x] Skenario inti: RabbitMQ dimatikan (`rabbitmqctl stop_app`) saat chat berjalan, dinyalakan lagi: 3 push terkirim, masing-masing sekali, outbox kosong (`TestPushSurvivesTheBrokerBeingDown`)
- [x] Diuji dengan proses `api` dan `worker` sungguhan lewat pgbouncer lokal: relay, queue, metrics, backlog lama dilewati sebagai stale
- [ ] Build M4 live di dev
- [ ] NV-07: kredensial FCM project Firebase Narrativ dev disimpan di tenant (langkahnya di bawah tabel integrasi)
- [ ] **Selesai jika:** matikan RabbitMQ saat mengirim pesan, nyalakan lagi, push tetap terkirim tanpa duplikat. Sudah lolos sebagai test otomatis; tinggal uji di dev dengan token perangkat sungguhan

### M5: Media dan reaction 🟡

- [ ] INF-04: bucket dan user selesai; sisa aturan CORS bucket untuk upload langsung dari browser (PUT presigned lintas origin)
- [x] `POST /attachments` + `complete`, validasi mime/ukuran, presigned PUT/GET
- [x] Consumer media: thumbnail gambar/video + blurhash, status `ready`, event `message.updated`
- [x] Pesan dengan attachment: milik pengirim, sudah di-`complete` (`processing` boleh, `ready` menyusul lewat `message.updated`)
- [x] Reaction: tambah/hapus, `reaction_counts`, event `reaction.added/removed` dengan `mine` per penerima
- [x] Edit dan hapus pesan (pengirim; owner/admin group boleh hapus), event `message.updated/deleted`
- [x] Reply (`reply_to_id`) dengan cuplikan pesan asal
- [x] Job pembersih attachment yang tidak pernah dipakai (grace 24 jam) dan milik pesan terhapus
- [x] NV-06: `GET /v1/server/channels/dm` di Marpokat + freeze/unfreeze di narrativ-backend (PR #83, merge setelah M5 jalan di dev)
- [x] **Selesai jika:** foto + reaction + edit + hapus + reply + putus koneksi dari klien referensi (`examples/web-chat/test.mjs`, dua klien) tampil sama di keduanya setelah reconnect + sync. Lolos terhadap api + worker + MinIO sungguhan; tinggal uji di dev

### M6: SDK JS + Narrativ web ✅

- [x] Scaffold `sdk-js` (workspace npm, TypeScript strict, Vitest, tsup), tipe di-generate dari `spec/openapi.yaml`
- [x] `@rizkysiregar/marpokat-core`: `MarpokatClient` (`tokenProvider`), reconnect backoff + jitter, ping, refresh token
- [x] State channel dan pesan, antrean pending + retry idempotent (`send/retry/discard`), deteksi celah `rev_seq` → sync
- [x] Upload attachment (presigned) dengan progress
- [x] `@rizkysiregar/marpokat-react`: `useChannels`, `useChannel`, `useMessages`, `useSendMessage`, `useMessageActions`, `useTyping`, `useUnreadCount`
- [x] Cache IndexedDB untuk daftar channel dan 50 pesan terakhir per channel
- [x] Publish ke GitHub Packages (`@rizkysiregar/marpokat-core` dan `-react` 0.1.1)
- [x] NV-08: narrativ-web-client PR #16
- [x] **Selesai jika:** chat dipakai internal di Narrativ web dev. Sudah terbukti: core terhadap server sungguhan (`sdk-js/packages/core/e2e/live.mjs`, dua klien, termasuk putus koneksi); 22 + 4 tes unit

### M7: SDK Android + Narrativ Android 🟡

- [x] Scaffold `sdk-android` (modul `core` Kotlin/JVM murni + modul `android`; kotlinx.serialization, OkHttp, Room)
- [x] Koneksi WS + reconnect + ping; `AppLifecycleConnector` (disconnect di background, reconnect di foreground)
- [x] State sebagai `StateFlow<ChatState>`, cache Room (channel + 50 pesan terakhir), antrean kirim idempoten, sync
- [x] Upload attachment dengan progress, reaction, edit, hapus, read, typing
- [x] Registrasi push token dan `MarpokatPush.parse` untuk payload FCM
- [x] Publish ke GitHub Packages (Maven), `marpokat-core` dan `marpokat-android` 0.1.0
- [x] NV-09: narrativ-android PR #59 (diuji di perangkat: chat jalan)
- [x] Chat lintas web ↔ Android di Narrativ dev: diuji 2026-11-22 malam, jalan
- [ ] **Selesai jika:** chat lintas web ↔ Android di Narrativ dev, push membuka ruang chat yang benar. Sudah terbukti: core terhadap server sungguhan (`LiveServerTest`, dua klien); 22 tes klien + 6 tes OkHttp/MockWebServer + 5 tes push. Cache Room belum punya tes instrumented

### M9: Website 🟡

~~Satu aplikasi Laravel (frontend saja) untuk landing page, pendaftaran, manajemen proyek, dan admin platform.~~ Diganti
2026-11-24 (D68): dua aplikasi Vue murni frontend, **portal** (pengguna) dan **admin** (internal), di repo `marpokat-web`.
Semua data dan aturan lewat Console API, yang dibuka ke browser secara same-origin (D69–D73).

- [x] Kontrak `spec/console.yaml` (auth, akun, proyek, push, audit, admin)
- [x] Go: migrasi 00004 (accounts, account_tokens, console_sessions, console_audit, tenants.account_id), paket `internal/console`, listener terpisah, CLI `account promote`, bersih-bersih sesi kedaluwarsa
- [x] Tes: unit (password, e-mail, slug, MIME, SMTP palsu, konfigurasi) dan integrasi terhadap Postgres + Redis nyata (alur daftar → verifikasi → proyek → rotasi, batas, isolasi antar akun, admin)
- [x] Gitops dev: port dan Service `marpokat-console` (mati sampai `CONSOLE_ADDR` diisi)
- [x] SMTP: akun yang sama dengan Narrativ (`monostacklabs.studio@gmail.com`); password aplikasinya disimpan di secret, bukan di repo
- [x] ~~Repo terpisah `marpokat-web` (Laravel 12, Blade, Tailwind 4, FrankenPHP; tanpa database): landing, docs, daftar/masuk/lupa password, konfirmasi e-mail, proyek (secret tampil sekali, push, pengaturan, matikan), akun (sesi, audit), admin; 36 tes; CSP ketat~~ Dihentikan (D68). Tetap dipakai sebagai acuan perilaku dan teks: halaman, pesan error, Turnstile, Terms/Privacy, tema avocado
- [x] ~~Jenkinsfile + manifest `homelabs-gitops/apps/marpokat-web-dev` untuk Laravel~~ Diganti tugas gitops di bawah

**Go (Console API untuk browser, D69–D73):**

- [x] `login` men-set cookie `__Host-mp_session` (token tidak lagi di body), `logout` dan sesi yang sudah berakhir menghapusnya; middleware membaca sesi dari cookie
- [x] Cek `Origin` + `Content-Type: application/json` untuk request yang mengubah data; semua jawaban `no-store` + `nosniff`
- [x] Scope sesi `portal`/`admin` (migrasi 00005); login admin hanya dari `CONSOLE_ADMIN_ORIGINS` dan hanya untuk platform admin; `/admin/*` butuh sesi admin (D71)
- [x] Verifikasi Turnstile di `register` dan `forgot-password` (`TURNSTILE_SECRET_KEY`, gagal tertutup, D72)
- [x] Alamat pengunjung dari `CF-Connecting-IP` di belakang proxy tepercaya (`CONSOLE_TRUSTED_PROXIES`, D73). Limit per alamat untuk `register`, `forgot-password`, `login` sudah ada; `resend-verification` dibatasi per akun
- [x] `X-Console-Key`, `X-Client-IP`, dan `CONSOLE_SERVICE_KEY` dihapus (tanpa masa transisi: Laravel tidak pernah menyala di dev)
- [x] `spec/console.yaml` 0.2.0: bagian "Not public" ditulis ulang (cookie, origin, scope, Turnstile), field `turnstile_token`, server URL publik
- [x] Tes: unit (guard Origin/Content-Type, IP dari proxy tepercaya vs tidak, atribut cookie, Turnstile dengan Cloudflare palsu termasuk tidak terjangkau, config) dan integrasi (cookie login/logout/sesi berakhir, request lintas situs, scope admin, Turnstile)
- [x] Hapus proyek dengan masa tenggang + pulihkan, purge di worker termasuk file MinIO (D75); tes isolasi: proyek lain dari akun yang sama tidak tersentuh
- [x] API admin luas (D76): detail akun/proyek, keluarkan sesi, audit platform, pencarian, deret harian + proyek tersibuk; audit promote/demote
- [x] `DELETE /me/sessions/{id}` (keluarkan satu perangkat), saklar `CONSOLE_REGISTRATION_OPEN` + `GET /status`
- [x] `spec/console.yaml` 0.3.0

**Frontend (repo `marpokat-web`, D74):**

- [x] Monorepo pnpm: `apps/portal`, `apps/admin`, `packages/console-client` (generate dari `spec/console.yaml`), `packages/theme` (port `marpokat-theme.css` + uji kontras) (marpokat-web#3)
- [x] Portal: landing, Terms, Privacy (draft), docs (getting started, push, batas). Docs lanjutan (server API, SDK JS/Android lebih rinci) menyusul
- [x] Portal: daftar (Turnstile), masuk, lupa/reset password, konfirmasi e-mail, kirim ulang verifikasi
- [x] Portal: proyek (buat, daftar, detail, rotasi secret, kredensial push, izin klien, matikan/nyalakan), akun (profil, password, sesi, aktivitas) (marpokat-web#4). Sisa di portal: tombol hapus/pulihkan, keluarkan satu sesi, halaman daftar saat pendaftaran ditutup
- [ ] Admin: masuk, statistik, daftar akun (paging, nonaktifkan), daftar proyek (cari, nonaktifkan) selesai; sisa: dashboard dengan chart, detail akun/proyek, audit platform, pencarian (API sudah ada, D76)
- [x] Image nginx statis per app (UID 10001, read-only, CSP tanpa `unsafe-inline` dengan hash yang dihitung saat build, `frame-ancestors 'none'`, halaman `no-cache`, `config.json` `no-store`, aset ber-hash cache 1 tahun, `/healthz`)
- [x] Tes: Vitest (69 tes), ESLint + Prettier + type check di image `test`; e2e di Chrome headless terhadap container dan Console API lokal (18 langkah, tanpa error console/CSP), dijalankan manual
- [ ] Playwright di CI untuk alur utama portal dan admin
- [x] Jenkinsfile (lint, typecheck, test, build dua image `marpokat-portal` dan `marpokat-admin`, tulis tag keduanya ke gitops)
- [ ] Menutup pendaftaran: saklar server `CONSOLE_REGISTRATION_OPEN` + `GET /status` selesai; sisa: portal membaca statusnya

**Gitops dan dev:**

- [x] `apps/marpokat-web-dev` ditulis ulang: deployment portal + admin, `config.json` dari ConfigMap, Ingress host portal dan admin dengan path `/console/v1` → `marpokat-console` (D69) (homelabs-gitops#18)
- [x] Configmap Go: `CONSOLE_ADMIN_ORIGINS` (homelabs-gitops#18)
- [ ] Secret `TURNSTILE_SECRET_KEY` + site key di `portal-config.json`, `MAIL_*`; `CONSOLE_SERVICE_KEY` dan `marpokat-web-secrets` dihapus
- [ ] Cloudflare Tunnel host dev (portal `app-dev-marpokat`, admin `admin-dev-marpokat`), Argo app, pipeline Jenkins `marpokat-web`, akses GHCR package `marpokat-portal` dan `marpokat-admin`
- [ ] **Selesai jika:** orang bisa mendaftar di portal, membuat proyek, mendapat API key dan secret, dan memakainya di aplikasi; admin bisa melihat dan menonaktifkan akun/proyek dari host admin, dan hanya platform admin yang bisa masuk ke sana

### M8: Hardening dan launch prod 🟡

Hardening di dev (2026-11-23), sebelum prod:

- [x] Probe 1980 request rusak (NUL, unicode aneh, JSON bersarang 30 ribu level, id panjang, kredensial salah) ke setiap route di spec: satu bug, byte NUL di parameter jalur menjadi 500 → dijaga di tengah (400) untuk path, query, body JSON dan frame WebSocket
- [x] Rate limit REST umum per pemanggil (user: 60 burst, 20/detik; backend tenant: 200 burst, 100/detik), di luar batas kirim pesan dan upload yang sudah ada
- [x] Audit: query berkunci tenant (sisanya berkunci UUID yang sudah dimuat lewat query bertenant), batas body 64 KB dan frame WS, JWT dikunci HS256, tidak ada secret di log
- [x] Batas koneksi WebSocket per user: **sudah ada** (10 per user di semua pod, ditolak 4029, ada tes). Catatan "belum ada" sebelumnya keliru
- [x] Throttle kredensial salah per alamat (API key tidak dikenal atau token palsu: 30 per 10 menit, lalu 429 sampai jendela habis, termasuk di WebSocket); token kedaluwarsa dan user belum dibuat tidak dihitung
- [x] URL presigned PUT yang dipakai lagi setelah `complete`: worker memeriksa ulang ukuran objek, yang berubah ditandai gagal dan dihapus
- [x] Redis: `REDIS_USERNAME` (user ACL yang hanya menjangkau key `marpokat-prod:*` dan tanpa perintah berbahaya), diuji dengan alur chat penuh. Menerapkannya di VM adalah langkah Anda (INF-12)


- [ ] Webhook ke tenant (event pesan, signature HMAC, retry + DLQ)
- [ ] Load test k6: 1000 koneksi WS serentak + 50 pesan/detik, catat p95 latensi kirim → terima
- [ ] Review keamanan: isolasi tenant, validasi input, batas ukuran, rate limit, rotasi secret
- [ ] INF-12, INF-13, INF-14, INF-15
- [ ] Tenant `narrativ` prod + kredensial FCM prod, NV-10
- [ ] Dokumentasi: `docs/` integrasi tenant (alur token, server API, event, SDK quickstart)
- [ ] **Selesai jika:** chat rilis ke user Narrativ prod

---

## 13. Di luar MVP

~~Developer console web~~ (masuk M9), billing/metering, UI kit, thread, search full-text, E2E encryption, voice/video call, channel publik,
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
- **Cache tenant 30 detik per pod.** ~~Menonaktifkan tenant atau merotasi secret baru berlaku setelah paling lama 30 detik; WebSocket terbuka hidup sampai token habis.~~ Sejak D77, perubahan dari website langsung berlaku di semua pod dan WebSocket tenant yang dimatikan ditutup. Sisa: perubahan lewat CLI masih menunggu TTL, dan WebSocket yang tersambung dengan token dari secret lama tetap hidup setelah rotasi sampai token-nya habis (rotasi tidak menutup koneksi).
- **Redis mati berarti realtime berhenti**, bukan chat: pesan tetap tersimpan lewat REST dan sync. Saat Redis kembali, semua sesi ditutup 1012 dan client sync (D41).
- **Cloudflare menolak User-Agent bawaan beberapa library** (kode 1010, mis. `Python-urllib`). Skrip tes memakai User-Agent sendiri. Browser, OkHttp, dan klien Java Narrativ tidak terpengaruh.
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
| 2026-11-22 | M1 selesai: tenant, auth, user API; integrasi Narrativ dev teruji end-to-end   |
| 2026-11-22 | M2 dikerjakan: D32–D38, channel/pesan/sync/unread, NV-04/05                    |
| 2026-11-22 | M2 selesai: diuji dengan dua akun di dev                                       |
| 2026-11-22 | M3 dikerjakan: D39–D46, WebSocket, fan-out Redis, typing, presence, rate limit, CORS |
| 2026-11-22 | M3 selesai: diuji lewat wss di dev. M4 dikerjakan: D47–D53, relay outbox, RabbitMQ, push FCM, perangkat |
| 2026-11-23 | Pengembangan MVP ditutup: chat diuji di Narrativ web + Android dev. M8: manifest prod `apps/marpokat` disiapkan |
| 2026-11-24 | M9 diubah: Laravel dihentikan, website menjadi dua app Vue murni frontend (portal + admin). D62/D63 diganti, D68–D74 ditambahkan, stack website di bagian 3, diagram website di bagian 4, tugas M9 ditulis ulang. Console API untuk browser selesai di Go (cookie, Origin, scope admin, Turnstile, IP) |
| 2026-11-24 | M9: gitops portal + admin (homelabs-gitops#18), monorepo Vue (marpokat-web#3). D71: host admin publik tanpa Cloudflare Access. D74: VeeValidate diganti `useForm` + Zod, Reka UI dan Playwright menyusul, Node 24 / TS 5.9 |
| 2026-11-24 | D75 (hapus proyek dengan masa tenggang 3 hari, purge di worker), D76 (API admin luas, chart harian, audit platform). Console API 0.3.0 |
| 2026-11-24 | D77: perubahan tenant diumumkan ke semua pod; WebSocket tenant yang dimatikan/dihapus ditutup 4003, cache API key dibuang |
| 2026-11-24 | D78 (label proyek, tanpa konsep environment), D79 (suspend/delete/delete now/restore proyek oleh admin, e-mail ke pemilik, purge 1 menit). Console API 0.4.0 |
| 2026-11-24 | D80: suspend/hapus akun oleh admin (langsung atau masa tenggang), hapus akun sendiri di portal (masa tenggang 3 hari), purge akun + proyeknya. Console API 0.5.0 |
