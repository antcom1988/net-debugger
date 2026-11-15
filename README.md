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
- Redaksi data sensitif HTTP dan ISO 8583.

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

Client API tidak dapat menentukan upstream; upstream berasal dari `backend_addr` connection. Hanya HTTP/HTTPS yang diizinkan dan URL bercredential ditolak. Header/nilai `Authorization`, cookie, token, password, secret, dan `api_key` direduksi dalam log. Capture body HTTP dibatasi 64 KiB.

## Query Logs

- `GET /api/logs` memuat history log persisten sebelum dashboard menerima SSE `/api/stream`.
- Refresh browser tidak menghapus log.
- **Clear Logs** memanggil `POST /api/logs/clear` dan hanya menghapus file `*.log` di `log_dir`.
- Log harian menggunakan format `<connection-name>-YYYY-MM-DD.log`.

## Chaos Control

Pilih **Connection** terlebih dahulu. Action di dropdown otomatis berubah sesuai **Connection Type**. Backend juga memvalidasi `POST /api/chaos-rules`, sehingga action yang tidak cocok tidak dapat dibuat lewat request API langsung.

| Connection type | Action yang tersedia |
|---|---|
| PostgreSQL, MariaDB, Redis | Inject Delay; Return Error Response; Drop Connection; Return Empty Result Set; Throttle Response Speed |
| Socket ISO 8583 | Inject Delay; Drop Connection |
| API HTTP/HTTPS | Delay Request/Response; Custom HTTP Response; Drop Request; Corrupt Response Body; Override Response Header |

### Batasan ISO 8583

ISO 8583 tidak menawarkan generic error packet atau empty result yang aman tanpa konteks transaksi. Karena itu Chaos Control ISO hanya menyediakan delay dan penutupan socket; proxy tidak membuat frame respons ISO fiktif.

### Action API

- **Delay Request / Response**: delay dapat diterapkan sebelum forward request atau sebelum respons dikirim; mendukung rentang random delay.
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

Untuk keamanan, nilai mentah DE2, DE35, DE36, DE45, DE52, DE53, dan DE102–DE128 tidak diekspos dalam log.

## API Dashboard

| Method | Endpoint | Keterangan |
|---|---|---|
| `GET` | `/api/logs` | History log persisten |
| `POST` | `/api/logs/clear` | Hapus file `*.log` dalam log directory |
| `GET` | `/api/stream` | Event log real-time (SSE) |
| `GET`, `POST` | `/api/chaos-rules` | Baca/simpan Chaos rules |
| `GET`, `POST`, `DELETE` | `/api/services` | Kelola proxy connection |

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
