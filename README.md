# PS3 Online Storage Server

OpenPak's PS3 cloud save storage: the firmware's `ubs` service behind the save-data utility's "Online Storage"
(Sony offered it to PlayStation Plus members).

| | |
| --- | --- |
| Network | PlayStation 3 |
| Role | `/ubs/sec/set_save_data`, `get_save_data`, `get_save_data_info_list`, `delete_save_data`, `is_supported_title`, `notify_downloaded`; the signed discovery file `/np/resource/ubstorage/ubstorage.xml`; save icons at `/ubs/icon/{id}` |
| Depends on | [`account`](https://github.com/openpak/account) (account core), [`np-common`](https://github.com/openpak/np-common) (vendored), [`saves`](https://github.com/openpak/saves) (core save index and bytes, v0.5.0+) |
| Used by | [`np-account`](https://github.com/openpak/np-account) (forwards `/ubs/` and `/np/resource/ubstorage/` via `NP_UBS_URL`) |
| Image | `ghcr.io/openpak/np-ubs` |
| Port | 8536 |
| Licence | AGPL-3.0-only |

The wire format is static RE of firmware 4.93 and has not been confirmed against a console; see [docs/protocol.md](docs/protocol.md).

## Limits

| Limit | Default | Env |
| --- | --- | --- |
| Storage per account | the saves allowance, shared with the Switch cloud saves and every other network (1 GiB by default); `5f` when it is full | `SAVES_ALLOWANCE_BYTES` on saves |
| One save (data part) | 1 GiB, the PS Plus per-account size on PS3 | `NP_UBS_SAVE_MAX` |
| Saves per account | 1000 | (constant) |
| Icon | 4 MiB | (constant) |

Each account has one folder per save type (`ps3`, `ps1`, `ps2`, `psp`, `minis`); a save is replaced when the same name is
uploaded again (saves keeps one version per name for platform `ps3`). np-ubs holds no data of its own: it is an adapter
over the core `saves` service's neutral blob routes (platform `ps3`, title = folder type, name = save directory), so a
PS3 save appears in the same index as a Switch backup, counts in the same allowance and can live on the player's own
S3/WebDAV storage. Uploads and downloads stream and never hold a whole save in memory.
There is no once-per-day download limit: `downloadable-date` is always "now".

## Run

```sh
cp example.env .env   # fill in required values
set -a; . ./.env; set +a
GOWORK=off go run -mod=vendor .
```

Tests: `GOWORK=off go test -mod=vendor ./...` (against a fake saves server).
Deploy notes: [docs/deploy.md](docs/deploy.md).
