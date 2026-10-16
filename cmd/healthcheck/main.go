// Command healthcheck probes readiness without shell utilities in the runtime image.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	address := flag.String("url", "http://localhost:8081/health/ready", "readiness endpoint")
	flag.Parse()

	if err := check(*address); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func check(address string) error {
	client := &http.Client{Timeout: 4 * time.Second}
	response, err := client.Get(address)
	if err != nil {
		return fmt.Errorf("check readiness: %w", err)
	}

	if err := response.Body.Close(); err != nil {
		return fmt.Errorf("close readiness response: %w", err)
	}

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("readiness returned status %d", response.StatusCode)
	}

	return nil
}
