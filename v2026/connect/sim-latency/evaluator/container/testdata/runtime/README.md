# Synthetic runtime fixture

`geolite2.mmdb.b64` is the base64 representation of the repository's tiny empty
`local/testdata/config/local/mmdb/geolite2.mmdb`, not downloaded provider data.
Decoded SHA-256: `33734fa8dbd3b3a7dfa9cffdec0d7dcefc18abd8bd1857c76094b2cee18d41fb`.

Include this file beside `prepare-runtime.sh` at `testdata/runtime/` when packaging
immutable host-tool releases. The preparer decodes it only into the throwaway
`config/local` leaf. TLS keys/certificates are generated independently per run;
no production configuration or key is bundled.
