package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev"

func main() {
	node := os.Getenv("NODE_NAME")
	if node == "" {
		log.Fatal("NODE_NAME is required (set from spec.nodeName in the DaemonSet)")
	}

	log.Printf("stornas-agent %s on node %s", version, node)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
}
