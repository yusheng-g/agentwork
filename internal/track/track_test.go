package track

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/version"
)

// resetConfig restores package-level config vars after tests mutate them.
// These tests must NOT use t.Parallel(): they mutate package-level vars
// (EventPostUrl, AppID, version.DaemonVersion) shared across the suite.
func resetConfig() {
	EventPostUrl = ""
	AppID = ""
	EventSkipVerify = "false"
	version.DaemonVersion = "0.0.1-beta.1"
}

func TestNewReporter_DisabledWhenURLEmpty(t *testing.T) {
	t.Cleanup(resetConfig)
	EventPostUrl = ""

	r := NewReporter()
	if r.enabled {
		t.Fatal("Reporter should be disabled when EventPostUrl is empty")
	}
}

func TestNewReporter_EnabledWhenURLSet(t *testing.T) {
	t.Cleanup(resetConfig)
	EventPostUrl = "https://example.com/track"
	AppID = "test"

	r := NewReporter()
	if !r.enabled {
		t.Fatal("Reporter should be enabled when EventPostUrl is set")
	}
	if r.client == nil {
		t.Fatal("HTTP client should be non-nil when enabled")
	}
}

func TestReport_DisabledReporterIsNoOp(t *testing.T) {
	t.Cleanup(resetConfig)
	EventPostUrl = ""

	r := NewReporter()
	// Must not panic or hang — just returns immediately.
	r.Report(context.Background(), BuildEvent(TaskCreated, "goal-123", "active"))
}

func TestBuildEvent_Fields(t *testing.T) {
	t.Cleanup(resetConfig)
	version.DaemonVersion = "test-version"

	ev := BuildEvent(TaskCreated, "goal-abc", "backlog")

	if ev.Event != TaskCreated {
		t.Errorf("Event = %q, want %q", ev.Event, TaskCreated)
	}
	// Identity is the hostname (instance-stable), not the goalID.
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skip("hostname unavailable")
	}
	if !isHex32(host) {
		host = hex.EncodeToString(func() []byte { sum := md5.Sum([]byte(host)); return sum[:] }())
	}
	if ev.AnonymousId != host {
		t.Errorf("AnonymousId = %q, want hostname-derived %q", ev.AnonymousId, host)
	}
	if ev.DistinctId != host {
		t.Errorf("DistinctId = %q, want hostname-derived %q", ev.DistinctId, host)
	}
	if ev.Type != "track" {
		t.Errorf("Type = %q, want track", ev.Type)
	}
	if ev.Time <= 0 {
		t.Error("Time should be a positive UnixMilli timestamp")
	}

	if ev.Properties.Source != "agentwork" {
		t.Errorf("Source = %q, want agentwork", ev.Properties.Source)
	}
	if ev.Properties.Event != TaskCreated {
		t.Errorf("Event = %q, want %q", ev.Properties.Event, TaskCreated)
	}
	if ev.Properties.GoalID != "goal-abc" {
		t.Errorf("GoalID = %q, want goal-abc", ev.Properties.GoalID)
	}
	if ev.Properties.Status != "backlog" {
		t.Errorf("Status = %q, want backlog", ev.Properties.Status)
	}
	if ev.Properties.Version != "test-version" {
		t.Errorf("Version = %q, want test-version", ev.Properties.Version)
	}
	if _, err := time.Parse(time.RFC3339Nano, ev.Properties.Time); err != nil {
		t.Errorf("Time is not valid RFC3339Nano: %v", err)
	}
}

func TestBuildEvent_IdentityIsHex32(t *testing.T) {
	// The identity must always satisfy the platform's 32-char hex format,
	// whatever the hostname or goalID look like.
	ev := BuildEvent(TaskFinished, "", "done")
	if !isHex32(ev.AnonymousId) {
		t.Errorf("AnonymousId = %q, want 32-char hex", ev.AnonymousId)
	}
	if !isHex32(ev.DistinctId) {
		t.Errorf("DistinctId = %q, want 32-char hex", ev.DistinctId)
	}
	// Properties.goal_id stays as passed — the identity fallback is only
	// for the platform's anonymousId/distinctId fields.
	if ev.Properties.GoalID != "" {
		t.Errorf("GoalID = %q, want empty", ev.Properties.GoalID)
	}
}

