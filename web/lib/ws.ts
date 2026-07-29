"use client";

import { useEffect, useRef, useCallback } from "react";
import type { WSEvent } from "./types";

const WS_URL = process.env.NEXT_PUBLIC_WS_URL || "ws://localhost:7373/ws";

type EventHandler = (payload: WSEvent["payload"]) => void;

/**
 * WSClient connects to the daemon WebSocket, dispatches events by topic,
 * and auto-reconnects with exponential backoff + jitter. Single global
 * instance via useWS() in the root layout.
 */
class WSClient {
  private ws: WebSocket | null = null;
  private handlers = new Map<string, Set<EventHandler>>();
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private reconnectAttempt = 0;
  private connected = false;

  connect() {
    if (this.ws) return;
    try {
      this.ws = new WebSocket(WS_URL);
    } catch {
      this.scheduleReconnect();
      return;
    }
    this.ws.onopen = () => {
      this.reconnectAttempt = 0;
      this.connected = true;
    };
    this.ws.onclose = () => {
      this.connected = false;
      this.ws = null;
      this.scheduleReconnect();
    };
    this.ws.onerror = () => {
      this.ws?.close();
    };
    this.ws.onmessage = (e) => {
      try {
        const evt = JSON.parse(e.data) as WSEvent;
        const hs = this.handlers.get(evt.topic);
        if (hs) for (const h of hs) h(evt.payload);
      } catch {
        // ignore unparseable frames
      }
    };
  }

  private scheduleReconnect() {
    if (this.reconnectTimer) return;
    const base = Math.min(1000 * 2 ** this.reconnectAttempt, 30000);
    const delay = base + Math.random() * 500; // jitter
    this.reconnectAttempt++;
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.connect();
    }, delay);
  }

  on(topic: string, handler: EventHandler) {
    let hs = this.handlers.get(topic);
    if (!hs) {
      hs = new Set();
      this.handlers.set(topic, hs);
    }
    hs.add(handler);
    return () => {
      hs!.delete(handler);
    };
  }

  get isConnected() {
    return this.connected;
  }
}

// Singleton — one WS connection for the whole app.
let client: WSClient | null = null;
function getClient() {
  if (!client) {
    client = new WSClient();
    client.connect();
  }
  return client;
}

/**
 * Subscribe to a WS topic. Returns a cleanup function. Must be called in a
 * client component (uses useEffect).
 */
export function useWSEvent(topic: string, handler: EventHandler) {
  const handlerRef = useRef(handler);
  handlerRef.current = handler;

  useEffect(() => {
    const c = getClient();
    return c.on(topic, (p) => handlerRef.current(p));
  }, [topic]);
}

/** Hook to access the raw WS client for advanced use. */
export function useWS() {
  return useCallback(() => getClient(), []);
}
