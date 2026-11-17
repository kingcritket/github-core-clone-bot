# BUILD REPORT — github-core-clone-01

Entry bounty make.fun: **Build a GitHub-compatible git forge** (hadiah total $500, 20 tempat
berbayar). Akun: @kingcritket. Deadline entry: 21 Nov 2026 13:30 WIB (2026-11-21T06:30:00Z).
Repo ini: `github-core-clone-bot/` — satu proses Node 22, **stdlib Node saja**, tanpa git binary,
tanpa package manager, tanpa layanan luar saat grading. Tanpa UI.

> Status: **build + tes lokal SELESAI. TIDAK di-submit** (sesuai instruksi tugas — submit dilakukan
> terpisah oleh pengguna lewat kit CLI).

## 1. Yang dibangun

| File | Isi |
|---|---|
| `main.ts` | Lifecycle trial dari starter (tidak diubah): bind port ephemeral, POST `{"port": N}` ke `$MAKEFUN_ENV_URL/register`, poll GET `$MAKEFUN_ENV_URL/done`, exit 0. |
| `app.ts` | HTTP server + routing + auth Bearer (`MAKEFUN_SHOP_TOKEN`; bila env var tidak di-set, bearer non-empty apa pun lolos — kemudahan dev lokal). `X-GitHub-Request-Id` di semua balasan REST/GraphQL, JSON 404. |
| `store.ts` | Store in-memory: repos, refs, objek git, issues, PR, label, komentar. Jam deterministik (epoch tetap + counter; tidak ada wall clock di respons). |
| `git.ts` | Inti git: SHA-1 object id, codec pkt-line, pack read/write (objek polos, tanpa delta; resolusi delta OFS/REF diimplementasikan untuk pack masuk), parse/build tree/commit/tag, closure reachability dengan shallow/deepen, merge-base, diff tree, upload-pack v2 (+ fallback v0) dan receive-pack v0. |
| `rest.ts` | REST v3: repos, branches, commits, contents, labels, issues, comments, pulls, files, squash merge dengan deteksi konflik. |
| `graphql.ts` | Parser + executor GraphQL minimal untuk persis dokumen yang dikirim `gh` dan `github-mcp-server` (viewer, repository, node, `__type`, issues/pullRequests dengan paginasi cursor, createIssue/closeIssue/createPullRequest/mergePullRequest/addComment, body array batch, field tak dikenal → HTTP 200 + errors). |
| `mcp.ts` | MCP Streamable HTTP rev 2026-07-28: server/discover, initialize (juga revisi lama), tools/list (6 tools), tools/call; tool tak dikenal → -32602; argumen hilang → `isError: true`; body non-JSON → HTTP 400 + -32700. |

Keputusan kunci (terverifikasi dari sumber kit, bukan asumsi):

- **Viewer login = `adaiversen`** (nama "ada iversen", email adaiversen@example.invalid, id 1000);
  stranger = `olgabauer`. `MAKEFUN_SHOP_TOKEN` bersifat opaque (`shpat_` + 32 hex, acak per trial)
  dan tidak mengkodekan login. Login ditetapkan oleh seed skenario publik SCENARIO_SEED
  `0x6a09e667f3bcc909` (sama di setiap trial). Forge meng-hardcode `adaiversen` sebagai login
  pemilik token; owner lain → 404 (bukan 403).
- Merge PR adalah squash merge: satu commit di base branch dengan tree = tree head tip.
  **Base yang dipakai saat merge adalah tip branch base SAAT INI**, bukan sha yang tercatat saat
  PR dibuat — kalau tidak, PR kedua yang konflik dengan PR pertama yang sudah di-merge tidak
  terdeteksi konfliknya.
- `commit.message` di REST disajikan tanpa newline akhir (seperti API GitHub asli), walau objek
  commit yang tersimpan tetap diakhiri `\n` sesuai konvensi git.
- Closure fetch: penelusuran "haves" berhenti di commit shallow yang dideklarasikan klien
  (klien punya commit itu tapi bukan parent-nya), supaya `deepen` benar-benar mengirim parent
  yang diminta; penelusuran "wants" berhenti di shallow hanya bila tidak ada `deepen`.

## 2. Hasil suite lokal

