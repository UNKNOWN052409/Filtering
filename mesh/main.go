package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
)

func main() {
	mode := flag.String("mode", "coordinator", "coordinator or worker")
	port := flag.Int("port", 7700, "port for coordinator")
	configPath := flag.String("config", "nodes.json", "path to nodes.json")
	coordAddr := flag.String("coordinator", "http://localhost:7700", "coordinator address (worker mode)")
	flag.Parse()

	// load config
	f, err := os.Open(*configPath)
	if err != nil {
		log.Fatalf("config not found: %s — copy nodes.json.example and fill in", *configPath)
	}
	defer f.Close()
	var cfg MeshConfig
	// an unchecked decode failure leaves cfg zero-valued, which is what produces
	// a 0s heartbeat interval, a 0s task timeout and an unbounded chunk size
	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		log.Fatalf("config %s: %v", *configPath, err)
	}

	if *port != 7700 {
		cfg.CoordinatorPort = *port
	}

	switch *mode {
	case "coordinator":
		coord := NewCoordinator(cfg)
		addr := fmt.Sprintf(":%d", cfg.CoordinatorPort)
		log.Printf("═══ Coordinator starting on %s ═══", addr)
		coord.Run(addr)

	case "worker":
		worker := NewWorker(cfg, *coordAddr)
		log.Printf("═══ Worker starting → connecting to %s ═══", *coordAddr)
		worker.Run()

	default:
		fmt.Fprintf(os.Stderr, "usage: mesh -mode [coordinator|worker]\n")
		os.Exit(1)
	}
}
