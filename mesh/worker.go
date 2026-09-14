package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// ───────────────── Worker Node ─────────────────

type Worker struct {
	config    MeshConfig
	coordinatorURL string
	token     string
	nodeID    int
	email     string
}

func NewWorker(cfg MeshConfig, coordinatorAddr string) *Worker {
	return &Worker{
		config:         cfg,
		coordinatorURL: coordinatorAddr,
	}
}

func (w *Worker) Run() {
	// find our credentials from config
	if len(w.config.Nodes) == 0 {
		log.Fatal("[worker] no nodes configured in nodes.json")
	}
	// use first available node (or node_id from env)
	nodeIdx := 0
	if idStr := os.Getenv("NODE_ID"); idStr != "" {
		fmt.Sscanf(idStr, "%d", &nodeIdx)
		nodeIdx-- // 1-based to 0-based
	}
	if nodeIdx < 0 || nodeIdx >= len(w.config.Nodes) {
		nodeIdx = 0
	}
	node := w.config.Nodes[nodeIdx]
	w.nodeID = node.ID
	w.email = node.Email

	// register
	hostname, _ := os.Hostname()
	reg := RegisterReq{
		Email:    node.Email,
		Password: node.Password,
		NodeID:   node.ID,
		Hostname: hostname,
	}
	var regResp RegisterResp
	if err := w.post("/api/register", reg, &regResp); err != nil || !regResp.OK {
		log.Fatalf("[worker] register failed: %v — %s", err, regResp.Message)
	}
	w.token = regResp.Token
	log.Printf("[worker] registered as node %d (%s) — token %s", w.nodeID, w.email, w.token[:8])

	// start heartbeat
	go w.heartbeatLoop()

	// handle ctrl+c
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("[worker] shutting down...")
		w.sendHeartbeat("offline", 0, 0)
		os.Exit(0)
	}()

	// main loop: poll for tasks
	for {
		task := w.pollTask()
		if task == nil || !task.OK {
			time.Sleep(5 * time.Second)
			continue
		}

		log.Printf("[worker] got task %s: %d lines, keywords: %s",
			task.TaskID[:8], task.Lines, strings.Join(task.Keywords, ", "))

		// process the task
		w.processTask(task)
	}
}

func (w *Worker) pollTask() *TaskResp {
	var resp TaskResp
	url := fmt.Sprintf("/api/task?token=%s", w.token)
	if err := w.get(url, &resp); err != nil {
		return nil
	}
	return &resp
}

func (w *Worker) processTask(task *TaskResp) {
	// download chunk if available (for now, keywords-only tasks)
	// process each keyword against the combo lines
	// In a real setup, the chunk would be downloaded from chunkURL
	// For now, we report ready and wait for next task

	start := time.Now()
	w.sendHeartbeat("busy", 0, 0)

	// simulate processing (replace with actual combofilter engine call)
	time.Sleep(1 * time.Second)

	// submit results
	result := ResultSubmit{
		Token:     w.token,
		TaskID:    task.TaskID,
		Lines:     0,
		Hits:      0,
		Malformed: 0,
		Duration:  time.Since(start).Seconds(),
	}
	var resp ResultResp
	if err := w.post("/api/result", result, &resp); err != nil {
		log.Printf("[worker] result submit failed: %v", err)
	}

	w.sendHeartbeat("idle", 0, 0)
}

func (w *Worker) heartbeatLoop() {
	ticker := time.NewTicker(time.Duration(w.config.HeartbeatInterval) * time.Second)
	for range ticker.C {
		w.sendHeartbeat("idle", 0, 0)
	}
}

func (w *Worker) sendHeartbeat(status string, lines, hits int64) {
	req := HeartbeatReq{
		Token:  w.token,
		Status: status,
		Lines:  lines,
		Hits:   hits,
	}
	var resp map[string]bool
	w.post("/api/heartbeat", req, &resp)
}

// ───────────────── HTTP helpers ─────────────────

func (w *Worker) post(path string, data interface{}, result interface{}) error {
	body, _ := json.Marshal(data)
	resp, err := http.Post(w.coordinatorURL+path, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(result)
}

func (w *Worker) get(path string, result interface{}) error {
	resp, err := http.Get(w.coordinatorURL + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(result)
}
