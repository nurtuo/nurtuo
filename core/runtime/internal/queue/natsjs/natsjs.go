// SPDX-License-Identifier: AGPL-3.0-only

// Package natsjs implements queue.QueueBackend on an embedded NATS JetStream server.
//
// Each queue is a stream "Q_<token>" (subject "nq.<token>") and a dead letter stream
// "Q_<token>_DLQ" (subject "nqdlq.<token>"). Delivery is push-based (push consumers with a
// deliver subject); there is no polling. Messages that exhaust MaxDeliveries (on Nak) or are
// Term'd are copied to the DLQ stream and then terminated on the source stream.
// Server-side exhaustion (ack-wait timeouts, handler crash) is caught through the
// $JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES advisory and copied to the DLQ too (advisory.go).
package natsjs

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base32"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/nurtuo/nurtuo/core/runtime/internal/queue"
)

const (
	hdrKey         = "Nurtuo-Key"
	hdrPropPref    = "Nurtuo-P-"
	hdrReason      = "Nurtuo-Dlq-Reason"
	hdrAttempts    = "Nurtuo-Dlq-Attempts"
	hdrOrigID      = "Nurtuo-Orig-Id"
	defaultAckWait = 30 * time.Second
)

// Options configures the embedded server.
type Options struct {
	// StoreDir is the JetStream storage directory. Empty uses a temp dir removed on Close.
	StoreDir string
	// AckWait is the server-side redelivery timeout; zero means 30s.
	AckWait time.Duration
}

// Backend is the embedded JetStream queue backend.
type Backend struct {
	srv        *server.Server
	nc         *nats.Conn
	js         nats.JetStreamContext
	tmpDir     string
	mu         sync.Mutex
	defs       map[string]queue.QueueDef
	subs       map[*subscription]struct{}
	deliveryMu sync.Mutex // guards redelivery counts, ordering lanes and ordered consumer members
	redeliv    map[string]uint64
	lanes      map[laneKey]*keyLane
	ackWait    time.Duration
	advSub     *nats.Subscription
	closed     bool // guarded by mu
	handlers   sync.WaitGroup
	closeOnce  sync.Once
	closeErr   error
}

var _ queue.QueueBackend = (*Backend)(nil)

// New starts an embedded nats-server (no network listener) with JetStream enabled.
func New(o Options) (*Backend, error) {
	b := &Backend{defs: map[string]queue.QueueDef{}, subs: map[*subscription]struct{}{}, redeliv: map[string]uint64{}, lanes: map[laneKey]*keyLane{}}
	b.ackWait = o.AckWait
	if b.ackWait <= 0 {
		b.ackWait = defaultAckWait
	}
	dir := o.StoreDir
	if dir == "" {
		d, err := os.MkdirTemp("", "nurtuo-js-*")
		if err != nil {
			return nil, err
		}
		dir, b.tmpDir = d, d
	}
	srv, err := server.NewServer(&server.Options{
		ServerName: "nurtuo-embedded",
		DontListen: true,
		JetStream:  true,
		StoreDir:   dir,
		SyncAlways: o.StoreDir != "",
		NoLog:      true,
		NoSigs:     true,
	})
	if err != nil {
		b.cleanup()
		return nil, fmt.Errorf("natsjs: new server: %w", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(15 * time.Second) {
		srv.Shutdown()
		b.cleanup()
		return nil, errors.New("natsjs: server not ready")
	}
	nc, err := nats.Connect("", nats.InProcessServer(srv))
	if err != nil {
		srv.Shutdown()
		b.cleanup()
		return nil, fmt.Errorf("natsjs: connect: %w", err)
	}
	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		srv.Shutdown()
		b.cleanup()
		return nil, fmt.Errorf("natsjs: jetstream: %w", err)
	}
	b.srv, b.nc, b.js = srv, nc, js
	adv, err := nc.Subscribe(maxDeliveriesAdvisory, b.onMaxDeliveries)
	if err != nil {
		nc.Close()
		srv.Shutdown()
		b.cleanup()
		return nil, fmt.Errorf("natsjs: advisory subscribe: %w", err)
	}
	b.advSub = adv
	return b, nil
}

func (b *Backend) cleanup() {
	if b.tmpDir != "" {
		_ = os.RemoveAll(b.tmpDir)
	}
}

const maxTokenLen = server.JSMaxNameLen - len("Q__DLQ")
const identityKey = "nurtuo.identity"

func token(name string) string {
	if len(name) > maxTokenLen {
		return hashedToken(name)
	}
	// Preserve existing lowercase literal names. Reserve "~" for encoded
	// names and uppercase "_DLQ" for the dead-letter namespace.
	if name != "" && !strings.ContainsFunc(name, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_'
	}) {
		return name
	}
	encoding := base32.HexEncoding.WithPadding(base32.NoPadding)
	if 1+encoding.EncodedLen(len(name)) > maxTokenLen {
		return hashedToken(name)
	}
	return "~" + strings.ToLower(encoding.EncodeToString([]byte(name)))
}
func hashedToken(name string) string {
	return fmt.Sprintf("~~%x", sha256.Sum256([]byte(name)))
}

