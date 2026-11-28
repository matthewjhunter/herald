package main

import (
	"testing"

	"github.com/matthewjhunter/herald/internal/pipeline"
	"github.com/matthewjhunter/herald/internal/storage"
)

func TestBatchKindsFor(t *testing.T) {
	both := func(_ int64, _ string) bool { return true }
	curateOnly := func(_ int64, kind string) bool { return kind == pipeline.KindCurate }
	running := func(kinds ...string) []storage.AIBatch {
		var out []storage.AIBatch
		for _, k := range kinds {
			out = append(out, storage.AIBatch{ID: "b-" + k, Kind: k, Status: storage.AIBatchRunning})
		}
		return out
	}

	cases := []struct {
		name       string
		stages     stageSet
		mode       pipeline.Mode
		running    []storage.AIBatch
		wantScreen bool
		wantCurate bool
	}{
		{"batching on registers the batched kinds", allStages(), both, nil, true, true},
		{"batching on, only this process's stages", stageSet{stageScreen: true}, both, nil, true, false},
		{"batching off, nothing in flight", allStages(), nil, nil, false, false},
		// The reason this function exists: turning [batch] off must not strand
		// what was already submitted. The results still have to be collected.
		{"batching off drains what is in flight", allStages(), nil, running(pipeline.KindScreen, pipeline.KindCurate), true, true},
		{"batching off drains only the kinds in flight", allStages(), nil, running(pipeline.KindCurate), false, true},
		{"a drain respects stage ownership", stageSet{stageCurate: true}, nil, running(pipeline.KindScreen), false, false},
		// Same hole one level down: a kind dropped from [batch].stages while
		// its batches are still running.
		{"a kind removed from stages still drains", allStages(), curateOnly, running(pipeline.KindScreen), true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			screen, curate := batchKindsFor(tc.stages, tc.mode, 1, tc.running)
			if screen != tc.wantScreen || curate != tc.wantCurate {
				t.Errorf("batchKindsFor = (screen %v, curate %v), want (%v, %v)", screen, curate, tc.wantScreen, tc.wantCurate)
			}
		})
	}
}

func TestSubmitGate(t *testing.T) {
	if g := submitGate(nil); g(0, pipeline.KindScreen) || g(7, pipeline.KindCurate) {
		t.Error("with batching off the gate must refuse every submission")
	}
	curateOnly := func(_ int64, kind string) bool { return kind == pipeline.KindCurate }
	g := submitGate(curateOnly)
	if g(0, pipeline.KindScreen) || !g(7, pipeline.KindCurate) {
		t.Error("with batching on the gate must be the configured mode")
	}
}
