# Protocol: np-ubs

Full RE notes with firmware addresses: OpenPak research `ps3/ubs-protocol.md`. Summary of what this server implements.

## Discovery

`GET http://static-resource.<env>.community.playstation.net/np/resource/ubstorage/ubstorage.xml`, signed with resource key
slot 9 (`np-common/resource.Sign`):

```xml
<!--Sce-Np-Signature: 40 hex-->
<?xml version="1.0" encoding="utf-8"?><ubstorage status="alive"><ubstorage-fqdn>HOST</ubstorage-fqdn></ubstorage>
```

`status`: `alive`, or `maintenance`/`end`/`before` (console errors `0x8002a4b2`/`b1`/`b0`).

## Requests

`POST https://HOST/ubs/sec/<op>` with header `Sce-Np-Info-Length: <XML bytes>`:

```xml
<?xml version="1.0" encoding="utf-8"?><ubstorage platform="ps3" sv="4.93"><ticket>base64</ticket><ticketjid>jid</ticketjid>
<psid type="open">hex</psid><folder type="ps3|ps1|ps2|psp|minis" [seq] [start] [max]><data name="SAVEDIR"/></folder></ubstorage>
```

| Op | Request | Reply (inside `<ubstorage result="00">`) |
| --- | --- | --- |
| `is_supported_title` | `<data name="TITLE"/>` under the root, no ticket | `<supported>1</supported>` (2 = not supported) |
| `get_save_data_info_list` | `<folder type start max/>`, max ≤ 50 | storage-info, `<folder … start results>` with `results` × data |
| `set_save_data` | `<folder><data name><info>…</info><atts><att name="icon" length md5sum/><att name="data" length md5sum/></atts></data></folder>`, then `\r\n\r\n` [icon `\r\n\r\n`] data | storage-info, folder, data |
| `get_save_data` | folder + data name | header `Sce-Np-Info-Length`, XML (storage-info, folder, data), `\r\n\r\n`, data bytes |
| `delete_save_data` | folder + data name | storage-info, folder |
| `notify_downloaded` | folder + data name | folder, data (`downloadable-date`) |

Blocks:

```xml
<storage-info quotasize="Q" totalsize="USED"/>
<folder type="ps3" total="N" totalsize="BYTES" seq="REV"/>
<data name="SAVEDIR"><length>N</length><date>RFC3339</date><info><owner-info/><title/><subtitle/><updated/><lock/><size/>
  <details/>[<icon-url/>]<downloadable-date/></info></data>
```

`seq` is a per-folder counter bumped on every set and delete (not checked on requests). `start` ≥ 1 is treated as 1-based;
0 means the first entry. The request signatures (`Sce-Np-Signature`, `md5sum`) are not required, but a wrong `md5sum`
rejects the upload.

## Results

`00` ok, `02` bad ticket, `07` internal, `1a` save over `NP_UBS_SAVE_MAX`, `4a` no such save, `5f` account storage full (the saves allowance, shared across networks, or 1000 saves)
(inferred: the save-data plugin special-cases it), `ff` malformed. Always HTTP 200 with `Content-Length`.

## Storage

None here. Each operation maps onto the core saves service (`docs/protocol.md` there, "Neutral blobs"), platform `ps3`, title = the
folder `type`, name = the save directory: `get_save_data_info_list` is a paged list (page <= 50, `start` 1-based as RE'd),
`set_save_data` streams the icon then the data (md5s checked by saves), `get_save_data` returns the data after the XML with
`Sce-Np-Info-Length`, `delete_save_data` deletes, `notify_downloaded` reads the info. `quotasize`/`totalsize` are the account allowance
and its use across all networks; `seq` is saves' folder revision. Everything that is not an index key (`title`, `subtitle`, `details`,
`owner-info`, `lock`, `updated`, console `size`) travels as the blob's `meta` JSON.