func TestIsHex32(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"goal ID (32 hex)", "04f519e4922e62d5c02751edb1c255ce", true},
		{"uppercase hex rejected", "04F519E4922E62D5C02751EDB1C255CE", false},
		{"too short", "abc123", false},
		{"too long", "04f519e4922e62d5c02751edb1c255cef", false},
		{"non-hex chars", "zzzz19e4922e62d5c02751edb1c255ce", false},
		{"empty", "", false},
		{"hostname with hyphens", "my-host-name-123456789012345678", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHex32(tc.in); got != tc.want {
				t.Errorf("isHex32(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestEncodeToBase64_RoundTrip(t *testing.T) {
	original := []EventReq{{
		Event:       TaskCreated,
		AnonymousId: "g1",
		DistinctId:  "g1",
		Type:        "track",
		Time:        1700000000000,
		Properties:  properties{GoalID: "g1"},
	}}

	encoded, err := encodeToBase64(original)
	if err != nil {
		t.Fatalf("encodeToBase64: %v", err)
	}

	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}

	var decoded []EventReq
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if len(decoded) != 1 || decoded[0].Event != TaskCreated {
		t.Errorf("decoded event = %+v, want single TaskCreated", decoded)
	}
}

func TestReport_PostsCorrectFormData(t *testing.T) {
	t.Cleanup(resetConfig)
	version.DaemonVersion = "test-version"
	AppID = "test-app"

	var (
		gotPath    string
		gotAppID   string
		gotDecoded []EventReq
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAppID = r.FormValue("appid")
		_ = r.ParseForm()
		raw, _ := base64.StdEncoding.DecodeString(r.FormValue("data"))
		_ = json.Unmarshal(raw, &gotDecoded)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	EventPostUrl = srv.URL + "/track"
	r := NewReporter()
	r.Report(context.Background(), BuildEvent(TaskCreated, "goal-xyz", "active"))

	// Report is non-blocking but the httptest server is synchronous, so
	// by the time Report returns the POST has completed.
	if gotPath != "/track" {
		t.Errorf("POST path = %q, want /track", gotPath)
	}
	if gotAppID != "test-app" {
		t.Errorf("appid = %q, want test-app", gotAppID)
	}
	if len(gotDecoded) != 1 {
		t.Fatalf("decoded %d events, want 1", len(gotDecoded))
	}
	ev := gotDecoded[0]
	if ev.Event != TaskCreated {
		t.Errorf("event = %q, want %q", ev.Event, TaskCreated)
	}
	if ev.Properties.GoalID != "goal-xyz" {
		t.Errorf("goal_id = %q, want goal-xyz", ev.Properties.GoalID)
	}
	if ev.Properties.Status != "active" {
		t.Errorf("status = %q, want active", ev.Properties.Status)
	}
}

func TestReport_ServerErrorDoesNotPanic(t *testing.T) {
	t.Cleanup(resetConfig)
	AppID = "test"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	EventPostUrl = srv.URL
	r := NewReporter()
	// Must not panic — just logs a warning.
	r.Report(context.Background(), BuildEvent(TaskFinished, "g1", "failed"))
}

func TestExtractGoalID(t *testing.T) {
	tests := []struct {
		name    string
		payload any
		want    string
	}{
		{"map with goal_id", map[string]any{"goal_id": "g1"}, "g1"},
		{"map without goal_id", map[string]any{"foo": "bar"}, ""},
		{"nil payload", nil, ""},
		{"non-map payload", "just-a-string", ""},
		{"goal_id is not string", map[string]any{"goal_id": 123}, ""},
		// struct payloads (goal:created/assigned publish service.Goal)
		{"struct with id field", struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}{ID: "g-from-struct", Status: "active"}, "g-from-struct"},
		{"struct without id field", struct {
			Title string `json:"title"`
		}{Title: "hello"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractGoalID(tc.payload); got != tc.want {
				t.Errorf("extractGoalID() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractStatus(t *testing.T) {
	tests := []struct {
		name    string
		payload any
		want    string
	}{
		{"map with status", map[string]any{"status": "done"}, "done"},
		{"map without status", map[string]any{"foo": "bar"}, ""},
		{"nil payload", nil, ""},
		{"non-map payload", 42, ""},
		// struct payload
		{"struct with status", struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}{ID: "g1", Status: "backlog"}, "backlog"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractStatus(tc.payload); got != tc.want {
				t.Errorf("extractStatus() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHandler_SkipsWhenGoalIDEmpty(t *testing.T) {
	t.Cleanup(resetConfig)
	version.DaemonVersion = "test"
	AppID = "test"

	// If the handler were to POST, this server would record it. We assert
	// it never gets called.
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	EventPostUrl = srv.URL
	r := NewReporter()

	// Payload without goal_id — handler must skip.
	e := events.Event{Topic: "goal:created", Payload: map[string]any{"status": "active"}}
	r.OnGoalCreated(context.Background(), e)

	// Give the bus handler's Report goroutine a moment if it were to fire.
	// (reportGoalEvent calls r.Report synchronously, so no sleep is needed,
	// but we add a tiny grace period for safety.)
	time.Sleep(10 * time.Millisecond)
	if called {
		t.Error("handler should not POST when goal_id is empty")
	}
}

func TestHandler_ReportsWhenGoalIDPresent(t *testing.T) {
	t.Cleanup(resetConfig)
	version.DaemonVersion = "test"
	AppID = "test"

	var gotDecoded []EventReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		raw, _ := base64.StdEncoding.DecodeString(r.FormValue("data"))
		_ = json.Unmarshal(raw, &gotDecoded)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	EventPostUrl = srv.URL
	r := NewReporter()

	e := events.Event{
		Topic:   "goal:created",
		Payload: map[string]any{"goal_id": "g-handler", "status": "backlog"},
	}
	r.OnGoalCreated(context.Background(), e)

	if len(gotDecoded) != 1 {
		t.Fatalf("decoded %d events, want 1", len(gotDecoded))
	}
	if gotDecoded[0].Event != TaskCreated {
		t.Errorf("event = %q, want %q", gotDecoded[0].Event, TaskCreated)
	}
	if gotDecoded[0].Properties.GoalID != "g-handler" {
		t.Errorf("goal_id = %q, want g-handler", gotDecoded[0].Properties.GoalID)
	}
}

// Verify that the request body is form-urlencoded with the expected fields.
func TestReport_RequestBodyShape(t *testing.T) {
	t.Cleanup(resetConfig)
	version.DaemonVersion = "v"
	AppID = "aid"

	var bodyBytes []byte
	var contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		bodyBytes, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	EventPostUrl = srv.URL
	r := NewReporter()
	r.Report(context.Background(), BuildEvent(TaskAssigned, "g1", "active"))

	if contentType != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", contentType)
	}
	// Body should contain data=, appid=, debug=0, gzip=0 fields.
	body := string(bodyBytes)
	for _, field := range []string{"data=", "appid=aid", "debug=0", "gzip=0"} {
		if !bytes.Contains(bodyBytes, []byte(field)) {
			t.Errorf("body %q missing %q", body, field)
		}
	}
}
