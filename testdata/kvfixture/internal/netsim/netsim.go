// Package netsim provides an in-process Raft peer transport whose links can be
// cut and healed for deterministic unit testing without Docker.
package netsim

import (
	"context"
	"fmt"
	"sync"
	"time"

	"synthesis.dev/kvfixture/internal/raft"
)

// Net is a set of Raft nodes joined by directed in-memory links.
type Net struct {
	mu      sync.RWMutex
	nodes   map[string]*raft.Raft
	cut     map[string]bool
	frozen  map[string]bool
	latency time.Duration
}

// NewNet returns an empty simulated peer network.
func NewNet(latency time.Duration) *Net {
	return &Net{
		nodes:   make(map[string]*raft.Raft),
		cut:     make(map[string]bool),
		frozen:  make(map[string]bool),
		latency: latency,
	}
}

// Register attaches a Raft node to the simulated network.
func (n *Net) Register(id string, r *raft.Raft) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.nodes[id] = r
}

func edge(from, to string) string { return from + ">" + to }

// Cut blackholes traffic in both directions between a and b.
func (n *Net) Cut(a, b string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.cut[edge(a, b)] = true
	n.cut[edge(b, a)] = true
}

// Heal restores traffic in both directions between a and b.
func (n *Net) Heal(a, b string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.cut, edge(a, b))
	delete(n.cut, edge(b, a))
}

// Isolate cuts a node off from every other registered node.
func (n *Net) Isolate(id string) {
	n.mu.Lock()
	peers := make([]string, 0, len(n.nodes))
	for k := range n.nodes {
		if k != id {
			peers = append(peers, k)
		}
	}
	n.mu.Unlock()
	for _, p := range peers {
		n.Cut(id, p)
	}
}

// Rejoin heals a node's links to every other registered node.
func (n *Net) Rejoin(id string) {
	n.mu.Lock()
	peers := make([]string, 0, len(n.nodes))
	for k := range n.nodes {
		if k != id {
			peers = append(peers, k)
		}
	}
	n.mu.Unlock()
	for _, p := range peers {
		n.Heal(id, p)
	}
}

// HealAll removes every cut.
func (n *Net) HealAll() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.cut = make(map[string]bool)
}

// CutCount reports how many directed links are currently dropped.
func (n *Net) CutCount() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return len(n.cut)
}

func (n *Net) isCut(from, to string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.cut[edge(from, to)] || n.frozen[to] || n.frozen[from]
}

// Freeze makes a node stop answering peer RPCs.
func (n *Net) Freeze(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.frozen[id] = true
}

// Thaw undoes Freeze.
func (n *Net) Thaw(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.frozen, id)
}

func (n *Net) node(id string) (*raft.Raft, bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	r, ok := n.nodes[id]
	return r, ok
}

// Transport returns a raft.Transport sending from the given node.
func (n *Net) Transport(from string) raft.Transport { return &transport{net: n, from: from} }

type transport struct {
	net  *Net
	from string
}

func (t *transport) deliver(ctx context.Context, to string, fn func(*raft.Raft)) error {
	if t.net.isCut(t.from, to) {
		<-ctx.Done()
		return ctx.Err()
	}
	if t.net.latency > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(t.net.latency):
		}
	}
	target, ok := t.net.node(to)
	if !ok {
		return fmt.Errorf("netsim: unknown node %q", to)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(target)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
	}
	if t.net.isCut(to, t.from) {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (t *transport) AppendEntries(ctx context.Context, peer string, req raft.AppendReq) (raft.AppendResp, error) {
	var resp raft.AppendResp
	err := t.deliver(ctx, peer, func(r *raft.Raft) { resp = r.HandleAppend(req) })
	if err != nil {
		return raft.AppendResp{}, err
	}
	return resp, nil
}

func (t *transport) RequestVote(ctx context.Context, peer string, req raft.VoteReq) (raft.VoteResp, error) {
	var resp raft.VoteResp
	err := t.deliver(ctx, peer, func(r *raft.Raft) { resp = r.HandleVote(req) })
	if err != nil {
		return raft.VoteResp{}, err
	}
	return resp, nil
}

func (t *transport) Close() error { return nil }
