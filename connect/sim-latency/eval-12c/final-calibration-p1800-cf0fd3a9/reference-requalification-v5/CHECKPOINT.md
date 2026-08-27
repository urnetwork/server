# Reference v5 pilot checkpoint

Sealed at 2026-08-27T18:55:30Z against frozen source commit
`5ca3d5242f4a7d40efe4415635608023b05a0956`.

- Pilot result: accepted for the fresh hidden five-seed/four-pass screen only.
- Shared-baseline ordering: better `35391.23925 ms` < no-op
  `38802.5505 ms` < worse `46422.22730 ms`; designated baseline
  `36599.16255 ms`.
- Integrity: 164 evidence-manifest entries verified, zero hash mismatches,
  exactly one attempt per reference, all security controls true, and zero
  residual competition containers or networks.
- Pilot decision SHA-256:
  `cf539704ee1dd80c5df93e4d417cbac4abcbe037a1b89c63c60534851eac8100`.
- Pilot qualification SHA-256:
  `8bdc86dcf68a8f8a4c686d8d6267510e121ab7800c9bbcc7cfa4dbce1ac1ca10`.
- Hidden protocol SHA-256:
  `4969535eb343049d7b790c5fff8e82b7eb7a60b6e92d2e2aa94e6466e7789fad`.
- Hidden runner SHA-256:
  `a889248bf2b2175e79ce0f5526cfa294fea17700f56f6eec46eda8f53aae519e`.

The checkpoint intentionally excludes private seed material, the disclosed
pilot seed reveal, and bulk runtime evidence. The retained local runtime is
bound by the immutable decision and manifest hashes above.
