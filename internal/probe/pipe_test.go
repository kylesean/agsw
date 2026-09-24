package probe

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestPipeExitsWhenServerClosesFirst(t *testing.T) {
	clientA, clientB := net.Pipe()
	serverA, serverB := net.Pipe()

	defer clientB.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		pipe(ctx, clientA, serverA)
		close(done)
	}()

	// Server reads one message and closes
	go func() {
		buf := make([]byte, 5)
		_, _ = serverB.Read(buf)
		_ = serverB.Close()
	}()

	// Client writes one message, but DOES NOT close clientB
	_, err := clientB.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}

	select {
	case <-done:
		// Succeeded: pipe terminated because server closed
	case <-time.After(500 * time.Millisecond):
		t.Fatal("pipe did not exit promptly when server closed first")
	}
}

func TestPipeExitsWhenClientClosesFirst(t *testing.T) {
	clientA, clientB := net.Pipe()
	serverA, serverB := net.Pipe()

	defer serverB.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		pipe(ctx, clientA, serverA)
		close(done)
	}()

	// Server keeps reading
	go func() {
		buf := make([]byte, 10)
		_, _ = serverB.Read(buf)
	}()

	// Client writes then closes clientB
	_, _ = clientB.Write([]byte("ping"))
	_ = clientB.Close()

	select {
	case <-done:
		// Succeeded
	case <-time.After(500 * time.Millisecond):
		t.Fatal("pipe did not exit promptly when client closed first")
	}
}