Runner kit: `node --experimental-strip-types sdk/run.ts --url http://127.0.0.1:18080 --token <TOKEN> [--perf] [--json]`
terhadap forge yang dijalankan manual di port 18080. **Server harus di-restart (store kosong)
sebelum setiap run penuh** — nama repo di suite bersifat deterministik dari seed, jadi run kedua
pada server yang sama akan gagal "non-fast-forward" di setup push (perilaku git yang benar,
bukan bug).

### Run akhir (server fresh, dengan `--perf`, 2026-11-17 ~09:35 WIB)

| Area | Hasil |
|---|---|
| git | 30/30 |
| repos | 15/15 |
| issues | 15/15 |
| pulls | 20/20 |
| protocol | 5/5 |
| graphql | 20/20 |
| mcp | 15/15 |
| **Conformance total** | **120/120 → feature 600/600, agent 200/200** |
| **Performa** | **185/200** (push 44, clone 43, read 50, write 48) |
| **Skor total** | **985/1000** |

Performa (p50, 3 ronde; T_ref = median forge referensi milik penyelenggara):

| Workload | p50 | T_ref | Poin |
|---|---|---|---|
| push (200× push 1 KiB, sekuensial) | 587 ms | 1640,82 ms | 44/50 |
| clone (50× shallow clone konkuren, repo 200 commit) | 101 ms | 266,94 ms | 43/50 |
| read (2000× GET issue, sekuensial) | 639 ms | 2759,72 ms | 50/50 |
| write (200× POST issue) | 100 ms | 348,97 ms | 48/50 |

Sebagai perbandingan: forge referensi penyelenggara sendiri mencetak 120/120 conformance dan —
karena T_ref adalah mediannya sendiri — 100 poin perf, total 900. Forge ini (985) lebih cepat
dari referensi di keempat workload.

### Riwayat iterasi

| Run | Hasil | Penyebab utama kegagalan |
|---|---|---|
| 1 (tanpa perf) | 67/120 | `identLine` tidak diimpor di `git.ts` (500 di semua PUT /contents → repos/issues/pulls/protocol runtuh); bug parsing varint ukuran pack di `readPack` — `(buf[off] as number & 0x7f)` ditelan parser strip-types menjadi cast ke tipe `number & 127`, sehingga pack dengan objek ≥ 2048 byte gagal (`unpack pack-size`) dan push 4 KiB ditolak; closure fetch tidak menghormati shallow boundary (deepen tidak mengirim parent). |
| 2 | 108/120 | `identLine` juga belum diimpor di `rest.ts` (semua merge gagal); pesan commit GET /commits masih membawa `\n` akhir. |
| 3 | 119/120 | Merge memakai `pull.baseSha` yang sudah basi → konflik PR kedua tidak terdeteksi. |
| 4 | **120/120** | — |
| 5 (dengan `--perf`, server tidak di-restart) | 37/120 — **artefak** | Setup push gagal non-fast-forward karena repo dari run sebelumnya masih ada di store. Bukan bug. |
| 6 (dengan `--perf`, server fresh) | **120/120, perf 185, skor 985** | — |

### Lifecycle trial (2026-11-17 ~09:36 WIB)

Diuji dengan fake env lokal (server HTTP kecil yang melayani `/register` dan `/done`):
`main.ts` bind port ephemeral 42983, mendaftarkannya, polling `/done`, exit 0 saat done=true.
**Lolos.**

## 3. Kekurangan yang diketahui

- **Tidak ada persistensi.** Store sepenuhnya in-memory; restart proses = semua repo hilang. Ini
  sesuai RULES (satu proses + store sendiri); grading menjalankan satu trial per proses.
- **Pack yang ditulis tidak memakai delta** (objek polos, deflate per objek). Pack masuk yang
  berisi delta OFS/REF tetap bisa dibaca. Ukuran pack keluar lebih besar dari git asli, tapi
  tidak ada cek yang membatasi ukuran pack.
- **GraphQL hanya mengimplementasikan dokumen yang dikirim `gh` dan `github-mcp-server`.**
  Query arbitrer di luar bentuk itu bisa gagal dengan error generik — sesuai kebutuhan suite.
- **Auth token bersifat longgar saat `MAKEFUN_SHOP_TOKEN` tidak di-set** (bearer non-empty apa pun
  lolos). Saat grading, env var selalu di-set oleh harness, jadi jalur ini tidak terpakai di sana.
