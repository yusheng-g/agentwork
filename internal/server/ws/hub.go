// Package ws is the WebSocket boundary. A Hub subscribes to all events.Bus
// topics and broadcasts each event as {"topic":..., "payload":...} JSON to
// every connected client. MVP is fan-out only: no per-connection filtering,
// no RPC, no auth. The frontend subscribes to the stream and filters by
// payload.task_id itself.
package ws

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/logging"
)

// topics is the full set of bus topics the hub forwards to clients. New
// topics added to the system must be listed here or they won't reach the
// frontend.
var topics = []string{
	"goal:created", "goal:assigned", "goal:finished", "goal:retrying", "goal:retry_failed",
	"goal:deleted", "goal:reviewing", "goal:review_ready", "goal:approved", "goal:review_resolved",
	"goal:delivered", "goal:deliver_failed",
	// NOTE: the Coordinator's terminal edge is "run.terminal" (dot — run.go
	// publishes it; the daemon subscribes it). P1-2 (决策 6-15⑧) aligned this
	// whitelist to the published spelling — the colon variant never fired.
	"run:enqueued", "run:coalesced", "run:claimed", "run:discarded", "run:event", "run:cancelled", "run.terminal",
	"sub_goal.created", "sub_goal.verifying", "sub_goal.verified", "sub_goal.rejected", "sub_goal.retrying", "sub_goal.failed", "sub_goal.cancelled",
	"change.ready", "change.integrated", "change.conflict",
	"comment:created",
	// the daemon's live log tail — the Web logs panel's real-time pane
	"log:line",
	"agent:created", "agent:deleted", "agent:pin_changed",
	"machine:offline",
	"squad:created", "squad:deleted", "squad:member_added", "squad:member_removed",
	"schedule:created", "schedule:fired",
	"domain:created", "domain:deleted", "domain:compiled", "domain:compile_failed",
	// team import lifecycle — published by TeamImportService (team_import.go)
	// but were missing from this whitelist, so the frontend never received
	// import completion/failure notifications.
	"team:import_enqueued", "team:imported", "team:import_failed",
}

// Hub owns the set of active WS clients and fans out bus events to them.
type Hub struct {
	bus     *events.Bus
	mu      sync.Mutex
	clients map[*Client]struct{}
}

func NewHub(bus *events.Bus) *Hub {
	return &Hub{bus: bus, clients: make(map[*Client]struct{})}
}

// Run subscribes to every topic and blocks until ctx is cancelled. Call in a
// goroutine at server startup.
func (h *Hub) Run(ctx context.Context) {
	for _, topic := range topics {
		h.bus.Subscribe(topic, func(_ context.Context, e events.Event) {
			msg, err := json.Marshal(map[string]any{"topic": e.Topic, "payload": e.Payload})
			if err != nil {
				logging.Infof("ws: marshal event %s: %v", e.Topic, err)
				return
			}
			h.broadcast(msg)
		})
	}
	<-ctx.Done()
}

func (h *Hub) register(c *Client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) unregister(c *Client) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.send)
	}
	h.mu.Unlock()
}

// broadcast sends msg to every client's send channel. Non-blocking: a slow
// client whose send buffer is full is dropped (its readPump will still tear it
// down on the next failed write). Single-user local use makes this path rare.
func (h *Hub) broadcast(msg []byte) {
	h.mu.Lock()
	for c := range h.clients {
		select {
		case c.send <- msg:
		default:
			// drop on full; better to lose a message than block the bus.
		}
	}
	h.mu.Unlock()
}
