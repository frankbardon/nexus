//go:build integration

package integration

import (
	"bytes"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/frankbardon/nexus/pkg/testharness"
)

// Fixed loopback addresses the checked-in serve configs bind, so each config
// stays runnable by hand. Tests never use them: rewritePorts swaps every one a
// config mentions for a free port allocated for the running test, so suites
// cannot collide with each other, with a parallel `go test`, or with anything
// else on the host that holds these ports.
const (
	// aguiDefaultAddr: configs/test-agui-*.yaml (bind) and the remote endpoint
	// in configs/test-agui-consume.yaml.
	aguiDefaultAddr = "127.0.0.1:18190"
	// a2aDefaultAddr: configs/test-a2a-serve.yaml (bind, public_url).
	a2aDefaultAddr = "127.0.0.1:18191"
	// a2aLoopbackDefaultAddr: configs/test-a2a-loopback-{server,hitl-server}.yaml
	// (bind, public_url) and the base_url in configs/test-a2a-loopback-caller.yaml.
	a2aLoopbackDefaultAddr = "127.0.0.1:18192"
)

var portDefaults = []string{aguiDefaultAddr, a2aDefaultAddr, a2aLoopbackDefaultAddr}

// testAddrs memoizes the allocated address per top-level test and default, so
// every engine, config and client in one test — subtests included — agrees.
var testAddrs sync.Map // "<TopLevelTest>|<default>" -> "127.0.0.1:<port>"

// testAddr returns the free loopback address standing in for def in the
// current top-level test, allocating it on first use. Safe to call from any
// goroutine.
func testAddr(t *testing.T, def string) string {
	key := strings.SplitN(t.Name(), "/", 2)[0] + "|" + def
	if v, ok := testAddrs.Load(key); ok {
		return v.(string)
	}
	v, _ := testAddrs.LoadOrStore(key, freeLoopbackAddr(t))
	return v.(string)
}

// aguiAddr is the current test's AG-UI listener address.
func aguiAddr(t *testing.T) string { return testAddr(t, aguiDefaultAddr) }

// a2aAddr is the current test's A2A serve listener address.
func a2aAddr(t *testing.T) string { return testAddr(t, a2aDefaultAddr) }

// a2aLoopbackAddr is the current test's loopback-callee listener address.
func a2aLoopbackAddr(t *testing.T) string { return testAddr(t, a2aLoopbackDefaultAddr) }

// freeLoopbackAddr asks the kernel for an unused loopback port and releases it.
// The port is free when returned; nothing holds it for the caller.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate loopback port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release loopback port: %v", err)
	}
	return addr
}

// rewritePorts replaces every fixed default address a config mentions with
// the current test's allocated address for it. Defaults the config does not
// mention allocate nothing.
func rewritePorts(t *testing.T, data []byte) []byte {
	for _, def := range portDefaults {
		if bytes.Contains(data, []byte(def)) {
			data = bytes.ReplaceAll(data, []byte(def), []byte(testAddr(t, def)))
		}
	}
	return data
}

// portsOption applies rewritePorts to a config loaded by the test harness.
func portsOption(t *testing.T) testharness.Option {
	return testharness.WithConfigRewrite(func(b []byte) []byte { return rewritePorts(t, b) })
}
