package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"synthesis.dev/synthesis/pkg/schema"
)

type partitionRecord struct {
	Spec       string
	TargetNode string
	InjectNS   int64
	WithdrawNS int64
	InjectMS   int64
	WithdrawMS int64
}

func resolvePartitionTarget(ctx context.Context, cl *composeCluster, selector string) (*nodeEndpoint, error) {
	if selector != "role:leader" {
		for i := range cl.Nodes {
			if cl.Nodes[i].ID == selector {
				return &cl.Nodes[i], nil
			}
		}
		return nil, fmt.Errorf("partition selector %q does not match any cluster node", selector)
	}

	client := &http.Client{Timeout: 1200 * time.Millisecond}
	var bestNodeID string
	var bestTerm uint64
	leaderVotes := map[string]int{}

	for _, n := range cl.Nodes {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.BaseURL()+"/status", nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			continue
		}
		var st statusPayload
		if err := json.Unmarshal(body, &st); err != nil {
			continue
		}
		if st.Role == "leader" && st.Term >= bestTerm {
			bestTerm = st.Term
			bestNodeID = n.ID
		}
		if st.LeaderID != "" {
			leaderVotes[st.LeaderID]++
		}
	}

	if bestNodeID == "" {
		var topVotes int
		for id, v := range leaderVotes {
			if v > topVotes {
				topVotes = v
				bestNodeID = id
			}
		}
	}
	for i := range cl.Nodes {
		if cl.Nodes[i].ID == bestNodeID {
			return &cl.Nodes[i], nil
		}
	}
	return nil, fmt.Errorf("could not resolve elected leader across %d nodes", len(cl.Nodes))
}

func executePeerPartition(
	ctx context.Context,
	cl *composeCluster,
	spec schema.ScheduledFault,
	driveStart time.Time,
) (*partitionRecord, error) {
	startWait := time.Until(driveStart.Add(time.Duration(spec.StartMS) * time.Millisecond))
	if startWait > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(startWait):
		}
	}

	target, err := resolvePartitionTarget(ctx, cl, spec.Target)
	if err != nil {
		return nil, err
	}

	// Disconnect the isolated node from the internal Raft peer network ONLY.
	// Its client network stays attached so clients can observe stale lease reads
	// while the remaining majority elects a new leader on kvpeer.
	discCmd := exec.CommandContext(ctx, "docker", "network", "disconnect", "-f", cl.PeerNetwork, target.ContainerName)
	if out, err := discCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("docker network disconnect %s %s: %w (%s)",
			cl.PeerNetwork, target.ContainerName, err, strings.TrimSpace(string(out)))
	}
	injNow := time.Now()
	rec := &partitionRecord{
		Spec:       spec.Raw,
		TargetNode: target.ID,
		InjectNS:   injNow.UnixNano(),
		InjectMS:   injNow.Sub(driveStart).Milliseconds(),
	}

	// Always attempt heal even if the parent context is cancelled mid-partition.
	heal := func() error {
		hctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		connCmd := exec.CommandContext(
			hctx,
			"docker", "network", "connect",
			"--alias", target.PeerAlias,
			cl.PeerNetwork,
			target.ContainerName,
		)
		out, err := connCmd.CombinedOutput()
		if err != nil && !strings.Contains(strings.ToLower(string(out)), "already exists") {
			return fmt.Errorf("docker network connect %s %s: %w (%s)",
				cl.PeerNetwork, target.ContainerName, err, strings.TrimSpace(string(out)))
		}
		return nil
	}

	endWait := time.Until(driveStart.Add(time.Duration(spec.EndMS) * time.Millisecond))
	if endWait > 0 {
		select {
		case <-ctx.Done():
			_ = heal()
			return nil, ctx.Err()
		case <-time.After(endWait):
		}
	}

	if err := heal(); err != nil {
		return nil, err
	}
	wdNow := time.Now()
	rec.WithdrawNS = wdNow.UnixNano()
	rec.WithdrawMS = wdNow.Sub(driveStart).Milliseconds()
	return rec, nil
}
