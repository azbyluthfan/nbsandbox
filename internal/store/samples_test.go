// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"
)

// A run's samples, oldest first, are removed with the run.
func TestRunSamples(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedSpace(t, db, "s1")
	if _, _, _, err := db.SubmitRunWithEvent(ctx, NewRun{ID: "r1", SpaceID: "s1", Owner: "alice", Notebook: "nb.ipynb",
		CreatedAt: time.Now(), ReadOnlySpace: true}, nil, nil); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Truncate(time.Millisecond)
	w := int64(4096)
	for _, s := range []RunSample{
		{At: t0.Add(5 * time.Second), MemoryBytes: 200, CPUMillicores: 900, WrittenBytes: &w},
		{At: t0, MemoryBytes: 100, CPUMillicores: 250},
	} {
		if err := db.AddRunSample(ctx, "r1", s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.RunSamples(ctx, "r1")
	if err != nil || len(got) != 2 || !got[0].At.Equal(t0) || got[0].WrittenBytes != nil || got[1].MemoryBytes != 200 ||
		got[1].CPUMillicores != 900 || got[1].WrittenBytes == nil || *got[1].WrittenBytes != 4096 {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := db.FinishRun(ctx, "r1", Outcome{Status: "cancelled", FinishedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteRun(ctx, "s1", "r1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.RunSamples(ctx, "r1"); len(got) != 0 {
		t.Fatalf("left after the run: %+v", got)
	}
}
