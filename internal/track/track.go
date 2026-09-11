// Package track reports task lifecycle events to an external analytics
// platform. It ports the reporting framework from another product's CLI
// (base64-encoded JSON → form-urlencoded POST) with agentwork-specific
// business fields.
//
// Reporting is compile-time opt-in: EventPostUrl is injected via ldflags
// and defaults to empty. The endpoint URL and AppID are NOT hardcoded in
// the repository — they are injected from CI pipeline variables
// (EVENT_POST_URL / EVENT_APP_ID). When EventPostUrl is empty (the default
// for a plain `go build` or `./build.sh`), NewReporter is a no-op and all
// Report calls return immediately without making network requests, so dev
// builds and tests never touch the external endpoint.
//
// All reporting is non-blocking: Report logs errors and recovers panics,
// never propagating failures to the caller. A tracking failure must never
// affect the daemon's primary flow.
package track

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/logging"
	"github.com/eushing/agentwork/internal/version"
)

// Compile-time config (ldflags-injected). The endpoint URL and AppID are
// not hardcoded in the repo — they are injected from CI pipeline variables
// (EVENT_POST_URL / EVENT_APP_ID). When EventPostUrl is empty, the package
// is fully disabled (no-op).
var (
	EventPostUrl    string    // ldflags: -X ...track.EventPostUrl=$URL
	AppID           string    // ldflags: -X ...track.AppID=$APPID
	EventSkipVerify = "false" // ldflags: -X ...track.EventSkipVerify=$SKIP
)

// EventReq is the platform's event contract. Properties carries
// agentwork-specific fields as a fixed struct — type-safe vs a free-form
// map, and the JSON tags are the platform contract.
type EventReq struct {
	AnonymousId string     `json:"anonymousId"`
	DistinctId  string     `json:"distinctId"`
	Event       string     `json:"event"`
	Time        int64      `json:"time"`
	Type        string     `json:"type"`
	Properties  properties `json:"properties"`
}

// properties is the fixed set of agentwork-specific fields reported to the
// platform. Adding a field is adding a struct field + setting it in
// BuildEvent — no map key typos possible.
type properties struct {
	Source     string `json:"source"`
	InstanceID string `json:"instance_id"`
	Event      string `json:"event"`
	GoalID     string `json:"goal_id"`
	Status     string `json:"status"`
	Version    string `json:"version"`
	Time       string `json:"time"`
}

// Event names — stable constants reported to the platform. Adding a new
// event is adding a constant here + a bus handler that uses it.
const (
	TaskCreated  = "AgentWork_Task_Created"
	TaskAssigned = "AgentWork_Task_Assigned"
	TaskFinished = "AgentWork_Task_Finished"
	TaskDeleted  = "AgentWork_Task_Deleted"
)

// Reporter sends tracking events to the external platform. A Reporter
// with enabled=false is a no-op — Report returns immediately.
type Reporter struct {
	client  *http.Client
	enabled bool
}

// NewReporter builds a Reporter from the ldflags-injected config. If
// EventPostUrl is empty, reporting is disabled (no HTTP client allocated).
func NewReporter() *Reporter {
	if EventPostUrl == "" {
		return &Reporter{enabled: false}
	}
	skipVerify := strings.EqualFold(EventSkipVerify, "true")
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: skipVerify},
		},
	}
	return &Reporter{client: client, enabled: true}
}

// Report sends one event to the platform. It is void: errors are logged
// and panics are recovered — a tracking failure never propagates to the
// caller (the bus handler) and never affects the daemon.
func (r *Reporter) Report(ctx context.Context, req EventReq) {
	if !r.enabled {
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			logging.Errorf("track: report panic: %v", rec)
		}
	}()

	data, err := encodeToBase64([]EventReq{req})
	if err != nil {
		logging.Errorf("track: encode event: %v", err)
		return
	}

	formData := url.Values{
		"data":  []string{data},
		"appid": []string{AppID},
		"debug": []string{"0"},
		"gzip":  []string{"0"},
	}
	if err := r.sendPostRequest(ctx, formData); err != nil {
		logging.Warnf("track: send event: %v", err)
	}
}

