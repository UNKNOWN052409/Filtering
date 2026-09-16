package main

import (
	"testing"
	"time"
)

func TestRequeueStaleAssignedTasks(t *testing.T) {
	// worker died mid-run: task older than timeout must go back to pending,
	// fresh assignments and pending tasks untouched
	c := NewCoordinator(MeshConfig{})

	old := time.Now().Add(-10 * time.Minute) // 10 min ago > default 5m timeout
	recent := time.Now().Add(-10 * time.Second)
	c.tasks["stale"] = &Task{ID: "stale", Status: "assigned", Assigned: "tok12345678", AssignedAt: &old}
	c.tasks["fresh"] = &Task{ID: "fresh", Status: "assigned", Assigned: "tok12345678", AssignedAt: &recent}
	c.tasks["pend"] = &Task{ID: "pend", Status: "pending"}
	c.tasks["done"] = &Task{ID: "done", Status: "done", AssignedAt: &old}

	c.requeueStale()

	if c.tasks["stale"].Status != "pending" {
		t.Errorf("stale assigned task must be requeued, got %s", c.tasks["stale"].Status)
	}
	if c.tasks["stale"].Assigned != "" || c.tasks["stale"].AssignedAt != nil {
		t.Error("requeued task must clear Assigned/AssignedAt")
	}
	if c.tasks["fresh"].Status != "assigned" {
		t.Errorf("fresh assignment must stay assigned, got %s", c.tasks["fresh"].Status)
	}
	if c.tasks["pend"].Status != "pending" || c.tasks["done"].Status != "done" {
		t.Error("pending/done tasks must not be touched by the reaper")
	}
}

func TestTaskTimeoutConfig(t *testing.T) {
	// explicit task_timeout_sec respected (nodes.json ships 300)
	c := NewCoordinator(MeshConfig{TaskTimeout: 1}) // 1 sec
	old := time.Now().Add(-2 * time.Second)
	c.tasks["t1"] = &Task{ID: "t1", Status: "assigned", AssignedAt: &old}
	c.requeueStale()
	if c.tasks["t1"].Status != "pending" {
		t.Errorf("task_timeout_sec=1 must requeue a 2s-old assignment, got %s", c.tasks["t1"].Status)
	}
}
