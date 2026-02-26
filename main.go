package main

import (
	"fmt"
	"os"

	"github.com/unkeyed/mono-repo-test/svc/api"
	"github.com/unkeyed/mono-repo-test/svc/worker"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: service <api|worker>")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "api":
		api.Run()
	case "worker":
		worker.Run()
	default:
		fmt.Fprintf(os.Stderr, "unknown service: %s (expected api or worker)\n", os.Args[1])
		os.Exit(1)
	}
}
