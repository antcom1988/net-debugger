# Net Debugger (DaPoeR)

Net Debugger adalah proxy observability dan fault-injection berbasis Go untuk **PostgreSQL, MariaDB/MySQL, Redis, API HTTP/HTTPS**, dan **Socket ISO 8583**. Dashboard lokal menyediakan Query Logs persisten, pengelolaan connection, dan Chaos Control.

> Ditujukan untuk development, testing, dan troubleshooting; bukan pengganti gateway atau kontrol keamanan produksi.

## Fitur

- Proxy TCP untuk PostgreSQL, MariaDB/MySQL, dan Redis.
- Reverse proxy API HTTP/HTTPS dengan upstream tetap (*fixed upstream*).
- Socket ISO 8583 berdasarkan `packager/iso8583/iso_spec.xml`.
- Dashboard real-time melalui SSE di `http://localhost:9999`.
- Log file persisten; refresh dashboard tidak menghapus log.
- Dynamic connection dan warna label yang konsisten pada kartu/filter/badge.
- Chaos rules yang disimpan pada `chaos_rules.json`.
- Log request/response API dan field ISO 8583 ditampilkan lengkap untuk kebutuhan debugging.

## Requirements

- Go **1.26.4** (lihat `go.mod`)
- Service backend yang ingin diproxy

## Menjalankan

Dari root project:

```bash
go test ./... -count=1
go vet ./...
go build -o proxy.exe ./cmd/proxy
./proxy.exe
```

Atau untuk development:

```bash
go run ./cmd/proxy
```

Aplikasi membaca `config.json` dari current working directory. Dashboard default tersedia pada `http://localhost:9999`.

## Konfigurasi

Nama connection adalah identitas untuk log, filter, warna label, dan Chaos Control. `type` menentukan parser serta action Chaos yang valid.

```json
{
  "retention_days": 7,
  "log_dir": "./logs",
  "services": {
    "redis_dev": {
      "type": "redis",
      "listen_port": 16379,
      "backend_addr": "localhost:6379",
      "label_color": "#dd0e0e",
      "enabled": true
    },
    "payments_iso": {
      "type": "iso8583",
      "listen_port": 8990,
      "backend_addr": "127.0.0.1:8989",
      "label_color": "#63f83a",
      "enabled": true
    },
    "partner_api": {
      "type": "api",
      "listen_port": 18088,
      "backend_addr": "https://api.example.test",
      "label_color": "#f83acf",
      "enabled": true
    }
  },
  "web": { "port": 9999, "enabled": true }
}
```

| Type | Kegunaan | Backend |
|---|---|---|
| `postgres` | PostgreSQL wire protocol | `host:port` |
| `mariadb` | MariaDB/MySQL wire protocol | `host:port` |
| `redis` | Redis RESP | `host:port` |
| `api` | HTTP/HTTPS reverse proxy | URL HTTP(S), atau `host:port` yang diperlakukan sebagai HTTP |
| `iso8583` | Socket ISO 8583 | `host:port` |

### API security

Client API tidak dapat menentukan upstream; upstream berasal dari `backend_addr` connection. Hanya HTTP/HTTPS yang diizinkan dan URL bercredential ditolak. Log request API menyimpan URL, semua header, body, serta response body hingga batas capture 64 KiB. Karena log dapat berisi credential atau data transaksi, batasi akses ke dashboard dan direktori log.

## Query Logs

- `GET /api/logs` memuat history log persisten sebelum dashboard menerima SSE `/api/stream`.
- Refresh browser tidak menghapus log.
- **Clear Logs** memanggil `POST /api/logs/clear` dan hanya menghapus file `*.log` di `log_dir`.
- Log harian menggunakan format `<connection-name>-YYYY-MM-DD.log`.

## Chaos Control

Pilih **Connection** terlebih dahulu. Action di dropdown otomatis berubah sesuai **Connection Type**. Backend juga memvalidasi `POST /api/chaos-rules`, sehingga action yang tidak cocok tidak dapat dibuat lewat request API langsung.

| Connection type | Action yang tersedia |
|---|---|
| PostgreSQL, MariaDB, Redis, Socket ISO 8583, API HTTP/HTTPS | **Delay** (Delay Request dan Delay Response); serta action lain yang kompatibel dengan masing-masing connection type |

### Batasan ISO 8583

ISO 8583 tidak menawarkan generic error packet atau empty result yang aman tanpa konteks transaksi. Karena itu Chaos Control ISO hanya menyediakan delay dan penutupan socket; proxy tidak membuat frame respons ISO fiktif.

