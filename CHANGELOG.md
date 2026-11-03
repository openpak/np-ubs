# Changelog: np-ubs

## 0.2.0

- Storage moved to the core `saves` service: np-ubs is now an adapter over saves' neutral blob routes (platform `ps3`, title = folder type, name = save directory; needs saves v0.5.0). The wire format, discovery, signing and result codes are unchanged. The Postgres store, `NP_UBS_DATABASE_URL`, `NP_UBS_ACCOUNT_QUOTA` and the 4 MiB chunk rows are gone; new `NP_SAVES_URL` and `NP_SAVES_KEY`. PS3 saves now share the Switch cloud-save index and the per-account allowance, and appear on the website's saves page. The old `np_ubs` tables held no rows and are dropped.

## 0.1.0

- First release: signed `ubstorage.xml` discovery, all six `/ubs/sec/` operations and save icons on Postgres
  (chunked data, 1 GiB per account and per save by default). Wire format from static RE of VSH 4.93, unconfirmed on a console.
