package coord

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// stubHandler is a WorkerHandler that simulates a worker for tests
// without touching the runner package.
type stubHandler struct {
	mu       sync.Mutex
	state    string
	report   *Report
	accepted *Assignment
}

func (s *stubHandler) Accept(ctx context.Context, a *Assignment) Acceptance {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = "running"
	s.accepted = a
	return Acceptance{Accepted: true, WorkerID: a.WorkerID}
}

func (s *stubHandler) Heartbeat() Heartbeat {
	s.mu.Lock()
	defer s.mu.Unlock()
	wid := ""
	rid := ""
	if s.accepted != nil {
		wid = s.accepted.WorkerID
		rid = s.accepted.RunID
	}
	return Heartbeat{
		WorkerID:     wid,
		RunID:        rid,
		State:        s.state,
		EventsSent:   100,
		LatencyP99Ms: 50,
		CurrentTPS:   1000,
	}
}

func (s *stubHandler) Abort(ctx context.Context, reason string) AbortResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = "aborted"
	return AbortResponse{Accepted: true, WorkerID: idFromAccepted(s.accepted)}
}

func (s *stubHandler) LastReport() *Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.report
}

func (s *stubHandler) Complete(rep *Report) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = "done"
	s.report = rep
}

func idFromAccepted(a *Assignment) string {
	if a == nil {
		return ""
	}
	return a.WorkerID
}

// makeMTLS generates a self-signed CA + server/client cert/key pair on
// disk under tmpDir and returns the MTLSConfig pair (server, client).
// Both halves trust the single CA so the tests exercise full mTLS.
func makeMTLS(t *testing.T) (server, client MTLSConfig) {
	t.Helper()
	tmp := t.TempDir()

	// CA.
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "aforo-loadgen-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	caPath := filepath.Join(tmp, "ca.pem")
	writePEM(t, caPath, "CERTIFICATE", caDER)

	mkLeaf := func(commonName string) (string, string) {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		template := &x509.Certificate{
			SerialNumber: big.NewInt(2),
			Subject:      pkix.Name{CommonName: commonName},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
			IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
			DNSNames:     []string{"localhost"},
		}
		der, _ := x509.CreateCertificate(rand.Reader, template, caTemplate, &key.PublicKey, caKey)
		certPath := filepath.Join(tmp, commonName+".cert.pem")
		keyPath := filepath.Join(tmp, commonName+".key.pem")
		writePEM(t, certPath, "CERTIFICATE", der)
		keyDER, _ := x509.MarshalECPrivateKey(key)
		writePEM(t, keyPath, "EC PRIVATE KEY", keyDER)
		return certPath, keyPath
	}

	srvCert, srvKey := mkLeaf("server")
	cliCert, cliKey := mkLeaf("client")

	server = MTLSConfig{CertFile: srvCert, KeyFile: srvKey, CAFile: caPath}
	client = MTLSConfig{CertFile: cliCert, KeyFile: cliKey, CAFile: caPath, ServerName: "localhost"}
	return server, client
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if err := pem.Encode(out, &pem.Block{Type: typ, Bytes: der}); err != nil {
		t.Fatal(err)
	}
}

