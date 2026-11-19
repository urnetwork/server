# Corrected bridge full-archive retention

The complete raw bridge archive was uploaded and read back by exact MinIO
version before reclaiming its redundant local tar file. All original local run
files and the versioned curated baseline remain intact.

- Archive: 381,931,520 bytes, SHA-256
  `b9fa2303bcebab516a66ce6bf40d0433eda61c0214c989b2169494d598fc8824`.
- Bucket: `competition`.
- Key: `blob/competition/calibration/c2-bridge-2026-10-27/efc1648c4a33d646740e70602c7a3bcb28d5ecc536068abd/b9fa2303bcebab516a66ce6bf40d0433eda61c0214c989b2169494d598fc8824.tar`.
- Version: `68f6bf2e-72ae-41a4-8605-4ac0710dd297`.
- Protection: COMPLIANCE through `2028-01-01T00:00:00Z`.
- Native receipt on sille:
  `/var/lib/urnetwork/competition-c2-fullarchive-20261027.pZrHJfGj/receipt.private.json`,
  SHA-256 `828239a2059c454593a1501abcf97955131608a586158f4c5468d54fc69f3deb`.

The receipt records full exact-version SHA-256 readback equality and all member
hashes verified before upload. This archive's replica delivery remains
unverified; the earlier small-object replica proof is not a substitute.

At approximately 15:10 UTC, the redundant local `full-evidence.tar` was removed
after rechecking its regular-file type, exact size, SHA-256 and the retained
receipt. This recovered 381,931,520 bytes for launch qualification. It can be
restored by downloading the exact version above using the native protected blob
store. No original measurement, receipt, source, container image, or MinIO
object was deleted. Incident and retention owner: `support@ur.xyz`.
