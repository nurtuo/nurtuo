// SPDX-License-Identifier: AGPL-3.0-only

package listener

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/nurtuo/nurtuo/core/runtime/internal/engine"
	"github.com/nurtuo/nurtuo/core/runtime/internal/engine/docs"
	"github.com/nurtuo/nurtuo/core/runtime/internal/queue"
	"github.com/nurtuo/nurtuo/core/runtime/internal/secrets"
	"github.com/nurtuo/nurtuo/sdk/go/flow"
)

// FactoryDeps are the shared resources listeners are built on.
type FactoryDeps struct {
	Queues   queue.QueueBackend // required for queue starts
	Spool    *docs.Spool        // payload spool; default in-memory-first
	HTTPAddr string             // HTTP listen address; default 127.0.0.1:0
	// Secrets resolves secret:// references in a Start step's auth and tls settings (in the
	// runtime: the connections.Resolver over the configured secret provider).
	Secrets SecretResolver
}

// SecretResolver resolves one secret://<name> reference.
type SecretResolver interface {
	ResolveRef(ctx context.Context, ref string) (secrets.Secret, error)
}

// StartOf returns the typed config of f's Start step when it is a listener or queue start.
func StartOf(f *flow.Flow) (flow.StartConfig, bool, error) {
	for _, s := range f.Steps {
		if s.Kind != flow.KindStart {
			continue
		}
		cfg, err := s.TypedConfig()
		if err != nil {
			return flow.StartConfig{}, false, err
		}
		sc, ok := cfg.(flow.StartConfig)
		if !ok {
			return flow.StartConfig{}, false, fmt.Errorf("listener: unexpected start config %T", cfg)
		}
		return sc, IsListenerStart(sc), nil
	}
	return flow.StartConfig{}, false, errors.New("listener: flow has no start step")
}

// FromStart builds the Listener a Start step describes. {"type":"listener","connector":"http"}
// (or an empty connector) becomes an HTTP listener copying path/method; {"type":"queue"} becomes
// a queue Source on deps.Queues wrapped by NewSourceListener. Other connector listeners are
// bridge listeners and are not built here.
func FromStart(ctx context.Context, sc flow.StartConfig, deps FactoryDeps) (Listener, error) {
	switch sc.Type {
	case StartListener:
		if sc.Connector != "" && sc.Connector != "http" {
			return nil, fmt.Errorf("listener: connector %q listeners are not supported by the factory", sc.Connector)
		}
		cfg, err := httpConfig(ctx, sc, deps)
		if err != nil {
			return nil, err
		}
		return NewHTTP(cfg)
	case StartQueue:
		if sc.Queue == "" {
			return nil, errors.New("listener: queue start requires a queue")
		}
		if deps.Queues == nil {
			return nil, errors.New("listener: no queue backend configured")
		}
		sp := deps.Spool
		if sp == nil {
			sp = docs.NewSpool(docs.SpoolConfig{})
		}
		return NewSourceListener(&QueueSource{Backend: deps.Queues, Queue: sc.Queue, Spool: sp}), nil
	}
	return nil, fmt.Errorf("listener: start type %q is not a listener", sc.Type)
}

// httpConfig builds the HTTP listener config of sc, resolving its auth and TLS secrets.
func httpConfig(ctx context.Context, sc flow.StartConfig, deps FactoryDeps) (HTTPConfig, error) {
	cfg := HTTPConfig{Addr: deps.HTTPAddr, Path: sc.Path, Method: sc.Method, Spool: deps.Spool, Insecure: sc.Insecure}
	if (sc.Auth != nil || sc.TLS != nil) && deps.Secrets == nil {
		return cfg, errors.New("listener: auth/tls need a secret provider, none is configured")
	}
	if a := sc.Auth; a != nil {
		s, err := deps.Secrets.ResolveRef(ctx, a.Secret)
		if err != nil {
			return cfg, fmt.Errorf("listener: auth: %w", err)
		}
		cfg.Auth = &HTTPAuth{Type: a.Type, Secret: []byte(s.Reveal()), Header: a.Header,
			Algorithm: a.Algorithm, Encoding: a.Encoding, Prefix: a.Prefix}
	}
	if t := sc.TLS; t != nil {
		cert, err := deps.Secrets.ResolveRef(ctx, t.Cert)
		if err != nil {
			return cfg, fmt.Errorf("listener: tls cert: %w", err)
		}
		key, err := deps.Secrets.ResolveRef(ctx, t.Key)
		if err != nil {
			return cfg, fmt.Errorf("listener: tls key: %w", err)
		}
		pair, err := tls.X509KeyPair([]byte(cert.Reveal()), []byte(key.Reveal()))
		if err != nil {
			return cfg, errors.New("listener: tls: cannot load certificate/key pair (want PEM)") // never wrap: may quote key material
		}
		cfg.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}
	}
	return cfg, nil
}

// ForFlow returns a Desired entry (for Manager.Apply) that runs f on every trigger of its Start
// step: the factory builds the listener and the handler runs eng.Run(f, in). Flows whose start
// is not a listener or queue (e.g. manual/cron) return ok=false.
func ForFlow(key, spec string, eng *engine.Engine, f *flow.Flow, deps FactoryDeps) (Desired, bool, error) {
	sc, ok, err := StartOf(f)
	if err != nil || !ok {
		return Desired{}, false, err
	}
	return Desired{Key: key, Spec: spec, Factory: func() (Listener, Handler, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		l, err := FromStart(ctx, sc, deps)
		if err != nil {
			return nil, nil, err
		}
		return l, FlowHandler(eng, f), nil
	}}, true, nil
}

// QueueSource is a Source that consumes a queue.QueueBackend queue. A delivery is acked when the
// flow handler succeeds and nacked (redelivered, eventually dead-lettered by the backend's
// max-deliveries policy) when it returns an error, which the listener does for any failed document.
type QueueSource struct {
	Backend queue.QueueBackend
	Queue   string
	Durable string // default "nurtuo-<queue>"
	Spool   *docs.Spool
	seq     atomic.Int64
}

// Consume subscribes and blocks until ctx is cancelled.
func (s *QueueSource) Consume(ctx context.Context, deliver func(ctx context.Context, in *docs.Set) error) error {
	durable := s.Durable
	if durable == "" {
		durable = "nurtuo-" + s.Queue
	}
	sp := s.Spool
	if sp == nil {
		sp = docs.NewSpool(docs.SpoolConfig{})
	}
	sub, err := s.Backend.Subscribe(ctx, s.Queue, queue.SubOpts{Durable: durable}, func(hctx context.Context, d queue.Delivery) {
		m := d.Message()
		payload, err := sp.FromBytes(m.Data)
		if err != nil {
			_ = d.Nak(hctx, 0)
			return
		}
		id := m.ID
		if id == "" {
			id = "q-" + strconv.FormatInt(s.seq.Add(1), 10)
		}
		props := make(map[string]string, len(m.Properties)+1)
		for k, v := range m.Properties {
			props[k] = v
		}
		if m.Key != "" {
			props["queue.key"] = m.Key
		}
		if err := deliver(hctx, docs.NewSet(docs.New(id, props, payload))); err != nil {
			_ = d.Nak(hctx, 0)
			return
		}
		_ = d.Ack(hctx)
	})
	if err != nil {
		return err
	}
	<-ctx.Done()
	return sub.Close()
}
