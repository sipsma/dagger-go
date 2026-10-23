package main

import (
	"encoding/json"
	"sync"
	"time"
)

// Measurement build only: a timeline of the walk and the module
// extractions, from this helper's side, which Binary adds to its output as
// .gobuild-tier-trace/walk.json. Times are Unix nanoseconds.

type pkgTrace struct {
	Pkg    string
	Name   string
	Height int64
	// DepsDone is when this package's goroutine saw its last dependency's
	// result; Enqueued when it asked the gate for an exec slot, after
	// building its (lazy) container; Granted when the gate handed it the
	// slot; Acquired when acquire returned; DirSynced when its output
	// directory's request returned; FileSynced when its archive's did;
	// Released just before it gave the slot back.
	DepsDone, Enqueued, Granted, Acquired, DirSynced, FileSynced, Released int64
}

type modTrace struct {
	Mod string
	// Queued is when its prefetch was asked for; Started when the prefetch
	// got one of fetch's slots; Done when the prefetch's request returned,
	// 0 if it hadn't by the time the trace was written. A package's own
	// request can start the extraction before its prefetch does.
	Queued, Started, Done int64
}

type walkTrace struct {
	mu   sync.Mutex
	pkgs map[string]*pkgTrace
	mods map[string]*modTrace
}

var wtrace = &walkTrace{pkgs: map[string]*pkgTrace{}, mods: map[string]*modTrace{}}

func nowNS() int64 { return time.Now().UnixNano() }

func (t *walkTrace) pkg(id string, f func(*pkgTrace)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.pkgs[id]
	if !ok {
		p = &pkgTrace{}
		t.pkgs[id] = p
	}
	f(p)
}

func (t *walkTrace) mod(name string, f func(*modTrace)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	m, ok := t.mods[name]
	if !ok {
		m = &modTrace{Mod: name}
		t.mods[name] = m
	}
	f(m)
}

func (t *walkTrace) json() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	data, _ := json.Marshal(map[string]any{"slots": packageConcurrency, "written": nowNS(), "pkgs": t.pkgs, "mods": t.mods})
	return string(data)
}
