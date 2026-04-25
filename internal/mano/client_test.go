package mano_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/duongnguyen/mysql-keeper/internal/mano"
)

// mockMANO stands up a minimal MANO HTTP server for testing.
// It records every /update call and immediately returns COMPLETED on poll.
type mockMANO struct {
	srv *httptest.Server

	// captured fields from the last /update request
	lastCNF      string
	lastVDU      string
	lastIsSource string // "true" or "false"

	// behaviour knobs
	updateStatus    int    // HTTP status to return from POST /update (default 202)
	opState         string // operationState in poll response (default "COMPLETED")
	opErrorDetail   string // if opState=FAILED, include this detail
	missingLcmHeader bool  // omit lcmOpOccId header from 202 response
	pollCount        atomic.Int32

	// auth behaviour knobs
	authCallCount atomic.Int32 // incremented on every POST /users/auth
	authStatus    int          // HTTP status for /users/auth (0 = 200)
	authExpiresIn string       // expires_in value (default "28800")
}

func newMockMANO(t *testing.T) *mockMANO {
	t.Helper()
	m := &mockMANO{
		updateStatus: http.StatusAccepted,
		opState:      "COMPLETED",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/users/auth", m.handleAuth)
	mux.HandleFunc("/cnflcm/v1/custom-resources/", m.handleUpdate)
	mux.HandleFunc("/cnflcm/v1/lcm-op-occ/", m.handlePoll)
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockMANO) handleAuth(w http.ResponseWriter, _ *http.Request) {
	m.authCallCount.Add(1)
	status := m.authStatus
	if status == 0 {
		status = http.StatusOK
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	expiresIn := m.authExpiresIn
	if expiresIn == "" {
		expiresIn = "28800"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"access_token": "mock-bearer-token",
		"expires_in":   expiresIn,
	})
}

func (m *mockMANO) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Parse CNF/VDU from URL: /cnflcm/v1/custom-resources/{cnf}/{vdu}/update
	var body struct {
		CNFName string `json:"cnfName"`
		VDUName string `json:"vduName"`
		UpdateVduOperatorRequest struct {
			UpdateFields map[string]string `json:"updateFields"`
		} `json:"updateVduOperatorRequest"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	m.lastCNF = body.CNFName
	m.lastVDU = body.VDUName
	m.lastIsSource = body.UpdateVduOperatorRequest.UpdateFields["replicationChannels[0].isSource"]

	if m.updateStatus != http.StatusAccepted {
		w.WriteHeader(m.updateStatus)
		return
	}
	if !m.missingLcmHeader {
		w.Header().Set("lcmOpOccId", "op-001")
	}
	w.WriteHeader(http.StatusAccepted)
}

func (m *mockMANO) handlePoll(w http.ResponseWriter, _ *http.Request) {
	m.pollCount.Add(1)
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]interface{}{"id": "op-001", "operationState": m.opState}
	if m.opState == "FAILED" {
		resp["error"] = map[string]interface{}{
			"status": 500,
			"detail": m.opErrorDetail,
		}
	}
	json.NewEncoder(w).Encode(resp)
}

// --- tests ---------------------------------------------------------------

func TestClientSetIsSource_HappyPath(t *testing.T) {
	mock := newMockMANO(t)
	client := mano.NewClient(mock.srv.URL, "mock-bearer-token")

	err := client.SetIsSource(context.Background(), "cnf-dc", "vdu-pxc", false, 10*time.Millisecond, 5*time.Second)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if mock.lastCNF != "cnf-dc" {
		t.Errorf("cnfName = %q, want cnf-dc", mock.lastCNF)
	}
	if mock.lastVDU != "vdu-pxc" {
		t.Errorf("vduName = %q, want vdu-pxc", mock.lastVDU)
	}
	if mock.lastIsSource != "false" {
		t.Errorf("isSource = %q, want false", mock.lastIsSource)
	}
	if mock.pollCount.Load() < 1 {
		t.Error("expected at least one poll, got 0")
	}
}

func TestClientSetIsSource_SetTrue(t *testing.T) {
	mock := newMockMANO(t)
	client := mano.NewClient(mock.srv.URL, "mock-bearer-token")

	if err := client.SetIsSource(context.Background(), "cnf-dc", "vdu-pxc", true, 10*time.Millisecond, 5*time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mock.lastIsSource != "true" {
		t.Errorf("isSource = %q, want true", mock.lastIsSource)
	}
}

func TestClientSetIsSource_CredentialsMode(t *testing.T) {
	mock := newMockMANO(t)
	// Use credentials mode — client must call /users/auth first.
	client := mano.NewClientWithCredentials(mock.srv.URL, "keeper", "pass")

	if err := client.SetIsSource(context.Background(), "cnf-dc", "vdu-pxc", false, 10*time.Millisecond, 5*time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mock.lastIsSource != "false" {
		t.Errorf("isSource = %q, want false", mock.lastIsSource)
	}
}

func TestClientSetIsSource_LcmOpFailed(t *testing.T) {
	mock := newMockMANO(t)
	mock.opState = "FAILED"
	mock.opErrorDetail = "PXC operator timeout"
	client := mano.NewClient(mock.srv.URL, "tok")

	err := client.SetIsSource(context.Background(), "cnf-dc", "vdu-pxc", false, 10*time.Millisecond, 5*time.Second)
	if err == nil {
		t.Fatal("expected error for FAILED op, got nil")
	}
}

func TestClientSetIsSource_Non202Response(t *testing.T) {
	mock := newMockMANO(t)
	mock.updateStatus = http.StatusInternalServerError
	client := mano.NewClient(mock.srv.URL, "tok")

	err := client.SetIsSource(context.Background(), "cnf-dc", "vdu-pxc", false, 10*time.Millisecond, 5*time.Second)
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
}

func TestClientSetIsSource_ContextCancelled(t *testing.T) {
	// slow mock: never reaches COMPLETED during context window
	slow := newMockMANO(t)
	slow.opState = "PROCESSING"
	client := mano.NewClient(slow.srv.URL, "tok")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := client.SetIsSource(ctx, "cnf-dc", "vdu-pxc", false, 10*time.Millisecond, 5*time.Second)
	if err == nil {
		t.Fatal("expected error when context cancelled, got nil")
	}
}

// TestClientEnsureToken_ConcurrentFirstCall verifies that two goroutines
// racing on the first ensureToken call produce only one POST /users/auth.
// The mutex at client.go:75-76 must serialise the burst so the second
// goroutine reuses the token the first one fetched rather than making a
// duplicate login request.
func TestClientEnsureToken_ConcurrentFirstCall(t *testing.T) {
	mock := newMockMANO(t)
	client := mano.NewClientWithCredentials(mock.srv.URL, "user", "pass")

	const goroutines = 5
	errs := make(chan error, goroutines)
	for range goroutines {
		go func() {
			errs <- client.SetIsSource(context.Background(), "cnf-dc", "vdu-pxc", false,
				10*time.Millisecond, 5*time.Second)
		}()
	}
	for range goroutines {
		if err := <-errs; err != nil {
			t.Errorf("concurrent SetIsSource failed: %v", err)
		}
	}
	if n := mock.authCallCount.Load(); n != 1 {
		t.Errorf("expected exactly 1 auth call for %d concurrent goroutines, got %d", goroutines, n)
	}
}

// TestClientSetIsSource_MissingLcmOpOccIdHeader verifies that a 202 response
// without the lcmOpOccId header is treated as a protocol error — we cannot
// poll without the operation ID.
func TestClientSetIsSource_MissingLcmOpOccIdHeader(t *testing.T) {
	mock := newMockMANO(t)
	mock.missingLcmHeader = true
	client := mano.NewClient(mock.srv.URL, "tok")

	err := client.SetIsSource(context.Background(), "cnf-dc", "vdu-pxc", false, 10*time.Millisecond, 5*time.Second)
	if err == nil {
		t.Fatal("expected error when lcmOpOccId header is absent, got nil")
	}
}

// TestClientSetIsSource_PollLcmOpOcc_FAILED_TEMP covers the FAILED_TEMP
// terminal state — the MANO spec treats it the same as FAILED for our purposes.
func TestClientSetIsSource_PollLcmOpOcc_FAILED_TEMP(t *testing.T) {
	mock := newMockMANO(t)
	mock.opState = "FAILED_TEMP"
	mock.opErrorDetail = "operator reconcile timed out (transient)"
	client := mano.NewClient(mock.srv.URL, "tok")

	err := client.SetIsSource(context.Background(), "cnf-dc", "vdu-pxc", false, 10*time.Millisecond, 5*time.Second)
	if err == nil {
		t.Fatal("expected error for FAILED_TEMP operation state, got nil")
	}
}

// TestClientSetIsSource_PollLcmOpOcc_ROLLED_BACK verifies that a ROLLED_BACK
// terminal state is surfaced as an error rather than silently swallowed.
func TestClientSetIsSource_PollLcmOpOcc_ROLLED_BACK(t *testing.T) {
	mock := newMockMANO(t)
	mock.opState = "ROLLED_BACK"
	client := mano.NewClient(mock.srv.URL, "tok")

	err := client.SetIsSource(context.Background(), "cnf-dc", "vdu-pxc", false, 10*time.Millisecond, 5*time.Second)
	if err == nil {
		t.Fatal("expected error for ROLLED_BACK operation state, got nil")
	}
}

// TestClientSetIsSource_PollTimeout verifies that pollLcmOpOcc returns a
// "did not complete within" error when the poll deadline fires (distinct from
// the context-cancellation path tested in TestClientSetIsSource_ContextCancelled).
func TestClientSetIsSource_PollTimeout(t *testing.T) {
	mock := newMockMANO(t)
	mock.opState = "PROCESSING" // never reaches COMPLETED
	client := mano.NewClient(mock.srv.URL, "tok")

	// Small poll interval + tight timeout: deadline fires after a few iterations.
	err := client.SetIsSource(context.Background(), "cnf-dc", "vdu-pxc", false,
		1*time.Millisecond, 5*time.Millisecond)
	if err == nil {
		t.Fatal("expected poll timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "did not complete within") {
		t.Errorf("expected 'did not complete within' in error message; got: %v", err)
	}
}

// TestClientLogin_Non200ReturnsError verifies that a non-200 response from
// POST /users/auth is surfaced as an error rather than leaving a stale token.
func TestClientLogin_Non200ReturnsError(t *testing.T) {
	mock := newMockMANO(t)
	mock.authStatus = http.StatusUnauthorized
	client := mano.NewClientWithCredentials(mock.srv.URL, "bad-user", "wrong-pass")

	err := client.SetIsSource(context.Background(), "cnf-dc", "vdu-pxc", false, 10*time.Millisecond, 5*time.Second)
	if err == nil {
		t.Fatal("expected error when login returns 401, got nil")
	}
}

// TestClientEnsureToken_CachedTokenIsReused verifies that a second SetIsSource
// call reuses the cached token (expires_in=28800) without re-calling /users/auth.
func TestClientEnsureToken_CachedTokenIsReused(t *testing.T) {
	mock := newMockMANO(t)
	client := mano.NewClientWithCredentials(mock.srv.URL, "user", "pass")

	if err := client.SetIsSource(context.Background(), "cnf-dc", "vdu-pxc", false,
		10*time.Millisecond, 5*time.Second); err != nil {
		t.Fatalf("first call failed: %v", err)
	}
	if err := client.SetIsSource(context.Background(), "cnf-dc", "vdu-pxc", true,
		10*time.Millisecond, 5*time.Second); err != nil {
		t.Fatalf("second call failed: %v", err)
	}
	if mock.authCallCount.Load() != 1 {
		t.Errorf("expected exactly 1 auth call (token reused), got %d", mock.authCallCount.Load())
	}
}

// TestClientEnsureToken_RefreshesWhenAboutToExpire verifies that a token with
// expires_in=50s is refreshed on the next call because the 60-second pre-refresh
// window fires: time.Now().Add(60s) > tokenExpiry (now+50s).
// Using 50s (10s below the 60s window) rather than 59s gives robust CI margin.
func TestClientEnsureToken_RefreshesWhenAboutToExpire(t *testing.T) {
	mock := newMockMANO(t)
	mock.authExpiresIn = "50" // expiry = now+50s; inside the 60s pre-refresh window
	client := mano.NewClientWithCredentials(mock.srv.URL, "user", "pass")

	if err := client.SetIsSource(context.Background(), "cnf-dc", "vdu-pxc", false,
		10*time.Millisecond, 5*time.Second); err != nil {
		t.Fatalf("first call failed: %v", err)
	}
	if err := client.SetIsSource(context.Background(), "cnf-dc", "vdu-pxc", true,
		10*time.Millisecond, 5*time.Second); err != nil {
		t.Fatalf("second call failed: %v", err)
	}
	if mock.authCallCount.Load() != 2 {
		t.Errorf("expected 2 auth calls (token refreshed before expiry), got %d", mock.authCallCount.Load())
	}
}