### Action API

- **Delay**: tersedia di semua Connection Type. Isi **Delay Request (milliseconds)** untuk menahan request sebelum diteruskan ke backend dan/atau **Delay Response (milliseconds)** untuk menahan respons backend sebelum diteruskan ke client. Kedua nilai dapat digunakan bersamaan; tidak ada random delay.
- **Custom HTTP Response**: mengembalikan status/body yang diatur tanpa menghubungi upstream.
- **Drop Request**: menghentikan request sebelum upstream.
- **Corrupt Response Body**: memodifikasi body respons upstream untuk resilience test.
- **Override Response Header**: menambahkan/menimpa header respons, misalnya `Retry-After`.

## Socket ISO 8583

Parser ISO memuat field specification dari:

```text
packager/iso8583/iso_spec.xml
```

Karakteristik:

- TCP frame dengan prefix length 2-byte big-endian;
- payload ASCII, MTI, dan bitmap ASCII-hex;
- field fixed-width, LLVAR, dan LLLVAR;
- buffering untuk frame TCP terfragmentasi;
- request dicatat saat frame lengkap diterima, tanpa menunggu socket persistent ditutup.

Semua field yang berhasil diparse, termasuk DE2, DE35, DE36, DE45, DE52, DE53, dan DE102–DE128, dicatat dengan nilai aslinya untuk debugging transaksi. Lindungi akses ke Query Logs dan folder log karena data tersebut dapat bersifat sensitif.

## API Dashboard

| Method | Endpoint | Keterangan |
|---|---|---|
| `GET` | `/api/logs` | History log persisten |
| `POST` | `/api/logs/clear` | Hapus file `*.log` dalam log directory |
| `GET` | `/api/stream` | Event log real-time (SSE) |
| `GET`, `POST` | `/api/chaos-rules` | Baca/simpan Chaos rules |
| `GET`, `POST`, `DELETE` | `/api/services` | Kelola proxy connection |

## Docker

Image menggunakan multi-stage build: Go Alpine hanya dipakai saat kompilasi, sedangkan runtime memakai **Alpine non-root**. Binary dibuat statis dan di-strip (`-s -w`); Alpine menyediakan `/bin/sh` sehingga container dapat diperiksa dengan `kubectl exec` saat troubleshooting.

```bash
docker build -t net-debugger:local .
docker run --rm --name net-debugger \
  -p 9999:9999 \
  -p 10000-30000:10000-30000 \
  -v net-debugger-config:/app/config \
  net-debugger:local
```

Image menyertakan `config.docker.json` sebagai `/app/config/config.json`; seluruh connection awalnya disabled agar tidak secara tidak sengaja melakukan koneksi ke host. Aktifkan connection yang diperlukan melalui dashboard atau mount konfigurasi sendiri:

```bash
docker run --rm \
  -p 9999:9999 \
  -v "$(pwd)/my-config.json:/app/config/config.json:ro" \
  net-debugger:local
```

Pada Docker Desktop, gunakan `host.docker.internal:<port>` di `backend_addr` untuk mengakses service di host. Untuk service dalam Docker network yang sama, gunakan nama service Docker, misalnya `postgres:5432` atau `redis:6379`.

`/app/config` menampung `config.json` dan `chaos_rules.json` dan dideklarasikan sebagai volume. `/app/logs` bersifat ephemeral di filesystem container; log akan hilang ketika container dibuat ulang. Jika `config.json` di-mount read-only, jangan mengubah connection atau Chaos rules dari dashboard karena aplikasi perlu menulis state tersebut.

## Validasi

```bash
gofmt -w internal/adapters/inbound/web_server.go internal/adapters/inbound/web_server_test.go
go test ./... -count=1
go vet ./...
go build -o proxy.exe ./cmd/proxy
git diff --check
```

## Arsitektur

```text
Client -> Connection Listener -> Protocol Parser -> Proxy Service -> Fixed Backend
                                       |                |
                                       +-> File Logs + SSE
                                       +-> Chaos Rules
```

- `internal/core/domain`: model konfigurasi, log, dan chaos rule.
- `internal/core/services`: proxy lifecycle, parser response, chaos evaluation, broadcast log.
- `internal/core/ports`: interface inbound/outbound.
- `internal/adapters/inbound`: TCP listener dan web dashboard.
- `internal/adapters/outbound`: parser protokol dan file logger.
