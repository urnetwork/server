# T3's file clusters

§22.4's T3 for the file clusters (EVSB-M2-17): production's shapes, every host a container of the minio fork's
`settle_testhooks` build, which §22.9's suite drives through each host's barrier control port.

| Cluster | Shape | S3 | Control ports |
|---|---|---|---|
| meta-1 | 4 hosts x 2 drives, EC:4, the first metadata cluster | `127.0.0.1:52101`-`52104` | `52111`-`52114` |
| seg-1 | 4 hosts x 4 drives, EC:8, the segment cluster | `127.0.0.1:52201`-`52204` | `52211`-`52214` |
| meta-2 | one 8-drive process, EC:4, a second metadata cluster | `127.0.0.1:52301` | `52311` |

```sh
local/file-cluster-t3/t3.sh build    # the fork's hooked build and t3tool, into sidecar-test-fc-minio:local
local/file-cluster-t3/t3.sh up       # starts them and makes the buckets
local/file-cluster-t3/t3.sh suite    # server/cli/filecluster-suite on every cluster
go test -tags sidecar_filecluster_t3 -count=1 -timeout 30m -run TestFileClusterSuite .
local/file-cluster-t3/t3.sh down     # removes the containers and their drives
```

Containers are `sidecar-test-fc-*`, ports are on loopback above 50000, and the root user in `docker-compose.yml` is
synthetic. Every host has a fixed address on the stack's own network (172.30.52.0/24), so a host a test stops and starts
(sidecart3's HostDown and HostUp, the suite's host cases) comes back where the others dial it. The image is `FROM scratch` with two binaries built here, so nothing is fetched. Hosts run a drive timeout
of 5 s unless `FILECLUSTER_T3_DRIVE_TIMEOUT` sets production's 30 s, as the nightly and Gn-out runs do.

The hooked build never ships: a production cluster runs the same commit's release build, whose suite run has no
FS-114 cases and so does not pass alone. `settle_passed_build` is recorded once this T3 run and the cluster's own run
agree on the build (plan-EVSB-settle-gate).

meta-2, the one-process cluster, fails on the fork's build of 2026-11-11: some minutes after any DELETE its stash
sweep dereferences a lock server a one-process set never registers and the process exits, and in one run of three it
took a DELETE past its deadline. It names no
build, which is the gate failing closed, and its FS-114 cases need two hosts, so it never passes alone.