// Use an independent fingerprint to detect collisions, including after restart.
// Its size is bounded independently of the original name.
func identity(name, id string) map[string]string {
	if !strings.HasPrefix(id, "~~") {
		return nil
	}
	return map[string]string{identityKey: fmt.Sprintf("%x", sha512.Sum512([]byte(name)))}
}

func streamName(name string) string { return "Q_" + token(name) }
func dlqStream(name string) string  { return "Q_" + token(name) + "_DLQ" }
func subject(name string) string    { return "nq." + token(name) }
func dlqSubject(name string) string { return "nqdlq." + token(name) }

func (b *Backend) def(name string) (queue.QueueDef, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	d, ok := b.defs[name]
	if !ok {
		return d, fmt.Errorf("%w: queue %q", queue.ErrNotFound, name)
	}
	return d, nil
}

func maxDeliveries(d queue.QueueDef) int {
	if d.MaxDeliveries > 0 {
		return d.MaxDeliveries
	}
	return queue.DefaultMaxDeliveries
}

// Ensure creates or updates the queue's streams.
func (b *Backend) Ensure(_ context.Context, def queue.QueueDef) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	metadata := identity(def.Name, token(def.Name))
	cfg := &nats.StreamConfig{
		Metadata: metadata,
		Name:     streamName(def.Name),
		Subjects: []string{subject(def.Name)},
		Storage:  nats.FileStorage,
		MaxAge:   def.MaxAge,
		MaxBytes: -1,
		Discard:  nats.DiscardOld,
	}
	if def.MaxBytes > 0 {
		cfg.MaxBytes = def.MaxBytes
	}
	switch def.Retention {
	case queue.RetentionLimits:
		cfg.Retention = nats.LimitsPolicy
	case queue.RetentionInterest:
		cfg.Retention = nats.InterestPolicy
	case queue.RetentionWorkQueue, "":
		cfg.Retention = nats.WorkQueuePolicy
	default:
		return fmt.Errorf("natsjs: unknown retention %q", def.Retention)
	}
	if def.Retention == queue.RetentionWorkQueue || def.Retention == "" {
		cfg.Duplicates = 2 * time.Minute
	}
	dcfg := &nats.StreamConfig{
		Metadata: metadata,
		Name:     dlqStream(def.Name),
		Subjects: []string{dlqSubject(def.Name)},
		Storage:  nats.FileStorage,
		MaxAge:   def.MaxAge,
		MaxBytes: -1,
	}
	for _, c := range []*nats.StreamConfig{cfg, dcfg} {
		if info, err := b.js.StreamInfo(c.Name); err == nil {
			if info.Config.Metadata[identityKey] != metadata[identityKey] {
				return fmt.Errorf("natsjs: stream identity mismatch for %s", c.Name)
			}
			// Retention is immutable in JetStream; the other fields can be updated.
			if _, err := b.js.UpdateStream(c); err != nil && !strings.Contains(err.Error(), "retention") {
				return fmt.Errorf("natsjs: update stream: %w", err)
			}
		} else if errors.Is(err, nats.ErrStreamNotFound) {
			if _, err := b.js.AddStream(c); err != nil {
				return fmt.Errorf("natsjs: add stream: %w", err)
			}
		} else {
			return err
		}
	}
	b.defs[def.Name] = def
	return nil
}

