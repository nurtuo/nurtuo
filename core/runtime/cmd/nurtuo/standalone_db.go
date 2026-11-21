// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"fmt"
	"net"
	"net/url"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

// Database is the Postgres the control plane runs against.
type Database interface {
	Start(ctx context.Context) error
	DSN() string
	Stop(ctx context.Context) error
}

const embeddedUser = "nurtuo"

type embeddedDB struct {
	dirs DataDirs
	port uint32
	pass string
	pg   *embeddedpostgres.EmbeddedPostgres
}

// NewEmbeddedDatabase returns a Database backed by embedded-postgres (downloads and runs
// real Postgres binaries on Start). port 0 picks a free port.
func NewEmbeddedDatabase(dirs DataDirs, port uint32, password string) Database {
	return &embeddedDB{dirs: dirs, port: port, pass: password}
}

func freePort() (uint32, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return uint32(l.Addr().(*net.TCPAddr).Port), nil
}

func (e *embeddedDB) Start(ctx context.Context) error {
	if e.port == 0 {
		p, err := freePort()
		if err != nil {
			return err
		}
		e.port = p
	}
	cfg := embeddedpostgres.DefaultConfig().
		Username(embeddedUser).Password(e.pass).Database("nurtuo").
		Port(e.port).
		DataPath(e.dirs.Postgres).RuntimePath(e.dirs.PGRun).CachePath(e.dirs.PGCache).
		BinariesPath(e.dirs.PGRun)
	e.pg = embeddedpostgres.NewDatabase(cfg)
	if err := e.pg.Start(); err != nil {
		e.pg = nil
		return fmt.Errorf("start embedded postgres: %w", err)
	}
	return nil
}

func (e *embeddedDB) DSN() string {
	u := url.URL{
		Scheme: "postgres", User: url.UserPassword(embeddedUser, e.pass),
		Host: fmt.Sprintf("127.0.0.1:%d", e.port), Path: "/nurtuo", RawQuery: "sslmode=disable",
	}
	return u.String()
}

func (e *embeddedDB) Stop(context.Context) error {
	if e.pg == nil {
		return nil
	}
	err := e.pg.Stop()
	e.pg = nil
	return err
}
