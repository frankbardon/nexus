//go:build integration

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/frankbardon/nexus/cmd/nexus-broker/testdata/stubcore"
	"github.com/frankbardon/nexus/pkg/brokerframe"
)

// TestClaimClientHeadersReachTheSpawnedInstance is the far end of the
// X-Nexus-* hop, asserted the way this file asserts everything else: by asking
// the RUNNING child what it was handed.
//
// The broker terminates the client's HTTP request, so a header set on the
// handshake has to survive claim, cold spawn, the dial-back register, and the
// gateway's forwarding before a plugin inside the instance can read it. The
// unit tests prove each hop; only this one proves the whole chain, which is
// where a header assembled correctly and then dropped would show up.
func TestClaimClientHeadersReachTheSpawnedInstance(t *testing.T) {
	stubBin := buildStubInstance(t)
	base, _ := startStubBrokerWithRegistry(t, stubBin)

	cr := postClaimJSON(t, base, `{"config":"engine:\n  name: stub\n"}`)
	report := stubReportWithHeaders(t, cr, map[string]string{
		"X-Nexus-Tenant-ID": "acme",
		"X-Nexus-Timezone":  "Europe/Amsterdam",
		"Authorization":     "Bearer should-not-appear",
		"X-Trace-Id":        "should-not-appear",
	})

	if got := report.ClientHeaders["tenant-id"]; got != "acme" {
		t.Errorf("instance saw client header tenant-id = %q, want acme; saw %v", got, report.ClientHeaders)
	}
	if got := report.ClientHeaders["timezone"]; got != "Europe/Amsterdam" {
		t.Errorf("instance saw client header timezone = %q, want Europe/Amsterdam", got)
	}
	if len(report.ClientHeaders) != 2 {
		t.Errorf("instance saw %v; only the X-Nexus-* namespace may cross the hop", report.ClientHeaders)
	}
}

// A client that sends no X-Nexus-* header must leave the instance with none —
// the empty announcement is what makes a second caller's turn free of the
// first's context.
func TestClaimClientWithNoHeadersLeavesTheInstanceWithNone(t *testing.T) {
	stubBin := buildStubInstance(t)
	base, _ := startStubBrokerWithRegistry(t, stubBin)

	cr := postClaimJSON(t, base, `{"config":"engine:\n  name: stub\n"}`)
	report := stubReportWithHeaders(t, cr, nil)

	if len(report.ClientHeaders) != 0 {
		t.Errorf("instance saw %v, want no client headers", report.ClientHeaders)
	}
}

// stubReportWithHeaders is stubReport with extra headers on the client
// handshake, which is the only thing these tests need that it does not do.
func stubReportWithHeaders(t *testing.T, cr claimResponse, headers map[string]string) stubcore.Report {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	conn, _, err := websocket.Dial(ctx, cr.WSURL, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatalf("dial ws_url %s: %v", cr.WSURL, err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	out, err := brokerframe.Encode(brokerframe.Frame{
		LeaseID: cr.LeaseID,
		Signal:  brokerframe.SignalIO,
		Payload: []byte(stubcore.ReportRequest),
	})
	if err != nil {
		t.Fatalf("encode report request: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, out); err != nil {
		t.Fatalf("write report request: %v", err)
	}

	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	frame, err := brokerframe.Decode(data)
	if err != nil {
		t.Fatalf("decode report frame: %v", err)
	}
	var report stubcore.Report
	if err := json.Unmarshal(frame.Payload, &report); err != nil {
		t.Fatalf("decode report payload %q: %v", frame.Payload, err)
	}
	return report
}