// Remove deletes the queue and its dead letter stream.
func (b *Backend) Remove(_ context.Context, name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if metadata := identity(name, token(name)); metadata != nil {
		for _, stream := range []string{streamName(name), dlqStream(name)} {
			info, err := b.js.StreamInfo(stream)
			if errors.Is(err, nats.ErrStreamNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if info.Config.Metadata[identityKey] != metadata[identityKey] {
				return fmt.Errorf("natsjs: stream identity mismatch for %s", stream)
			}
		}
	}
	for s := range b.subs {
		if s.name == name {
			s.stop()
			delete(b.subs, s)
		}
	}
	delete(b.defs, name)
	for _, s := range []string{streamName(name), dlqStream(name)} {
		if err := b.js.DeleteStream(s); err != nil && !errors.Is(err, nats.ErrStreamNotFound) {
			return err
		}
	}
	return nil
}

func toNATS(subj string, m queue.Message) *nats.Msg {
	nm := nats.NewMsg(subj)
	nm.Data = m.Data
	if m.ID != "" {
		nm.Header.Set(nats.MsgIdHdr, m.ID)
		nm.Header.Set(hdrOrigID, m.ID)
	}
	if m.Key != "" {
		nm.Header.Set(hdrKey, m.Key)
	}
	for k, v := range m.Properties {
		nm.Header.Set(hdrPropPref+k, v)
	}
	return nm
}

func fromHeader(h nats.Header, data []byte) queue.Message {
	m := queue.Message{Data: data, Key: h.Get(hdrKey), ID: h.Get(hdrOrigID)}
	for k, v := range h {
		if strings.HasPrefix(k, hdrPropPref) && len(v) > 0 {
			if m.Properties == nil {
				m.Properties = map[string]string{}
			}
			m.Properties[strings.TrimPrefix(k, hdrPropPref)] = v[0]
		}
	}
	return m
}

// Publish stores a message; a non-empty ID de-duplicates within the stream's window.
func (b *Backend) Publish(ctx context.Context, name string, msg queue.Message) error {
	if _, err := b.def(name); err != nil {
		return err
	}
	_, err := b.js.PublishMsg(toNATS(subject(name), msg), nats.Context(ctx))
	return err
}

// Subscribe creates (or re-attaches to) a durable push consumer.
func (b *Backend) Subscribe(ctx context.Context, name string, opts queue.SubOpts, h queue.Handler) (queue.Subscription, error) {
	def, err := b.def(name)
	if err != nil {
		return nil, err
	}
	conc := opts.Concurrency
	if conc <= 0 {
		conc = def.Concurrency
	}
	if conc <= 0 {
		conc = 1
	}
	if def.Ordering == queue.OrderingStrict {
		conc = 1
	}
	durable := opts.Durable
	if durable == "" {
		durable = "default"
	}
	lifetime, cancel := context.WithCancel(ctx)
	s := &subscription{b: b, lifetime: lifetime, end: cancel, name: name, def: def, durable: token(durable), durableIdentity: identity(durable, token(durable)), conc: conc, h: h}
	if err := s.attach(); err != nil {
		cancel()
		return nil, err
	}
	return s, nil
}

type laneKey struct{ queue, durable, key string }

type keyLane struct{ pending []func() }

// An ordered consumer has one NATS callback so same-key deliveries enter
// their lane in broker delivery order, even when routed to different handlers.
type orderedConsumer struct {
	members []*subscription
	next    int
}

func (c *orderedConsumer) dispatch(m *nats.Msg, b *Backend) {
	b.deliveryMu.Lock()
	if len(c.members) == 0 {
		b.deliveryMu.Unlock()
		_ = m.Nak()
		return
	}
	s := c.members[c.next%len(c.members)]
	c.next = c.next%len(c.members) + 1
	s.dispatch(m)
	b.deliveryMu.Unlock()
}

type subscription struct {
	end             context.CancelFunc
	lifetime        context.Context
	ctx             context.Context    // guarded by deliveryMu
	cancel          context.CancelFunc // guarded by deliveryMu
	b               *Backend
	name            string
	def             queue.QueueDef
	durable         string
	durableIdentity map[string]string
	conc            int
	h               queue.Handler

	sub     *nats.Subscription // guarded by b.mu
	closed  bool               // guarded by b.mu
	ordered *orderedConsumer   // guarded by b.mu
}

func (s *subscription) attach() error {
	stream := streamName(s.name)
	// Serialize consumer setup when listeners start concurrently. Explicit creation
	// keeps the durable alive when any one subscriber pauses or closes.
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	if s.closed || s.b.closed {
		return errors.New("natsjs: subscription closed")
	}
	if err := s.lifetime.Err(); err != nil {
		return err
	}
	if s.sub != nil {
		return nil
	}
	var ordered *orderedConsumer
	if s.def.Ordering == queue.OrderingPerKey {
		for other := range s.b.subs {
			if other.name == s.name && other.durable == s.durable && other.ordered != nil {
				if other.durableIdentity[identityKey] != s.durableIdentity[identityKey] {
					return fmt.Errorf("natsjs: consumer identity mismatch for %s", s.durable)
				}
				s.sub, s.ordered = other.sub, other.ordered
				s.b.deliveryMu.Lock()
				s.ctx, s.cancel = context.WithCancel(s.lifetime)
				s.ordered.members = append(s.ordered.members, s)
				s.b.deliveryMu.Unlock()
				s.b.subs[s] = struct{}{}
				return nil
			}
		}
		ordered = &orderedConsumer{members: []*subscription{s}}
	}
	info, err := s.b.js.ConsumerInfo(stream, s.durable)
	switch {
	case errors.Is(err, nats.ErrConsumerNotFound):
		info, err = s.b.js.AddConsumer(stream, &nats.ConsumerConfig{
			Metadata: s.durableIdentity, Durable: s.durable, DeliverSubject: nats.NewInbox(), DeliverGroup: s.durable,
			FilterSubject: subject(s.name), AckPolicy: nats.AckExplicitPolicy,
			AckWait: s.b.ackWait, MaxDeliver: maxDeliveries(s.def), MaxAckPending: s.conc,
		})
	case err == nil && info.Config.Metadata[identityKey] != s.durableIdentity[identityKey]:
		return fmt.Errorf("natsjs: consumer identity mismatch for %s", s.durable)
	case err == nil && info.Config.DeliverGroup == "":
		// Upgrade the durable in place to preserve pending messages and ack state.
		cfg := info.Config
		cfg.DeliverGroup = s.durable
		if _, err = s.b.js.UpdateConsumer(stream, &cfg); err == nil {
			// Changing the subject refreshes JetStream's interest registration with the new group.
			cfg.DeliverSubject = nats.NewInbox()
			info, err = s.b.js.UpdateConsumer(stream, &cfg)
		}
	}
	if err != nil {
		return fmt.Errorf("natsjs: configure consumer: %w", err)
	}
	// Gate only this callback until the broker confirms the binding.
	ready := make(chan struct{})
	bound := false
	defer close(ready)
	cb := func(m *nats.Msg) {
		<-ready
		if !bound {
			_ = m.Nak()
			return
		}
		if ordered != nil {
			ordered.dispatch(m, s.b)
			return
		}
		s.b.deliveryMu.Lock()
		defer s.b.deliveryMu.Unlock()
		s.dispatch(m)
	}
	sub, err := s.b.js.QueueSubscribe(subject(s.name), info.Config.DeliverGroup, cb,
		nats.Bind(stream, s.durable), nats.ManualAck())
	if err != nil {
		return fmt.Errorf("natsjs: subscribe: %w", err)
	}
	if err := s.waitPushBound(true); err != nil {
		_ = sub.Unsubscribe()
		return err
	}
	s.b.deliveryMu.Lock()
	s.ctx, s.cancel = context.WithCancel(s.lifetime)
	s.b.deliveryMu.Unlock()
	s.sub, s.ordered = sub, ordered
	s.b.subs[s] = struct{}{}
	bound = true
	return nil
}

func (s *subscription) waitPushBound(want bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		info, err := s.b.js.ConsumerInfo(streamName(s.name), s.durable, nats.Context(ctx))
		if err != nil {
			return err
		}
		if info.PushBound == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("natsjs: wait for consumer %s binding: %w", s.durable, ctx.Err())
		case <-tick.C:
		}
	}
}

