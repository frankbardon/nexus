package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/brokerframe"
)

// newReleaseTestServer wires the release endpoint over an httptest server,
// returning the server and the shared registry so a test can pre-seed leases.
func newReleaseTestServer(t *testing.T, grace time.Duration) (*httptest.Server, *Registry) {
	t.Helper()
	reg := NewRegistry(testLogger())
	rs := NewReleaseServer(testLogger(), reg, grace)
	mux := http.NewServeMux()
	rs.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, reg
}

// seedLease mints a lease and attaches a nil-conn instance + the given process,
// mirroring what claim does after a successful spawn. It returns the lease id
// and the attached instance connection so the test can inspect queued frames.
func seedLease(t *testing.T, reg *Registry, proc processHandle) (string, *wsConn) {
	t.Helper()
	id, err := reg.NewLease()
	if err != nil {
		t.Fatalf("NewLease: %v", err)
	}
	wc := newWSConn(nil)
	if err := reg.AttachInstance(id, wc); err != nil {
		t.Fatalf("AttachInstance: %v", err)
	}
	reg.SetProcess(id, proc)
	return id, wc
}

func TestReleaseLease_GracefulSendsShutdownAndRemoves(t *testing.T) {
	reg := NewRegistry(testLogger())
	proc := newFakeProcess(100)
	id, wc := seedLease(t, reg, proc)

	// The instance exits cleanly on its own (as a real engine would after the
	// shutdown frame), so the grace path never force-kills.
	close(proc.exited)

	if err := reg.releaseLease(id, "test", 2*time.Second); err != nil {
		t.Fatalf("releaseLease: %v", err)
	}

	// A shutdown frame was queued to the instance.
	select {
	case data := <-wc.send:
		f, err := brokerframe.Decode(data)
		if err != nil {
			t.Fatalf("decode queued frame: %v", err)
		}
		if f.Signal != brokerframe.SignalShutdown {
			t.Fatalf("queued frame signal = %q, want shutdown", f.Signal)
		}
		if f.LeaseID != id {
			t.Errorf("queued frame lease = %q, want %q", f.LeaseID, id)
		}
	default:
		t.Fatal("no shutdown frame was queued to the instance")
	}

	// The process was NOT force-killed (it exited gracefully).
	select {
	case <-proc.killed:
		t.Fatal("process was force-killed despite a graceful exit")
	default:
	}

	// The lease/slot is freed.
	if reg.Has(id) {
		t.Error("lease still present after release")
	}
}

func TestReleaseLease_ForceKillsOnGraceTimeout(t *testing.T) {
	reg := NewRegistry(testLogger())
	proc := newFakeProcess(101) // never exits on its own
	id, _ := seedLease(t, reg, proc)

	start := time.Now()
	if err := reg.releaseLease(id, "test", 60*time.Millisecond); err != nil {
		t.Fatalf("releaseLease: %v", err)
	}
	elapsed := time.Since(start)

	// The grace period must have elapsed before the kill.
	if elapsed < 60*time.Millisecond {
		t.Errorf("release returned in %v, want >= grace (60ms)", elapsed)
	}

	// The process was force-killed (no orphan).
	select {
	case <-proc.killed:
	case <-time.After(time.Second):
		t.Fatal("stuck instance was not force-killed")
	}

	if reg.Has(id) {
		t.Error("lease still present after forced release")
	}
}

func TestReleaseLease_UnknownLeaseErrors(t *testing.T) {
	reg := NewRegistry(testLogger())
	err := reg.releaseLease("does-not-exist", "test", time.Second)
	if !errors.Is(err, errUnknownLease) {
		t.Fatalf("releaseLease(unknown) = %v, want errUnknownLease", err)
	}
}

func TestReleaseLease_IdempotentDoubleRelease(t *testing.T) {
	reg := NewRegistry(testLogger())
	proc := newFakeProcess(102)
	id, _ := seedLease(t, reg, proc)
	close(proc.exited)

	if err := reg.releaseLease(id, "test", time.Second); err != nil {
		t.Fatalf("first releaseLease: %v", err)
	}
	// Second release of the now-gone lease is a clean no-op (unknown), not a
	// panic.
	if err := reg.releaseLease(id, "test", time.Second); !errors.Is(err, errUnknownLease) {
		t.Fatalf("second releaseLease = %v, want errUnknownLease", err)
	}
}

func TestReleaseHTTP_KnownLeaseReturns200(t *testing.T) {
	ts, reg := newReleaseTestServer(t, 2*time.Second)
	proc := newFakeProcess(200)
	id, _ := seedLease(t, reg, proc)
	close(proc.exited)

	resp, err := http.Post(ts.URL+"/release/"+id, "application/json", nil)
	if err != nil {
		t.Fatalf("POST /release: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "released" || body["lease_id"] != id {
		t.Errorf("unexpected body: %+v", body)
	}
	if reg.Has(id) {
		t.Error("lease not removed after HTTP release")
	}
}

func TestReleaseHTTP_UnknownLeaseReturns404(t *testing.T) {
	ts, _ := newReleaseTestServer(t, time.Second)

	resp, err := http.Post(ts.URL+"/release/nope", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /release: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestReleaseHTTP_DoubleReleaseIsClean(t *testing.T) {
	ts, reg := newReleaseTestServer(t, time.Second)
	proc := newFakeProcess(201)
	id, _ := seedLease(t, reg, proc)
	close(proc.exited)

	first, err := http.Post(ts.URL+"/release/"+id, "application/json", nil)
	if err != nil {
		t.Fatalf("first POST /release: %v", err)
	}
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want 200", first.StatusCode)
	}

	second, err := http.Post(ts.URL+"/release/"+id, "application/json", nil)
	if err != nil {
		t.Fatalf("second POST /release: %v", err)
	}
	second.Body.Close()
	if second.StatusCode != http.StatusNotFound {
		t.Fatalf("second status = %d, want 404 (idempotent)", second.StatusCode)
	}
}
