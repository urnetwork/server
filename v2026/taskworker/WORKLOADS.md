# Taskworker workload profiles

The ordinary production `RunOptions` zero value and the existing `InitTasks`,
`InitTaskWorker`, and `InitTaskWorkerWithSettings` entry points retain the complete
production workload. `WorkloadProfileSubnetOperator` (`subnet-operator`) is an
explicit workload selection used by sim-testnet's two operator workers. It shares
production readiness, execution, post hooks, advisory claims, drain, retry and
finalization behavior. It changes neither task outcomes nor log severity.

The profile schedules and registers the task definitions in
`subnetOperatorTasks`. Canonical names and all legacy aliases come from the
ordinary registry. Definitions with no startup schedule are still registered:
the ST parent schedules epoch work; API/controller flows enqueue
`model.RemoveNetworkClientsTask`. Every queue claim applies the registry filter
before the candidate limit. This also covers excluded jobs enqueued independently
by API/controller flows after startup, without changing those flows or deleting
their pending rows. Restarting applies the same selection.

`RunPost` needs its own boundary because its name does not identify the original
workload. A scoped claim joins the referenced finished task and admits only a
registered original target. Malformed and orphaned wrappers remain untouched for
an ordinary worker; permitted wrappers still use the existing post executor.
The query uses PostgreSQL's validated JSON input check (available in PostgreSQL
16 and later; the simulator already pins PostgreSQL 18). Ordinary workers retain
the original claim query and unknown-target retry behavior.

## Required task dependencies

| Task family | Why the operator profile keeps it |
| --- | --- |
| `StSyncChain`, `StEpochClose`, `StCommitRoot`, `StDeposit`, `StFinalizePoke` | Contract epoch/event mirror, payout root, operator commit/deposit and finalization. Every parent/child and post retry remains available. |
| `SweepVerifyTrails`, `RollupVerifyProviderStats`, `RemoveOldVerifyProviderStats`, `RefreshVerifyProxyEgress` | Full `/verify` trail lifecycle, provider statistics, and proxy egress index refresh. |
| `RollupSearchProviderStats`, `RemoveOldSearchProviderStats` | Provider search evidence and retention. |
| `CloseExpiredContracts`, `SweepOrphanContractData`, `RemoveCompletedContracts`, `ReconcileNetEscrow` | Transfer finalization, contract retention, and escrow reconciliation. |
| `CloseExpiredNetworkClientHandlers`, `RemoveDisconnectedNetworkClients`, `SweepOrphanNetworkClientData`, `RemoveNetworkClientsTask` | Connection lifecycle and API-enqueued client removal. |
| `BackfillInitialTransferBalance`, `RefreshFreeTransferBalances`, `RebuildPointsLeaderboard` | Transfer grants and accounting-derived provider/account state. |
| `IndexSearchLocations`, `WarmNetworkGetProviderLocations`, `UpdateClientLocations`, `UpdateClientScores`, `RemoveExpiredProviderEgressLocations` | Local provider discovery, location/score indices and egress expiry. |
| `RollupClientReliabilityStats`, `UpdateReliabilities`, `RemoveOldClientReliabilityStats`, `RemoveOldClientLocationReliabilities`, `RemoveOldNetworkReliabilityWindow`, `RemoveOldProvideKeyChanges`, `UpdateClientReliabilityScores`, `UpdateNetworkReliabilityWindow` | Existing reliability maintenance, including registered legacy tasks whose current schedules are inert. |
| `ExportStats`, `ExportProvidersMap`, `BackfillClock`, `SweepProviderAuditEvents`, `RollupTransferAuditEvents`, `RemoveOldAuditNetworkEvents`, `RemoveOldAuditEvents` | Evidence/statistics export, usage clock reconciliation, audit rollups and retention. |
| `RemoveExpiredAuthCodes`, `RemoveExpiredAuthAttempts`, `RemoveExpiredWalletAuthChallenges`, `RemoveExpiredWalletNonces`, `RemoveExpiredBulkClientRemovalQuota` | Authentication and client-removal resource maintenance. |
| `TaskCleanup`, `DbMaintenance` | Existing terminal task retention and database maintenance. The profile does not introduce a purge of excluded pending work. |

API artifact publication, immutable attempt history, admission, and relay stream
readers do not execute through the task registry and are unchanged. Stats
collection, queue metrics and stream retention still start through the ordinary
taskworker runtime.

## Excluded workloads

Retail payout/advancement, subscription renewal, store reconciliation, payment
intent cleanup, wallet population, pro/referral grants, product-update and
onboarding/email campaigns are separate customer workloads. The ST task family
above performs subnet settlement independently of retail `Payout` and
`controller.AdvancePayment`; excluding the latter does not disable ST payments.

Public geolocation pin refresh, `ProberBootstrap`, `ProviderEgressProbe`,
`ExtenderProbe`, `ExtenderPublish`, and web analytics are separate external
discovery/analytics workloads. Simulator
`renderOperatorProviderEgressProbeIsolation` already authenticates an
`enabled: false` provider probe configuration and removes its ingest credential;
its bounded miner/validator scenarios supply provider traffic. Simulator
`operatorSimulationSiteSettings` supplies location metadata for those peers.
Geolocation pin reads serve `/network/geolocation-source-pins` for
qualityprobe's independent prober (`qualityprobe/ingest/pins.go`); `/verify` does not read
that table. `RefreshVerifyProxyEgress`, the verification index maintenance that
the campaign does need, remains enabled and registered.

The profile also leaves obsolete/no-op location-lookup targets and removed
targets to ordinary production workers/cleanup. Neither startup nor claims
delete their retained pending rows. The simulator must not use this profile for
a campaign that intends to exercise retail payments or the external prober;
those require their own explicit workload scope.
