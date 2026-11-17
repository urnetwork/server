package server

// Which blocks of the deployed services reach PgBouncer directly
// (db_pgbouncer_path.go), while that path rolls out. Owner, 2026-11-13: "Code
// can choose to use a single port, or choose to go direct."
//
// The services warp runs beside the database host (`pgBouncerPathServices`)
// call `SetServicePgBouncerPath` with their own name in main, before any
// database use. A process goes direct when warp runs it in a block
// (WARP_BLOCK) that `pgBouncerDirectBlocks` lists for its service, or the
// service lists `pgBouncerEveryBlock`. Every other block, a process warp does
// not run, and every other main keep the single port: bringyourctl and the
// other tools among them, which operators run through a tunnel that forwards
// only that port.
//
// A stage of the rollout is a change to `pgBouncerDirectBlocks`, and to the
// stage its test pins, merged in the stage's window and deployed with its
// service. One build serves every block of a service and only the listed
// blocks go direct, so a stage can go out to its block alone (`warpctl deploy
// main <service> <version> <block>`), and that block reverts the same way, to
// the build before.

import (
	"github.com/urnetwork/glog"
)

// In a service's list, every block of the service.
const pgBouncerEveryBlock = "*"

// The services whose mains choose their path with `SetServicePgBouncerPath`.
var pgBouncerPathServices = []string{"api", "connect", "proxy", "taskworker", "alt", "gossip", "mcp"}

// The blocks whose main pools dial the PgBouncer instances directly, by
// service. Every block not listed is on the single port.
//
// Stage 1 (owner go 2026-11-13; live from 2026-11-14 07:58 UTC, its 24-hour
// watch passed 2026-11-15 09:00 UTC): api beta, about 1.3% of the database
// connection requests.
//
// Stage 2 (owner go 2026-11-15 ~16:50 UTC, after the PgBouncer instances
// were restarted for their listen backlog of 65535): every api block, about
// 29% of the pool acquires (CPUOPT6 Part C §4.2, Part D #1).
//
// Stage 3 (same go; owner 2026-11-15 ~19:45 UTC: move as fast as possible,
// next stage after 45 minutes clean): every connect block, about 43% of the
// pool acquires.
//
// Stage 4 (same go): every block of taskworker, proxy, alt, gossip and mcp,
// the remaining 28% of the pool acquires, so no deployed service uses nginx's
// stream hop. The single port stays for the tools and the tunnel (owner
// 2026-11-13: keep both options).
var pgBouncerDirectBlocks = map[string][]string{
	"api":        {pgBouncerEveryBlock},
	"connect":    {pgBouncerEveryBlock},
	"proxy":      {pgBouncerEveryBlock},
	"taskworker": {pgBouncerEveryBlock},
	"alt":        {pgBouncerEveryBlock},
	"gossip":     {pgBouncerEveryBlock},
	"mcp":        {pgBouncerEveryBlock},
}

// The path the table gives a service's block. Only a process in a block goes
// direct.
func pgBouncerPathOfBlock(directBlocks map[string][]string, service string, block string) PgBouncerPath {
	if block == "" {
		return PgBouncerPathSinglePort
	}
	for _, directBlock := range directBlocks[service] {
		if directBlock == pgBouncerEveryBlock || directBlock == block {
			return PgBouncerPathDirect
		}
	}
	return PgBouncerPathSinglePort
}

// Chooses this process's path from the rollout table, for the block warp runs
// it in, as a deployed service's main does before any database use. Returns
// the path chosen.
func SetServicePgBouncerPath(service string) PgBouncerPath {
	block, _ := Block()
	path := pgBouncerPathOfBlock(pgBouncerDirectBlocks, service, block)
	glog.Infof("[db]pgbouncer path for service %s block %q = %s\n", service, block, path)
	SetPgBouncerPath(path)
	return path
}
