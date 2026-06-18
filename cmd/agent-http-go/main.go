package main

import (
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/Brinjaul-Smile/agent-http-go/internal/agenthttp"
)

func main() {
	host := getenv("HOST", "127.0.0.1")
	port := getenv("PORT", "8787")

	server := agenthttp.NewServer(agenthttp.ServerOptions{
		Env: os.Environ(),
	})

	addr := host + ":" + port
	fmt.Printf("Agent HTTP server listening on http://%s\n", addr)
	if err := http.ListenAndServe(addr, server); err != nil {
		log.Fatal(err)
	}
}

func getenv(name, fallback string) string {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	return value
}
