# Deploy

```sh
cp example.env .env   # fill in required values
set -a; . ./.env; set +a
GOWORK=off go run -mod=vendor .
```

Needs:

- PostgreSQL database `np_ubs` (tables are created at start).
- `NP_UBS_RESOURCE_KEY`: the 20-byte discovery signing key, vsh `0x683fb8` XOR `0x7F` (resource key slot 9). Not in git.
- `NP_TICKET_KEY` and the account core (`NP_CORE_ADDR`, `NP_CORE_KEY`), as for the other NP services.
- np-account with `NP_UBS_URL=http://<np-ubs>:8536`: it forwards `/ubs/` and `/np/resource/ubstorage/` here and lifts its
  60 s read/write timeout for those paths (saves are large).
- DNS/TLS: the console fetches `http://static-resource.np.community.playstation.net/np/resource/ubstorage/ubstorage.xml`
  and then uses the `NP_UBS_FQDN` host (default `ubs.np.community.playstation.net`) over HTTPS; both must reach np-account
  and the TLS front must hold a name for `NP_UBS_FQDN`. Icons are fetched from `NP_UBS_ICON_BASE` (default `http://` + FQDN).

Quadlet: `deploy/openpak-np-ubs.container`; image `ghcr.io/openpak/np-ubs`. Health: `GET /healthz`.
