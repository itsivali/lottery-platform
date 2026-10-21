// Package testvalkey starts a throwaway Valkey container for integration tests.
package testvalkey

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/valkey-io/valkey-go"
)

// Server is a running Valkey with a stop function (for fail-closed tests).
type Server struct {
	Addr string
	c    testcontainers.Container
}

// Stop terminates the container, simulating Valkey being lost.
func (s *Server) Stop() { _ = testcontainers.TerminateContainer(s.c) }

// Start launches Valkey 8 and registers cleanup.
func Start(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "valkey/valkey:8-alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start valkey: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(c) })
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := c.MappedPort(ctx, "6379/tcp")
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Addr: host + ":" + port.Port(), c: c}
}

// Client connects to the server.
func (s *Server) Client(t *testing.T) valkey.Client {
	t.Helper()
	cl, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{s.Addr}, DisableCache: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cl.Close)
	return cl
}