// startStubWorker boots a WorkerServer on 127.0.0.1:<random> with the
// given handler. Returns the bound addr and a cleanup func.
func startStubWorker(t *testing.T, handler WorkerHandler, srvMTLS MTLSConfig) (string, func()) {
	t.Helper()
	srv, err := NewWorkerServer(WorkerServerConfig{
		ListenAddr: "127.0.0.1:0",
		MTLS:       srvMTLS,
		Handler:    handler,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan struct{})
	go func() {
		_ = srv.Start(ctx)
		close(doneCh)
	}()
	// Wait for Addr() to populate.
	deadline := time.Now().Add(2 * time.Second)
	for srv.Addr() == "" {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("worker server did not bind")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cleanup := func() {
		cancel()
		<-doneCh
	}
	return srv.Addr(), cleanup
}

func TestEndToEndDispatchHeartbeatReport(t *testing.T) {
	srvMTLS, cliMTLS := makeMTLS(t)

	stubA := &stubHandler{}
	stubB := &stubHandler{}
	addrA, stopA := startStubWorker(t, stubA, srvMTLS)
	defer stopA()
	addrB, stopB := startStubWorker(t, stubB, srvMTLS)
	defer stopB()

	c, err := NewCoordinator(CoordinatorConfig{
		WorkerAddrs:       []string{addrA, addrB},
		MTLS:              cliMTLS,
		HeartbeatInterval: 50 * time.Millisecond,
		DropoutTimeout:    1 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	pc := PartitionConfig{
		TenantIDs:      []string{"t1", "t2", "t3", "t4"},
		ScenarioYAML:   "schema_version: 1\nname: test-distributed\n",
		ManifestJSON:   "{}",
		TargetName:     "perf-aws",
		TotalTargetTPS: 4000,
	}
	accepted, err := c.Dispatch(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	if len(accepted) != 2 {
		t.Fatalf("expected 2 acceptances; got %d", len(accepted))
	}

	// Worker A and B should each have ~half the tenants.
	totalAssigned := 0
	stubA.mu.Lock()
	if stubA.accepted != nil {
		totalAssigned += len(stubA.accepted.TenantIDs)
	}
	stubA.mu.Unlock()
	stubB.mu.Lock()
	if stubB.accepted != nil {
		totalAssigned += len(stubB.accepted.TenantIDs)
	}
	stubB.mu.Unlock()
	if totalAssigned != 4 {
		t.Fatalf("partition lost tenants; total assigned=%d want 4", totalAssigned)
	}

	// Move stubs to "done" so PollUntilDone exits.
	finalReport := &Report{
		WorkerID:        "w",
		EventsSucceeded: 1000,
		LatencyP99Ms:    50,
	}
	stubA.Complete(finalReport)
	stubB.Complete(finalReport)

	pollCtx, pollCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer pollCancel()
	if err := c.PollUntilDone(pollCtx); err != nil {
		t.Fatalf("PollUntilDone: %v", err)
	}

	agg := c.AggregateReports(context.Background())
	if agg.WorkersReported != 2 {
		t.Fatalf("expected 2 workers in aggregate; got %d", agg.WorkersReported)
	}
	if agg.EventsSucceeded != 2000 {
		t.Fatalf("expected sum of events=2000; got %d", agg.EventsSucceeded)
	}
}

func TestPartitionTenantsDeterministic(t *testing.T) {
	ids := []string{"t1", "t2", "t3", "t4", "t5", "t6"}
	a, _ := partitionTenants(ids, 3)
	b, _ := partitionTenants(ids, 3)
	if !equalChunks(a, b) {
		t.Fatalf("partition not deterministic across calls")
	}
	// Every tenant lands in exactly one chunk.
	seen := map[string]int{}
	for _, c := range a {
		for _, id := range c {
			seen[id]++
		}
	}
	for _, id := range ids {
		if seen[id] != 1 {
			t.Fatalf("tenant %s seen %d times across chunks", id, seen[id])
		}
	}
}

func TestPartitionTenantsRejectsEmpty(t *testing.T) {
	if _, err := partitionTenants(nil, 3); err == nil {
		t.Fatal("empty tenant list must error")
	}
	if _, err := partitionTenants([]string{"t"}, 0); err == nil {
		t.Fatal("n=0 must error")
	}
}

func TestSplitTPSDistributesRemainder(t *testing.T) {
	got := splitTPS(10, 3)
	sum := 0
	for _, v := range got {
		sum += v
	}
	if sum != 10 {
		t.Fatalf("split lost throughput; got=%v sum=%d", got, sum)
	}
	for _, v := range got {
		if v == 0 {
			t.Fatalf("split produced zero share; got=%v", got)
		}
	}
}

func TestWorkerDropoutAfterTimeout(t *testing.T) {
	srvMTLS, cliMTLS := makeMTLS(t)

	stubA := &stubHandler{}
	addrA, stopA := startStubWorker(t, stubA, srvMTLS)
	stubB := &stubHandler{}
	addrB, stopB := startStubWorker(t, stubB, srvMTLS)
	defer stopB()

	c, err := NewCoordinator(CoordinatorConfig{
		WorkerAddrs:       []string{addrA, addrB},
		MTLS:              cliMTLS,
		HeartbeatInterval: 30 * time.Millisecond,
		DropoutTimeout:    150 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	pc := PartitionConfig{
		TenantIDs:    []string{"t1", "t2"},
		ScenarioYAML: "x",
		ManifestJSON: "{}",
		TargetName:   "perf-aws",
	}
	if _, err := c.Dispatch(context.Background(), pc); err != nil {
		t.Fatal(err)
	}

	// Fire a couple successful heartbeats first.
	c.pollOnce(context.Background())
	c.pollOnce(context.Background())
	// Then kill stubA.
	stopA()
	// Mark stubB done so the test terminates after dropout detection.
	stubB.Complete(&Report{WorkerID: "w", EventsSucceeded: 100})

	// Wait long enough for the dropout window + a few polls.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.pollOnce(context.Background())
		if c.allTerminal() {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	if !c.allTerminal() {
		t.Fatalf("coordinator did not reach terminal after dropout")
	}
	agg := c.AggregateReports(context.Background())
	if len(agg.WorkersDropped) != 1 {
		t.Fatalf("expected 1 dropped worker; got %v", agg.WorkersDropped)
	}
	if agg.WorkersReported != 1 {
		t.Fatalf("expected 1 reporting worker (the survivor); got %d", agg.WorkersReported)
	}
}

func TestTLSRefusesUntrustedClient(t *testing.T) {
	srvMTLS, _ := makeMTLS(t)
	stub := &stubHandler{}
	addr, stop := startStubWorker(t, stub, srvMTLS)
	defer stop()

	// Build an unrelated client cert+CA pair — the worker must reject.
	_, otherCli := makeMTLS(t)
	_, err := NewCoordinator(CoordinatorConfig{
		WorkerAddrs:       []string{addr},
		MTLS:              otherCli,
		HeartbeatInterval: 50 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("coordinator must refuse untrusted CA")
	}
}

func equalChunks(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}
