package factlayer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Runner manages the L0 lifecycle:
//   - Takes a full snapshot on startup, writes facts.jsonl
//   - Re-snapshots every ResnapInterval, writes only deltas to changes.jsonl
//   - Exposes StateAt / ChangesBetween for L1 consumption
type Runner struct {
	cfg             Config
	dataDir         string
	ResnapInterval  time.Duration // default 60s
	CollectTimeout  time.Duration // per-snapshot hard timeout, default 10s

	mu       sync.RWMutex
	current  *StateTree
	ledger   []Change // in-memory ledger (trimmed to last 7 days)
}

// NewRunner creates a Runner. Call Start() to begin background collection.
func NewRunner(cfg Config, dataDir string) *Runner {
	return &Runner{
		cfg:            cfg,
		dataDir:        dataDir,
		ResnapInterval: 60 * time.Second,
		CollectTimeout: 10 * time.Second,
	}
}

// Start runs the initial snapshot synchronously, then launches background resnapshot.
// The returned cancel func stops the background goroutine.
func (r *Runner) Start(ctx context.Context) (cancel context.CancelFunc, err error) {
	if err := os.MkdirAll(r.dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("factlayer: create data dir: %w", err)
	}

	// Initial full snapshot
	snap, err := r.snapshot()
	if err != nil {
		return nil, fmt.Errorf("factlayer: initial snapshot: %w", err)
	}
	r.mu.Lock()
	r.current = snap
	r.mu.Unlock()

	// Write full snapshot to facts.jsonl
	factsPath := filepath.Join(r.dataDir, "facts.jsonl")
	if err := AppendFactsJSONL(factsPath, snap.All()); err != nil {
		// Non-fatal: log but continue
		fmt.Fprintf(os.Stderr, "factlayer: write facts.jsonl: %v\n", err)
	}

	ctx2, cancel2 := context.WithCancel(ctx)
	go r.resnapLoop(ctx2)
	return cancel2, nil
}

func (r *Runner) resnapLoop(ctx context.Context) {
	t := time.NewTicker(r.ResnapInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.resnapOnce()
		}
	}
}

func (r *Runner) resnapOnce() {
	snap, err := r.snapshot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "factlayer: resnap: %v\n", err)
		return
	}

	r.mu.Lock()
	prev := r.current
	r.current = snap
	r.mu.Unlock()

	if prev == nil {
		return
	}

	changes := snap.Diff(prev)
	if len(changes) == 0 {
		return
	}

	// Append to changes.jsonl
	changesPath := filepath.Join(r.dataDir, "changes.jsonl")
	if err := AppendChangesJSONL(changesPath, changes); err != nil {
		fmt.Fprintf(os.Stderr, "factlayer: write changes.jsonl: %v\n", err)
	}

	// Keep in-memory ledger (trim to last 7 days)
	r.mu.Lock()
	r.ledger = append(r.ledger, changes...)
	cutoff := float64(time.Now().Add(-7 * 24 * time.Hour).UnixNano()) / 1e9
	i := 0
	for i < len(r.ledger) && r.ledger[i].T < cutoff {
		i++
	}
	r.ledger = r.ledger[i:]
	r.mu.Unlock()
}

// snapshot runs Collect with a hard timeout so a hung sysfs path can't
// block the whole process (common on failed NFS mounts or bad disks).
func (r *Runner) snapshot() (*StateTree, error) {
	type result struct {
		tree *StateTree
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		ch <- result{tree: Collect(r.cfg)}
	}()
	select {
	case res := <-ch:
		return res.tree, res.err
	case <-time.After(r.CollectTimeout):
		return nil, fmt.Errorf("snapshot timed out after %s", r.CollectTimeout)
	}
}

// StateAt returns the most recent complete snapshot.
// (Full StateAt(t) replay from ledger is a future enhancement.)
func (r *Runner) StateAt(_ time.Time) *StateTree {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.current
}

// ChangesBetween returns all ledger entries with T in [t1, t2].
func (r *Runner) ChangesBetween(t1, t2 time.Time) []Change {
	f1 := float64(t1.UnixNano()) / 1e9
	f2 := float64(t2.UnixNano()) / 1e9

	r.mu.RLock()
	defer r.mu.RUnlock()

	var out []Change
	for _, c := range r.ledger {
		if c.T >= f1 && c.T <= f2 {
			out = append(out, c)
		}
	}
	return out
}

// RecentChanges returns the last n changes from the in-memory ledger.
func (r *Runner) RecentChanges(n int) []Change {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.ledger) <= n {
		out := make([]Change, len(r.ledger))
		copy(out, r.ledger)
		return out
	}
	out := make([]Change, n)
	copy(out, r.ledger[len(r.ledger)-n:])
	return out
}