// dispatch runs on the NATS callback goroutine with deliveryMu held. It enqueues
// keyed handlers in delivery order; MaxAckPending bounds server-side concurrency.
func (s *subscription) dispatch(m *nats.Msg) {
	ctx := s.ctx
	if ctx == nil || ctx.Err() != nil {
		return
	}
	d := &delivery{s: s, m: m, msg: fromHeader(m.Header, m.Data)}
	if md, err := m.Metadata(); err == nil {
		d.attempt = int(md.NumDelivered)
		if d.attempt > 1 {
			s.b.redeliv[s.name]++
		}
	}
	run := func() {
		if ctx.Err() == nil {
			s.h(s.lifetime, d)
			return
		}
		// A paused subscription can no longer run a queued delivery. Let the
		// broker offer it again. On shutdown, leave it pending for recovery.
		if s.lifetime.Err() == nil {
			_ = m.Nak()
		}
	}
	key := laneKey{s.name, s.durable, d.msg.Key}
	if s.def.Ordering != queue.OrderingPerKey || key.key == "" {
		s.b.handlers.Add(1)
		go func() {
			defer s.b.handlers.Done()
			run()
		}()
		return
	}
	lane, busy := s.b.lanes[key]
	if busy {
		lane.pending = append(lane.pending, run)
		return
	}
	lane = &keyLane{}
	s.b.lanes[key] = lane
	s.b.handlers.Add(1)
	go func() {
		defer s.b.handlers.Done()
		run()
		for {
			s.b.deliveryMu.Lock()
			if len(lane.pending) == 0 {
				delete(s.b.lanes, key)
				s.b.deliveryMu.Unlock()
				return
			}
			next := lane.pending[0]
			lane.pending[0] = nil
			lane.pending = lane.pending[1:]
			s.b.deliveryMu.Unlock()
			next()
		}
	}()
}

