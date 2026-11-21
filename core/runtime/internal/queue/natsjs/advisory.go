// SPDX-License-Identifier: AGPL-3.0-only

package natsjs

import (
	"encoding/json"
	"strconv"

	"github.com/nats-io/nats.go"
)

// maxDeliveriesAdvisory is published by the server when a consumer gives up on a message
// (MaxDeliver reached through ack-wait timeouts, e.g. a handler crash or hang). The Nak path
// dead-letters on its own; this closes the gap for deliveries that never got settled.
const maxDeliveriesAdvisory = "$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.>"

const reasonServerMaxDeliveries = "max deliveries exceeded (ack timeout)"

type maxDeliveriesEvent struct {
	Stream     string `json:"stream"`
	Consumer   string `json:"consumer"`
	StreamSeq  uint64 `json:"stream_seq"`
	Deliveries uint64 `json:"deliveries"`
}

// onMaxDeliveries copies the exhausted message to the queue's DLQ and removes it from the
// source stream so it does not linger as an unacked work item.
func (b *Backend) onMaxDeliveries(m *nats.Msg) {
	var ev maxDeliveriesEvent
	if err := json.Unmarshal(m.Data, &ev); err != nil {
		return
	}
	b.mu.Lock()
	var queueName string
	for name := range b.defs {
		if streamName(name) == ev.Stream {
			queueName = name
			break
		}
	}
	b.mu.Unlock()
	if queueName == "" {
		return // not one of our queues
	}
	raw, err := b.js.GetMsg(ev.Stream, ev.StreamSeq)
	if err != nil {
		return // already gone (e.g. dead-lettered through the Nak path)
	}
	nm := nats.NewMsg(dlqSubject(queueName))
	nm.Data = raw.Data
	for k, v := range raw.Header {
		nm.Header[k] = append([]string(nil), v...)
	}
	nm.Header.Del(nats.MsgIdHdr)
	nm.Header.Set(hdrReason, reasonServerMaxDeliveries)
	nm.Header.Set(hdrAttempts, strconv.FormatUint(ev.Deliveries, 10))
	if _, err := b.js.PublishMsg(nm); err != nil {
		return // leave the message in place; operators can still see it via Stats
	}
	_ = b.js.DeleteMsg(ev.Stream, ev.StreamSeq)
}
