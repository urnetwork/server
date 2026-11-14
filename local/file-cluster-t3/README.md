# T3's file clusters

§22.4's T3 for the file clusters (EVSB-M2-17): production's shapes, every host a container of the minio fork's
`settle_testhooks` build, which §22.9's suite drives through each host's barrier control port.

| Cluster | Shape | S3 | Control ports |
|---|---|---|---|
| meta-1 | 4 hosts x 2 drives, EC:4, the first metadata cluster | `127.0.0.1:52101`-`52104` | `52111`-`52114` |
| seg-1 | 4 hosts x 4 drives, EC:8, the segment cluster | `127.0.0.1:52201`-`52204` | `52211`-`52214` |
| meta-2 | 4 hosts x 2 drives, EC:4, a second metadata cluster | `127.0.0.1:52301`-`52304` | `52311`-`52314` |

```sh
local/file-cluster-t3/t3.sh build    # the fork's hooked build and t3tool, into sidecar-test-fc-minio:local
local/file-cluster-t3/t3.sh up       # starts them and makes the buckets
local/file-cluster-t3/t3.sh suite    # server/cli/filecluster-suite on every cluster
go test -tags sidecar_filecluster_t3 -count=1 -timeout 30m -run TestFileClusterSuite .
local/file-cluster-t3/t3.sh down     # removes the containers and their drives
```

FS-41's storage losses (EVSB-M2-20; §19.10's site-loss runbook) run on these clusters through `wipe.sh`:

```sh
local/file-cluster-t3/wipe.sh drives seg-1 1 2 3   # those hosts' drives lost for good, past seg-1's parity
local/file-cluster-t3/wipe.sh site                 # every cluster on new empty drives, its bucket made again
go test -tags sidecar_filecluster_t3 -count=1 -timeout 30m -run '^TestFileClusterReseedStorageLossesOnT3$' .
```

`TestFileClusterReseedStorageLossesOnT3` (`file_cluster_reseed_test.go`) is the store's half of the row: a head
that still reads while the segment it names does not, every bucket new and empty and passing the suite before any
re-seed, and the new buckets taking a re-seed's segment, head and `reseed/` statement as first writes. It removes
every drive T3 has and leaves the clusters up and empty, so `sidecar-t3.sh nightly` runs it last, and it is in no
whole-suite script. What evs's `/reseed` admits and what a mirror returns through it run in sidecar on its fakes
(`evsservice/reseed_test.go`, `sidecartest/evs_reseed_world_test.go`); the route has not run on these clusters.

Containers are `sidecar-test-fc-*`, ports are on loopback above 50000, and the root user in `docker-compose.yml` is
synthetic. Every host has a fixed address on the stack's own network (172.30.52.0/24), so a host a test stops and starts
(sidecart3's HostDown and HostUp, the suite's host cases) comes back where the others dial it. The image is `FROM scratch` with two binaries built here, so nothing is fetched. Hosts run a drive timeout
of 5 s unless `FILECLUSTER_T3_DRIVE_TIMEOUT` sets production's 30 s, as the nightly and Gn-out runs do.

The hooked build never ships: a production cluster runs the same commit's release build, whose suite run has no
FS-114 cases and so does not pass alone. `settle_passed_build` is recorded once this T3 run and the cluster's own run
agree on the build (plan-EVSB-settle-gate).

meta-2 was one 8-drive process until 2026-11-12. On the fork's build of 2026-11-11 its stash sweep reached through a
lock server one process never registers and the process exited, and in one run of three it took a DELETE past its
deadline. The owner put a cluster of one process out of scope (QD-756): §19.6 builds every cluster from 4 hosts, the
fork refuses to start one process on a cluster's drives under the settle rules, and meta-2 is 4 x 2 like meta-1, so
FS-63's second-cluster cases and the suite run on a second cluster of hosts. The suite refuses a target of one host.
