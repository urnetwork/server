`geolite2.mmdb` is an empty synthetic GeoLite2-City database for local tests.
It contains no IP records and provides no geographic evidence. Lookups return
unknown geography; tests that need a location must supply their own explicit
fixture or settings override.

The file is the existing managed local suite fixture, SHA-256
`33734fa8dbd3b3a7dfa9cffdec0d7dcefc18abd8bd1857c76094b2cee18d41fb`.
It contains no downloaded MaxMind data or production resources.

The release gate's generated portable fixture deliberately has no GeoLite2
database, so a lookup that no ip override answers panics there. A test that
looks up such an address without asserting its geography reads this file
through `server.UseIpDbFileForTest`. A test that asserts the geography of the
deployed build skips when the config home has no GeoLite2 database.
