package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/githubflyideas/nodedata/internal/server"
	"github.com/githubflyideas/nodedata/cmd/nodedata/check"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "check":
		runCheck()
	case "serve":
		runServe()
	case "version":
		fmt.Printf("nodedata %s - L0 Sanity Check\n", version)
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `nodedata v%s - L0 Sanity Check

Usage:
  nodedata check [--timeout 5s]    Run 39-point sanity check
  nodedata serve [--port 8888]     Start Web UI server
  nodedata version                 Print version

Examples:
  nodedata check
  nodedata check --timeout 10s
  nodedata serve --port 8888

Exit codes:
  0 = all checks passed
  1 = one or more checks failed
  2 = warnings only (no failures)

`, version)
}

func runCheck() {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	timeout := fs.String("timeout", "5s", "Check timeout duration")
	fs.Parse(os.Args[2:])

	results, exitCode := check.Run(*timeout)
	check.PrintResults(results)
	os.Exit(exitCode)
}

func runServe() {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.String("port", "8888", "HTTP server port")
	interval := fs.String("interval", "5s", "Check interval")
	fs.String("ch-port", "9000", "ClickHouse port")
	fs.Parse(os.Args[2:])

	webDir, _ := os.Getwd()
	dataDir := filepath.Join(webDir, "data")
	os.MkdirAll(dataDir, 0755)

	checkInterval, _ := time.ParseDuration(*interval)
	if checkInterval == 0 {
		checkInterval = 5 * time.Second
	}
	
	runner := NewBackgroundCheckRunner(checkInterval, dataDir)
	runner.Start()
	defer runner.Stop()

	mux := server.NewMuxWithCheck(webDir, dataDir)
	
	addr := "0.0.0.0:" + *port
	fmt.Printf("📊 nodedata server listening on http://localhost:%s\n", *port)
	fmt.Printf("📁 Web root: %s\n", webDir)
	fmt.Printf("💾 Data dir: %s\n", dataDir)
	fmt.Printf("⏱️  Check interval: %s\n", *interval)
	
	if err := server.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
