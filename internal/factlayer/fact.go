// Package factlayer implements the L0 fact layer: a path-addressed state tree,
// a change ledger, and the collectors that populate them.
//
// Design principles (from L0 design doc):
//   - Facts are path-addressed, not label-tagged. Example: "net.iface.eth0.speed"
//   - Every fact carries its source file/syscall in the Src field.
//   - Missing facts are recorded explicitly as v=null with an error reason — never silently dropped.
//   - Three time references per fact: wall clock, monotonic, and boot_id.
//   - Only deltas are written to the ledger; stable machines produce KB/day.
//   - L0 never judges: no severity labels, no health scores, no causal inference.
package factlayer

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Fact is a single L0 observation.
// v is the value; nil means the fact could not be read (see Err).
type Fact struct {
	T    float64 `json:"t"`              // CLOCK_REALTIME (Unix seconds, fractional)
	Mono float64 `json:"mono"`           // CLOCK_MONOTONIC (seconds since boot)
	Boot string  `json:"boot"`           // /proc/sys/kernel/random/boot_id (first 8 chars)
	Path string  `json:"path"`           // dot-separated path, e.g. "net.iface.eth0.speed"
	V    any     `json:"v"`              // string | float64 | int64 | bool | nil
	Err  string  `json:"err,omitempty"` // EACCES / ENOENT / reason if V==nil
	Src  string  `json:"src"`           // source file or syscall, e.g. "/sys/class/net/eth0/speed"
}

// Change is one entry in the ledger: a path whose value changed between two snapshots.
type Change struct {
	T    float64 `json:"t"`
	Mono float64 `json:"mono"`
	Boot string  `json:"boot"`
	Path string  `json:"path"`
	From any     `json:"from"` // previous value (nil = was unknown/missing)
	To   any     `json:"to"`   // new value (nil = now unknown/missing)
	Src  string  `json:"src"`
}

// StateTree is an immutable snapshot of all facts at a moment in time.
type StateTree struct {
	TakenAt time.Time
	facts   map[string]Fact // keyed by Path
}

func newStateTree(facts []Fact, at time.Time) *StateTree {
	m := make(map[string]Fact, len(facts))
	for _, f := range facts {
		m[f.Path] = f
	}
	return &StateTree{TakenAt: at, facts: m}
}

// Get returns the fact at path, and whether it exists.
func (s *StateTree) Get(path string) (Fact, bool) {
	f, ok := s.facts[path]
	return f, ok
}

// All returns all facts in the tree.
func (s *StateTree) All() []Fact {
	out := make([]Fact, 0, len(s.facts))
	for _, f := range s.facts {
		out = append(out, f)
	}
	return out
}

// Diff computes the changes between the previous tree and this one.
// Only paths whose values differ (by string comparison of JSON representation)
// are included.
func (s *StateTree) Diff(prev *StateTree) []Change {
	mono := monoNow()
	boot := bootID()
	now := float64(time.Now().UnixNano()) / 1e9

	var changes []Change

	// Paths in new tree
	for path, newFact := range s.facts {
		if prevFact, ok := prev.facts[path]; !ok {
			// New path appeared
			changes = append(changes, Change{
				T: now, Mono: mono, Boot: boot,
				Path: path, From: nil, To: newFact.V, Src: newFact.Src,
			})
		} else if !valEqual(prevFact.V, newFact.V) || prevFact.Err != newFact.Err {
			changes = append(changes, Change{
				T: now, Mono: mono, Boot: boot,
				Path: path, From: prevFact.V, To: newFact.V, Src: newFact.Src,
			})
		}
	}

	// Paths that disappeared
	for path, prevFact := range prev.facts {
		if _, ok := s.facts[path]; !ok {
			changes = append(changes, Change{
				T: now, Mono: mono, Boot: boot,
				Path: path, From: prevFact.V, To: nil, Src: prevFact.Src,
			})
		}
	}

	return changes
}

// valEqual compares two fact values for equality using JSON encoding.
// This handles all types (string, float64, int64, bool, nil) uniformly.
func valEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

// AppendFactsJSONL appends a slice of facts to a JSONL file (one JSON object per line).
func AppendFactsJSONL(path string, facts []Fact) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, fact := range facts {
		if err := enc.Encode(fact); err != nil {
			return err
		}
	}
	return nil
}

// AppendChangesJSONL appends a slice of changes to a JSONL file.
func AppendChangesJSONL(path string, changes []Change) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, c := range changes {
		if err := enc.Encode(c); err != nil {
			return err
		}
	}
	return nil
}
