package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"
)

func TestRunHTTPServerStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := &http.Server{
		Addr: "127.0.0.1:0",
		Handler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			response.WriteHeader(http.StatusOK)
		}),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- runHTTPServer(ctx, server, logger, time.Second)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop after context cancellation")
	}
}
