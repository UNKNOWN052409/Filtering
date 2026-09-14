package main

import "time"

// ───────────────── API Protocol (shared between coordinator + worker) ─────────────────

type NodeConfig struct {
	ID       int    `json:"id"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

type MeshConfig struct {
	CoordinatorPort   int          `json:"coordinator_port"`
	RcloneRemote      string       `json:"rclone_remote"`
	DriveSyncPath     string       `json:"drive_sync_path"`
	HeartbeatInterval int          `json:"heartbeat_interval_sec"`
	TaskTimeout       int          `json:"task_timeout_sec"`
	MaxChunkLines     int          `json:"max_chunk_lines"`
	Nodes             []NodeConfig `json:"nodes"`
}

// ── Worker → Coordinator ──

type RegisterReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	NodeID   int    `json:"node_id"`
	Hostname string `json:"hostname"`
}

type RegisterResp struct {
	OK       bool   `json:"ok"`
	NodeID   int    `json:"node_id"`
	Token    string `json:"token"` // session token for subsequent requests
	Message  string `json:"message"`
}

type HeartbeatReq struct {
	Token  string `json:"token"`
	Status string `json:"status"` // "idle" | "busy" | "done"
	Lines  int64  `json:"lines_processed"`
	Hits   int64  `json:"hits"`
}

type TaskRequest struct {
	Token string `json:"token"`
}

type TaskResp struct {
	OK       bool     `json:"ok"`
	TaskID   string   `json:"task_id"`
	Keywords []string `json:"keywords"`
	Data     string   `json:"data"` // raw combo chunk lines (newline-joined) sent inline
	Lines    int      `json:"lines"`
	Message  string   `json:"message"`
}

type ResultSubmit struct {
	Token    string         `json:"token"`
	TaskID   string         `json:"task_id"`
	Results  []KeywordResult `json:"results"`
	Lines    int64          `json:"lines_processed"`
	Hits     int64          `json:"hits"`
	Malformed int64         `json:"malformed"`
	Duration float64        `json:"duration_sec"`
}

type KeywordResult struct {
	Keyword string   `json:"keyword"`
	Lines   int64    `json:"lines"`
	Hits    int64    `json:"hits"`
	Payloads []string `json:"payloads"` // login:password lines (limited)
}

type ResultResp struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// ── Coordinator → Worker ──

type StatusResp struct {
	OK          bool         `json:"ok"`
	TotalTasks  int          `json:"total_tasks"`
	DoneTasks   int          `json:"done_tasks"`
	ActiveNodes int          `json:"active_nodes"`
	TotalLines  int64        `json:"total_lines"`
	TotalHits   int64        `json:"total_hits"`
	Nodes       []NodeStatus `json:"nodes"`
}

type NodeStatus struct {
	ID       int       `json:"id"`
	Email    string    `json:"email"`
	Status   string    `json:"status"` // "offline" | "idle" | "busy" | "dead"
	LastSeen time.Time `json:"last_seen"`
	Lines    int64     `json:"lines_processed"`
	Hits     int64     `json:"hits"`
}
