// Command stubinstance is a tiny stand-in for the real nexus binary used by the
// broker's integration test. It imports pkg/brokerframe, reads the broker
// dial-back env the broker injects at spawn, connects to the broker's instance
// endpoint, registers its lease, signals ready, and then echoes any inbound IO
// frame straight back. This proves the broker's claim/spawn/proxy mechanics
// end to end without booting a real engine or requiring an LLM API key.
//
// It lives under testdata/ so the normal `go build ./...` ignores it; the
// integration test builds it on demand and points nexus_binary_path at it.
package main

import (
	"context"
	"os"
	"time"

	"github.com/coder/websocket"

	"github.com/frankbardon/nexus/pkg/brokerframe"
)

func main() {
	addr := os.Getenv(brokerframe.EnvBrokerAddr)
	leaseID := os.Getenv(brokerframe.EnvLeaseID)
	if addr == "" || leaseID == "" {
		os.Exit(2)
	}

	ctx := context.Background()
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, _, err := websocket.Dial(dialCtx, addr, nil)
	cancel()
	if err != nil {
		os.Exit(1)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	if err := write(ctx, conn, brokerframe.Frame{LeaseID: leaseID, Signal: brokerframe.SignalRegister}); err != nil {
		os.Exit(1)
	}
	if err := write(ctx, conn, brokerframe.Frame{LeaseID: leaseID, Signal: brokerframe.SignalReady}); err != nil {
		os.Exit(1)
	}

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		frame, err := brokerframe.Decode(data)
		if err != nil {
			continue
		}
		switch frame.Signal {
		case brokerframe.SignalIO:
			_ = write(ctx, conn, brokerframe.Frame{
				LeaseID: leaseID,
				Signal:  brokerframe.SignalIO,
				Payload: frame.Payload,
			})
		case brokerframe.SignalShutdown:
			return
		}
	}
}

func write(ctx context.Context, conn *websocket.Conn, f brokerframe.Frame) error {
	data, err := brokerframe.Encode(f)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, data)
}