// sendPostRequest POSTs the form-encoded body to the platform endpoint.
func (r *Reporter) sendPostRequest(ctx context.Context, formData url.Values) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, EventPostUrl, bytes.NewBufferString(formData.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logging.Warnf("track: close response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("platform returned status %d", resp.StatusCode)
	}
	return nil
}

// encodeToBase64 marshals data to JSON and base64-encodes the result,
// matching the platform's expected form field shape.
func encodeToBase64(data any) (string, error) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(jsonData), nil
}

// BuildEvent constructs an EventReq from the agentwork-specific fields.
// The identity fields (AnonymousId/DistinctId) carry the daemon INSTANCE
// identity — the hostname — so every event from one deployment shares a
// stable identity and the platform can build per-instance funnels
// (created → assigned → finished). Per-task analysis uses the goal_id
// property instead. The platform requires a 32-char hex identity (UUID
// without hyphens): a hostname that is already 32-char hex (e.g. a cloud
// instance ID) is used as-is; anything else is MD5-hex encoded.
func BuildEvent(eventName, goalID, status string) EventReq {
	now := time.Now()
	host, _ := os.Hostname()
	identity := host
	if !isHex32(identity) {
		sum := md5.Sum([]byte(identity))
		identity = hex.EncodeToString(sum[:])
	}
	return EventReq{
		AnonymousId: identity,
		DistinctId:  identity,
		Event:       eventName,
		Time:        now.UnixMilli(),
		Type:        "track",
		Properties: properties{
			Source:     "agentwork",
			InstanceID: identity,
			Event:      eventName,
			GoalID:     goalID,
			Status:     status,
			Version:    version.DaemonVersion,
			Time:       now.UTC().Format(time.RFC3339Nano),
		},
	}
}

// isHex32 reports whether s is exactly 32 lowercase-hex characters —
// the tracking platform's required identity format (UUID without hyphens).
func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// extractGoalID reads the goal identifier from a bus event payload.
// Payloads come in two shapes: map[string]any (key "goal_id", used by
// goal:finished/deleted/delivered and run.terminal) and service.Goal
// struct (json tag "id", used by goal:created/assigned). We JSON-round-trip
// to a uniform map so both work without importing service. Returns "" if
// the payload has no goal id — callers skip reporting for such events.
func extractGoalID(payload any) string {
	m := toMap(payload)
	if m == nil {
		return ""
	}
	// map payloads use "goal_id"; struct payloads use "id" (json tag).
	if id, _ := m["goal_id"].(string); id != "" {
		return id
	}
	id, _ := m["id"].(string)
	return id
}

// extractStatus reads status from a bus event payload (same two shapes).
func extractStatus(payload any) string {
	m := toMap(payload)
	if m == nil {
		return ""
	}
	s, _ := m["status"].(string)
	return s
}

// toMap normalizes a bus payload to map[string]any via JSON round-trip.
// A map payload is returned as-is; a struct payload is marshaled then
// unmarshaled. Returns nil if the payload cannot be normalized.
func toMap(payload any) map[string]any {
	if m, ok := payload.(map[string]any); ok {
		return m
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// ── Bus event handlers ──
//
// Each handler extracts goal_id/status from the bus payload, skips if
// goal_id is empty (structural events), builds an EventReq, and reports.
// The context comes from the bus Publish call.

func (r *Reporter) OnGoalCreated(_ context.Context, e events.Event) {
	r.reportGoalEvent(e, TaskCreated)
}

func (r *Reporter) OnGoalAssigned(_ context.Context, e events.Event) {
	r.reportGoalEvent(e, TaskAssigned)
}

func (r *Reporter) OnGoalFinished(_ context.Context, e events.Event) {
	r.reportGoalEvent(e, TaskFinished)
}

func (r *Reporter) OnGoalDeleted(_ context.Context, e events.Event) {
	r.reportGoalEvent(e, TaskDeleted)
}

// reportGoalEvent is the shared body: guard on goal_id, build, report.
func (r *Reporter) reportGoalEvent(e events.Event, eventName string) {
	goalID := extractGoalID(e.Payload)
	if goalID == "" {
		return
	}
	status := extractStatus(e.Payload)
	r.Report(context.Background(), BuildEvent(eventName, goalID, status))
}
