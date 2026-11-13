// Package lock implements the tamper-evident test lock (.synthesis/lock).
//
// The lock digests the raw user-authored synthesis.yaml fields that define
// workload strength, fault budget, oracle selection, and cluster topology,
// together with every oracle definition file under oracles.dir and the docker
// compose file. Any quiet weakening of workload, fault budget, or oracle set
// fails before a cluster is started and exits with ExitOracleDrift (4).
package lock
