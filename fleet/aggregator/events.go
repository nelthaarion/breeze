package aggregator

import (
	"encoding/json"

	"github.com/nelthaarion/breeze/events"
	"github.com/nelthaarion/breeze/fleet"
	"github.com/nelthaarion/breeze/fleet/transport/eventtransport"
)

func (a *Aggregator) attachEvents() error {
	if !a.cfg.transportEnabled("events") {
		return nil
	}
	backend := events.NewInMemoryBackend(a.cfg.EventsBus)
	spansCancel, err := backend.Subscribe(eventtransport.DefaultSpansTopic, func(_ *events.Context, msg events.RawMessage) error {
		if !eventtransport.Authorized(a.cfg.IngestToken, msg.Payload, msg.Meta) {
			return eventtransport.ErrUnauthorized
		}
		var spans []fleet.Span
		if err := json.Unmarshal(msg.Payload, &spans); err != nil {
			return err
		}
		return a.AcceptSpans(spans)
	})
	if err != nil {
		return err
	}
	hbCancel, err := backend.Subscribe(eventtransport.DefaultHeartbeatTopic, func(_ *events.Context, msg events.RawMessage) error {
		if !eventtransport.Authorized(a.cfg.IngestToken, msg.Payload, msg.Meta) {
			return eventtransport.ErrUnauthorized
		}
		var hb fleet.Heartbeat
		if err := json.Unmarshal(msg.Payload, &hb); err != nil {
			return err
		}
		return a.AcceptHeartbeat(hb)
	})
	if err != nil {
		spansCancel()
		return err
	}
	a.eventCancels = []func(){spansCancel, hbCancel}
	a.eventsBackend = backend
	return nil
}