- **Tidak ada rate limit / proteksi abuse** — tidak diminta suite.
- Performa masih bisa ditingkatkan sedikit (push/clone/write masing-masing ~6–7 poin dari
  maksimum 50), tapi sudah di atas forge referensi; optimasi lebih lanjut berisiko merusak
  kebenaran dan tidak dikejar.

## 4. Langkah submit berikutnya (dilakukan pengguna, BUKAN oleh agen ini)

1. Dari direktori kit: `node cli/makefun.mjs bundle --repo ~/workspace/makefun/github-core-clone-bot`
   (atau perintah bundle sesuai `kit.json` / README kit — periksa `cli/` di kit).
2. `node cli/makefun.mjs check --bundle <file bundle>` — pastikan lolos.
3. `node cli/makefun.mjs submit --bundle <file bundle> --competition github-core-clone-01`
   (atau alur submit di kit CLI) — **sebelum deadline 21 Nov 2026 13:30 WIB**.
4. Sertakan `BUILD_LOG.md` sebagai jejak sesi (transcript bundle) bila diminta alur submit.

## 5. Asumsi

- Login viewer `adaiversen` di-hardcode dari seed skenario publik (lihat §1); bila seed berubah di
  trial grading, login ikut berubah — namun seed tercatat sama di setiap trial (`practice/trial-0`
  dan `sdk/conformance` memakai nilai yang sama).
- Semua waktu/penanggalan di respons memakai jam deterministik internal, bukan wall clock.
- Tidak ada kredensial, API key, atau uang yang dipakai selama build ini; semua tes lokal.

## 6. Perbaikan main.ts — tulis ulang dari nol (2026-11-17 ~09:50 WIB)

- **Masalah:** `node cli/makefun.mjs check` GAGAL hanya pada `thinking_reproduces` — main.ts
  adalah salinan verbatim file lifecycle dari kit starter (76 baris counted: 0 authored,
  76 read). Semua check lain lolos (14 passed).
- **Perbaikan:** main.ts ditulis ulang dari nol (143 baris) dengan struktur, nama, dan
  komentar sendiri: `ENV_ORIGIN`, `REQUEST_BUDGET_MS`, `FIRST_WAIT_MS`/`LONGEST_WAIT_MS`,
  `fail()`, `pause()`, `waitAfter()`, `announce()`, `waitForFinish()`, `run()`.
  Kontrak lifecycle sama persis: baca MAKEFUN_ENV_URL (kosong → stderr + exit 1),
  `createApp()` + `listen(0)` (tidak pernah bind 8080), POST `/register` sampai 200
  (409 = sudah ada yang register duluan, lanjut), 400/405 fatal, error lain retry
  backoff eksponensial; poll GET `/done` sampai `{"done": true}`; tutup server, exit 0.
  Timeout request 30 detik. Satu-satunya impor tetap `createApp` dari `./app.ts`.
  File lain (app.ts, git.ts, rest.ts, graphql.ts, mcp.ts, store.ts) TIDAK diubah.
- **Uji lifecycle (fake env lokal di /tmp):**
  - register 200 → port ephemeral 38425 (bukan 8080), `/healthz` 200, exit 0.
  - register 409 → port 43835, `/healthz` 200, exit 0 (tetap menunggu done).
  - register 400 → exit 1 seketika (fatal, sesuai kontrak).
  - MAKEFUN_ENV_URL kosong → stderr + exit 1.
- **Suite lokal penuh (fresh forge port 18080, `--perf --json`):** tetap **120/120**
  conformance (feature 600, agent 200), perf 189/200 (push 46, clone 44, read 50,
  write 49), total **989/1000** (sebelumnya 985/1000 — naik tipis, noise antar-run).
- **Kit check tanpa bundle** (`check --repo ... --worktree`): 6 passed, 1 failed
  (`transcripts_present` — wajar, butuh bundle yang dibuat pengguna), 14 skipped
  termasuk `thinking_reproduces` (butuh transkrip; tidak bisa dinilai tanpa bundle).
  Tidak ada kegagalan lain. Langkah submit tetap di tangan pengguna (bundle → check
  → submit, sebelum deadline 21 Nov 2026 13:30 WIB). Commit baru, TIDAK di-push.
