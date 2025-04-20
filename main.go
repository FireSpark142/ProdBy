package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"git-monitor-app/config"  // Use correct module path
	"git-monitor-app/monitor" // Use correct module path
)

func main() {
	// Command line flag for custom config file path
	configFile := flag.String("config", "", "Path to configuration file (default: ~/.config/git-monitor-app/config.toml)")
	flag.Parse()

	// --- Load Configuration ---
	cfg, err := config.LoadConfig(*configFile)
	if err != nil {
		log.Fatalf("FATAL: Failed to load configuration: %v", err)
	}

	// --- Setup Logging ---
	// TODO: Implement more robust logging (e.g., to a file)
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile) // Basic logging setup
	log.Println("--- Git Monitor App Starting ---")

	// --- Setup Signal Handling for Graceful Shutdown ---
	sigs := make(chan os.Signal, 1)
	done := make(chan bool, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigs
		log.Printf("Received signal: %s. Shutting down...", sig)
		// TODO: Add cleanup here if needed (e.g., stop watcher explicitly)
		done <- true
	}()

	// --- Start Monitoring ---
	// Run monitor in a goroutine so main can wait for signals
	go monitor.Start(cfg)

	// --- Wait for Shutdown Signal ---
	log.Println("Application started. Waiting for shutdown signal (Ctrl+C)...")
	<-done // Block until a signal is received and processed
	log.Println("--- Git Monitor App Exiting ---")
}
