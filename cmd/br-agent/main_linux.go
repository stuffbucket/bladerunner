//go:build linux

package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/stuffbucket/bladerunner/internal/agent"
)

// CLI defaults. Kept as constants to satisfy goconst (host=2 appears more
// than once as a vsock CID).
const (
	defaultHostCID        uint32 = 2 // VMADDR_CID_HOST: dial the hypervisor host
	defaultPort           uint32 = 19001
	defaultLogLevel              = "info"
	defaultDialTimeoutSec        = 60
)

// version is overridden at build time via -ldflags="-X main.version=...".
var version = "dev"

func main() {
	cid := flag.Uint("host-cid", uint(defaultHostCID), "vsock CID of the host (default 2)")
	port := flag.Uint("port", uint(defaultPort), "vsock port the host listens on")
	dialTimeout := flag.Duration("dial-timeout", defaultDialTimeoutSec*time.Second, "max time to wait for host vsock listener")
	retryInterval := flag.Duration("retry-interval", 2*time.Second, "interval between vsock dial retries")
	showVersion := flag.Bool("version", false, "print version and exit")
	logLevel := flag.String("log-level", defaultLogLevel, "log level (debug|info|warn|error)")
	flag.Parse()

	if *showVersion {
		fmt.Printf("br-agent %s\n", version)
		return
	}
	_ = logLevel // reserved for future structured logging

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, uint32(*cid), uint32(*port), *dialTimeout, *retryInterval); err != nil {
		log.Printf("br-agent: %v", err)
		stop()
		os.Exit(1)
	}
}

// run dials the host vsock, then reads commands in a loop until EOF.
func run(ctx context.Context, cid, port uint32, dialTimeout, retryInterval time.Duration) error {
	log.Printf("br-agent %s starting: host_cid=%d port=%d", version, cid, port)

	conn, err := dialVsockWithRetry(ctx, cid, port, dialTimeout, retryInterval)
	if err != nil {
		return fmt.Errorf("dial host vsock: %w", err)
	}
	defer func() { _ = conn.Close() }()
	log.Printf("br-agent connected to host vsock %d:%d", cid, port)

	return serve(ctx, conn)
}

// serve reads JSON messages from conn and dispatches them to handlers
// defined in handlers.go.
func serve(ctx context.Context, conn net.Conn) error {
	br := bufio.NewReader(conn)
	for {
		msg, err := agent.DecodeMessage(br)
		switch {
		case errors.Is(err, io.EOF):
			log.Printf("br-agent: host closed connection")
			return nil
		case errors.Is(err, agent.ErrUnsupportedVersion):
			reply(conn, &agent.Message{Error: err.Error()})
			return err
		case err != nil:
			return fmt.Errorf("decode: %w", err)
		}
		resp := dispatch(ctx, msg)
		if err := agent.EncodeMessage(conn, resp); err != nil {
			return fmt.Errorf("encode response: %w", err)
		}
	}
}

// reply best-effort writes a response, swallowing errors.
func reply(w io.Writer, msg *agent.Message) {
	_ = agent.EncodeMessage(w, msg)
}

// dispatch routes a command message to the matching handler. Returns an
// error message rather than panicking on unknown commands.
func dispatch(ctx context.Context, msg *agent.Message) *agent.Message {
	switch msg.Command {
	case agent.CmdConfigPush:
		return handleConfigPush(ctx, msg)
	case agent.CmdReadyWait:
		return handleReadyWait(ctx, msg)
	case agent.CmdUserSync:
		return handleUserSync(ctx, msg)
	default:
		return &agent.Message{Error: fmt.Sprintf("unknown command: %s", msg.Command)}
	}
}