// stop is called with the backend lock held when shutting down or deleting a queue.
func (s *subscription) stop() {
	s.end()
	s.closed = true
	if sub := s.release(); sub != nil {
		_ = sub.Unsubscribe() // durable consumer is kept on the server
	}
}

// Pause detaches from the durable consumer; messages stay queued server-side.
func (s *subscription) Pause() error {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	return s.detach()
}

// detach is called with the backend lock held.
func (s *subscription) detach() error {
	sub := s.release()
	if sub == nil {
		return nil
	}
	if err := sub.Unsubscribe(); err != nil {
		return err
	}
	for other := range s.b.subs {
		if other != s && other.name == s.name && other.durable == s.durable && other.sub != nil {
			return nil
		}
	}
	// Wait until JetStream sees the last subscriber leave. Otherwise a publish
	// immediately after Pause can race with interest removal and miss delivery.
	return s.waitPushBound(false)
}

// release removes the local handler and returns the NATS subscription
// if no other handler owns it. The caller holds b.mu.
func (s *subscription) release() *nats.Subscription {
	sub, ordered := s.sub, s.ordered
	s.sub, s.ordered = nil, nil
	s.b.deliveryMu.Lock()
	defer s.b.deliveryMu.Unlock()
	if s.cancel != nil {
		s.cancel()
		s.ctx, s.cancel = nil, nil
	}
	if ordered != nil {
		ordered.members = slices.DeleteFunc(ordered.members, func(other *subscription) bool { return other == s })
		if len(ordered.members) > 0 {
			return nil
		}
	}
	return sub
}

// Resume re-attaches to the durable consumer.
func (s *subscription) Resume() error { return s.attach() }

// Close detaches permanently (the durable consumer remains).
func (s *subscription) Close() error {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	s.end()
	err := s.detach()
	s.closed = true
	delete(s.b.subs, s)
	return err
}

type delivery struct {
	s       *subscription
	m       *nats.Msg
	msg     queue.Message
	attempt int
}

func (d *delivery) Message() queue.Message { return d.msg }
func (d *delivery) Attempt() int           { return d.attempt }
func (d *delivery) Ack(context.Context) error {
	return d.m.AckSync()
}

func (d *delivery) Nak(_ context.Context, delay time.Duration) error {
	if d.attempt >= maxDeliveries(d.s.def) {
		return d.deadLetter("max deliveries exceeded")
	}
	if delay == 0 {
		if bo := d.s.def.Backoff; len(bo) > 0 {
			i := d.attempt - 1
			if i >= len(bo) {
				i = len(bo) - 1
			}
			delay = bo[i]
		}
	}
	if delay > 0 {
		return d.m.NakWithDelay(delay)
	}
	return d.m.Nak()
}

func (d *delivery) Term(context.Context) error { return d.deadLetter("terminated") }

