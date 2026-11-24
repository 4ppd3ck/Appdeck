// eventbus.go — the deck's local pub/sub event bus. Cards talk to each
// other through the launcher instead of discovering each other's ports:
// wHawk publishes to topic "security/alerts", wTower subscribes, and neither
// knows the other's address. Transport: plain HTTP — POST to publish, an
// SSE stream to subscribe (EventSource in every browser, one script tag
// with /appdeck-bus.js to use it all).
//
// Loopback by construction: the launcher's own Host guard applies, and the
// bus answers cross-origin card pages with a permissive CORS header so a
// page on 127.0.0.1:24501 can subscribe to 127.0.0.1:<deck>. The bus is a
// local-programmer surface, not an internet API.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// busHistoryCap bounds the replay buffer per topic — late subscribers get
// the recent past, not the whole history.
const busHistoryCap = 100

// busTopicRe is the topic charset: letters, digits, dots, dashes, slashes.
var busTopicRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,63}$`)

// busMessage is the wire format of one event (and the SSE data payload).
type busMessage struct {
	Topic  string          `json:"topic"`
	Type   string          `json:"type"`
	Source string          `json:"source,omitempty"` // publisher's APPDECK_APP_ID (self-declared)
	Data   json.RawMessage `json:"data,omitempty"`
	TS     float64         `json:"ts"`
}

// busSub is one SSE subscriber.
type busSub struct {
	ch     chan busMessage
	topics map[string]bool // empty set = every topic
}

// busPublish hands one message to the fan-out (tests and routes share it).
func (s *Server) busPublish(msg busMessage) int {
	msg.TS = float64(time.Now().UnixNano()) / 1e9
	s.busMu.Lock()
	defer s.busMu.Unlock()
	// Replay buffer per topic.
	s.busHistory[msg.Topic] = append(s.busHistory[msg.Topic], msg)
	if len(s.busHistory[msg.Topic]) > busHistoryCap {
		s.busHistory[msg.Topic] = s.busHistory[msg.Topic][1:]
	}
	delivered := 0
	for _, sub := range s.busSubs {
		if len(sub.topics) > 0 && !sub.topics[msg.Topic] {
			continue
		}
		select {
		case sub.ch <- msg:
			delivered++
		default:
			// A slow subscriber drops the event rather than stalling the
			// fan-out — SSE readers reconnect and the history replays.
		}
	}
	return delivered
}

// busSubscribe registers a subscriber; the returned func unregisters.
func (s *Server) busSubscribe(topics []string) (*busSub, func()) {
	tset := map[string]bool{}
	for _, t := range topics {
		if t != "" {
			tset[t] = true
		}
	}
	sub := &busSub{ch: make(chan busMessage, 256), topics: tset}
	s.busMu.Lock()
	s.busSubs = append(s.busSubs, sub)
	s.busMu.Unlock()
	return sub, func() {
		s.busMu.Lock()
		for i, cand := range s.busSubs {
			if cand == sub {
				s.busSubs = append(s.busSubs[:i], s.busSubs[i+1:]...)
				break
			}
		}
		s.busMu.Unlock()
	}
}

// busHistoryFor copies the replay buffer for the given topics (all when
// empty).
func (s *Server) busHistoryFor(topics []string) []busMessage {
	tset := map[string]bool{}
	for _, t := range topics {
		if t != "" {
			tset[t] = true
		}
	}
	s.busMu.Lock()
	defer s.busMu.Unlock()
	var out []busMessage
	topicsInOrder := make([]string, 0, len(s.busHistory))
	for t := range s.busHistory {
		topicsInOrder = append(topicsInOrder, t)
	}
	sort.Strings(topicsInOrder)
	for _, t := range topicsInOrder {
		if len(tset) > 0 && !tset[t] {
			continue
		}
		out = append(out, s.busHistory[t]...)
	}
	return out
}

// serveBusPublish answers POST /api/bus/publish {topic, type, data, source}.
func (s *Server) serveBusPublish(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*") // card pages on their own ports
	var msg busMessage
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad JSON body"})
		return
	}
	if !busTopicRe.MatchString(msg.Topic) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": `topic must match letters/digits/._-/ (max 64 chars), like "security/alerts" or "deck/updates"`})
		return
	}
	if msg.Type == "" {
		msg.Type = "message"
	}
	delivered := s.busPublish(msg)
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "delivered": delivered})
}

// serveBusEvents answers GET /api/bus/events?topics=a,b — an SSE stream:
// first the replay buffer for the requested topics, then live events, with
// a comment heartbeat keeping idle connections warm.
func (s *Server) serveBusEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}
	topics := strings.Split(r.URL.Query().Get("topics"), ",")
	if len(topics) == 1 && topics[0] == "" {
		topics = nil
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)

	sub, unsubscribe := s.busSubscribe(topics)
	defer unsubscribe()

	// Catch-up: everything the topics already carried, in publish order.
	for _, msg := range s.busHistoryFor(topics) {
		writeSSE(w, msg)
	}
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case msg := <-sub.ch:
			writeSSE(w, msg)
			flusher.Flush()
		}
	}
}

// writeSSE frames one message as an SSE data line.
func writeSSE(w http.ResponseWriter, msg busMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
}

// serveBusTopics answers GET /api/bus/topics: what exists and who listens.
func (s *Server) serveBusTopics(w http.ResponseWriter) {
	s.busMu.Lock()
	type topicInfo struct {
		Events      int `json:"events"`
		Subscribers int `json:"subscribers"`
	}
	out := map[string]topicInfo{}
	for t, hist := range s.busHistory {
		out[t] = topicInfo{Events: len(hist)}
	}
	for _, sub := range s.busSubs {
		for t := range sub.topics {
			info := out[t]
			info.Subscribers++
			out[t] = info
		}
		if len(sub.topics) == 0 {
			// Wildcard listeners are counted on "*" — there is no concrete
			// topic list to attach them to.
			info := out["*"]
			info.Subscribers++
			out["*"] = info
		}
	}
	s.busMu.Unlock()
	s.writeJSON(w, http.StatusOK, map[string]any{"topics": out})
}

// appdeckBusJS is the one-tag client for card pages: served from the
// launcher so it derives the bus base URL from its own script src — a card
// page includes <script src="{APPDECK_LAUNCHER_URL}/appdeck-bus.js"></script>
// and calls window.AppDeckBus.publish/subscribe with no other wiring.
const appdeckBusJS = `// AppDeck event bus client — one script tag, no wiring:
//   <script src="{APPDECK_LAUNCHER_URL}/appdeck-bus.js"></script>
//   AppDeckBus.subscribe(["security/alerts"], ev => …);
//   AppDeckBus.publish("my-topic", "ping", {hello: 1});
(function () {
  var script = document.currentScript;
  var base = "";
  if (script && script.src) base = new URL(script.src).origin;
  else if (window.APPDECK_LAUNCHER_URL) base = window.APPDECK_LAUNCHER_URL;
  function subscribe(topics, cb) {
    var url = base + "/api/bus/events";
    if (topics && topics.length) url += "?topics=" + encodeURIComponent(topics.join(","));
    var es = new EventSource(url);
    es.onmessage = function (e) {
      try { cb(JSON.parse(e.data)); } catch (err) { /* a half-written line is skipped */ }
    };
    return es; // the caller keeps/closes it
  }
  function publish(topic, type, data, source) {
    return fetch(base + "/api/bus/publish", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ topic: topic, type: type, data: data, source: source })
    }).then(function (r) { return r.json(); });
  }
  window.AppDeckBus = { subscribe: subscribe, publish: publish };
})();
`
