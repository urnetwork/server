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
	"github.com/urnetwork/glog/v2026"
)

// In a service's list, every block of the service.
const pgBouncerEveryBlock = "*"

// The services whose mains choose their path with `SetServicePgBouncerPath`.
var pgBouncerPathServices = []string{"api", "connect", "proxy", "taskworker", "alt", "gossip", "mcp"}

// The blocks whose main pools dial the PgBouncer instances directly, by
// service. Every block not listed is on the single port.
//
// Stage 1 (owner go 2026-11-13 for Monday 2026-11-16, 14:00-18:00 UTC): api
// beta, about 1.3% of the database connection requests.
var pgBouncerDirectBlocks = map[string][]string{
	"api": {"beta"},
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