func (d *delivery) deadLetter(reason string) error {
	nm := toNATS(dlqSubject(d.s.name), d.msg)
	nm.Header.Del(nats.MsgIdHdr)
	nm.Header.Set(hdrReason, reason)
	nm.Header.Set(hdrAttempts, strconv.Itoa(d.attempt))
	if _, err := d.s.b.js.PublishMsg(nm); err != nil {
		return fmt.Errorf("natsjs: publish dlq: %w", err)
	}
	return d.m.Term()
}

// Stats reports depth (undelivered + in flight), redeliveries, oldest age and DLQ depth.
func (b *Backend) Stats(_ context.Context, name string) (queue.QueueStats, error) {
	def, err := b.def(name)
	if err != nil {
		return queue.QueueStats{}, err
	}
	si, err := b.js.StreamInfo(streamName(name))
	if err != nil {
		return queue.QueueStats{}, err
	}
	st := queue.QueueStats{Depth: si.State.Msgs}
	if def.Retention == queue.RetentionLimits {
		// Limits streams keep acked messages; depth is what the consumers still owe.
		var depth uint64
		seen := false
		for ci := range b.js.Consumers(streamName(name)) {
			seen = true
			if p := uint64(ci.NumPending) + uint64(ci.NumAckPending); p > depth {
				depth = p
			}
		}
		if seen {
			st.Depth = depth
		}
	}
	if st.Depth > 0 && si.State.Msgs > 0 {
		st.OldestMessageAge = time.Since(si.State.FirstTime)
	}
	b.deliveryMu.Lock()
	st.Redeliveries = b.redeliv[name]
	b.deliveryMu.Unlock()
	if di, err := b.js.StreamInfo(dlqStream(name)); err == nil {
		st.DeadLetterDepth = di.State.Msgs
	}
	return st, nil
}

// ListDeadLetters returns up to limit parked messages, oldest first.
func (b *Backend) ListDeadLetters(_ context.Context, name string, limit int) ([]queue.DeadLetter, error) {
	if _, err := b.def(name); err != nil {
		return nil, err
	}
	di, err := b.js.StreamInfo(dlqStream(name))
	if err != nil {
		return nil, err
	}
	var out []queue.DeadLetter
	if di.State.Msgs == 0 {
		return out, nil
	}
	for seq := di.State.FirstSeq; seq <= di.State.LastSeq; seq++ {
		if limit > 0 && len(out) >= limit {
			break
		}
		raw, err := b.js.GetMsg(dlqStream(name), seq)
		if err != nil {
			continue // deleted (replayed) gap
		}
		att, _ := strconv.Atoi(raw.Header.Get(hdrAttempts))
		out = append(out, queue.DeadLetter{
			ID:       strconv.FormatUint(seq, 10),
			Message:  fromHeader(raw.Header, raw.Data),
			Reason:   raw.Header.Get(hdrReason),
			Attempts: att,
			At:       raw.Time,
		})
	}
	return out, nil
}

// ReplayDeadLetter republishes a dead letter to the queue and removes it from the DLQ.
func (b *Backend) ReplayDeadLetter(ctx context.Context, name, id string) error {
	if _, err := b.def(name); err != nil {
		return err
	}
	seq, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: dead letter %q", queue.ErrNotFound, id)
	}
	raw, err := b.js.GetMsg(dlqStream(name), seq)
	if err != nil {
		return fmt.Errorf("%w: dead letter %q", queue.ErrNotFound, id)
	}
	m := fromHeader(raw.Header, raw.Data)
	nm := toNATS(subject(name), m)
	nm.Header.Del(nats.MsgIdHdr) // a replay must not be de-duplicated against the original
	if _, err := b.js.PublishMsg(nm, nats.Context(ctx)); err != nil {
		return err
	}
	return b.js.DeleteMsg(dlqStream(name), seq)
}

// Close stops delivery and allows up to ten seconds for handlers to exit.
func (b *Backend) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return b.CloseContext(ctx)
}

// CloseContext cancels delivery, waits for handlers until ctx expires, then
// closes the broker. Handlers must observe cancellation to finish on time.
func (b *Backend) CloseContext(ctx context.Context) error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		for s := range b.subs {
			s.stop()
		}
		b.subs = nil
		b.mu.Unlock()

		done := make(chan struct{})
		go func() {
			b.handlers.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			b.closeErr = ctx.Err()
		}

		b.nc.Close()
		b.srv.Shutdown()
		b.srv.WaitForShutdown()
		b.cleanup()
	})
	return b.closeErr
}
