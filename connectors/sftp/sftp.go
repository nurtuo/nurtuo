// SPDX-License-Identifier: Apache-2.0

// Package sftp is the native SFTP connector.
//
// Operations (selected by ExecuteStart): QUERY lists a directory, GET downloads a file,
// CREATE/UPSERT uploads a file, DELETE removes a file, EXECUTE/RENAME renames or moves one.
// Each input document carries its parameters as properties ("path", and "newPath" for RENAME);
// for uploads the document payload is the file content. Uploads and downloads are streamed in
// chunks and never buffered whole. Every document gets exactly one DocumentResult.
package sftp

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strings"
	"time"

	"github.com/nurtuo/nurtuo/sdk/go/connector"
	connectorv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/connector/v1"
	pkgsftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/types/known/structpb"
)

//go:embed schemas/connection.json
var connectionSchema []byte

// ChunkSize is the size of output chunks when downloading.
const ChunkSize = 64 * 1024

// Connector implements connector.Connector for SFTP.
type Connector struct{}

var _ connector.Connector = (*Connector)(nil)

// New returns the SFTP connector.
func New() *Connector { return &Connector{} }

// Describe returns the descriptor (mirrors connector.yaml).
func (*Connector) Describe(context.Context) (*connectorv1.Descriptor, error) {
	schema := &structpb.Struct{}
	if err := schema.UnmarshalJSON(connectionSchema); err != nil {
		return nil, err
	}
	t := connectorv1.OperationType_OPERATION_TYPE_QUERY
	return &connectorv1.Descriptor{
		Id:               "sftp",
		Version:          "0.1.0",
		Host:             connectorv1.Host_HOST_GO_NATIVE,
		DisplayName:      "SFTP",
		ConnectionSchema: schema,
		Operations: []*connectorv1.OperationDescriptor{
			{Type: t},
			{Type: connectorv1.OperationType_OPERATION_TYPE_GET},
			{Type: connectorv1.OperationType_OPERATION_TYPE_CREATE},
			{Type: connectorv1.OperationType_OPERATION_TYPE_UPSERT},
			{Type: connectorv1.OperationType_OPERATION_TYPE_DELETE},
			{Type: connectorv1.OperationType_OPERATION_TYPE_EXECUTE, CustomType: "RENAME"},
		},
		TrackedProperties: []string{"path", "size", "truncated"},
		Capabilities:      &connectorv1.Capabilities{Streaming: true},
	}, nil
}

// Browse is not supported by the SFTP connector.
func (*Connector) Browse(context.Context, *connectorv1.BrowseRequest) (*connectorv1.BrowseResponse, error) {
	return nil, connector.ErrNotImplemented
}

type session struct {
	cfg *config
	ssh *ssh.Client
	fs  *pkgsftp.Client
}

func (s *session) Close() {
	_ = s.fs.Close()
	_ = s.ssh.Close()
}

// dial connects and handshakes, honouring ctx cancellation and cfg.Timeout for the whole
// connect + SSH handshake.
func dial(ctx context.Context, cfg *config) (*session, error) {
	sc, err := cfg.sshConfig()
	if err != nil {
		return nil, err
	}
	nc, err := (&net.Dialer{Timeout: cfg.Timeout}).DialContext(ctx, "tcp", cfg.addr())
	if err != nil {
		return nil, fmt.Errorf("ssh connect: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = nc.Close() })
	_ = nc.SetDeadline(time.Now().Add(cfg.Timeout))
	sshConn, chans, reqs, err := ssh.NewClientConn(nc, cfg.addr(), sc)
	if err != nil {
		stop()
		_ = nc.Close()
		if cerr := ctx.Err(); cerr != nil {
			return nil, fmt.Errorf("ssh connect: %w", cerr)
		}
		return nil, fmt.Errorf("ssh connect: %w", err)
	}
	if !stop() { // ctx was cancelled and the connection closed under us
		_ = sshConn.Close()
		return nil, fmt.Errorf("ssh connect: %w", ctx.Err())
	}
	_ = nc.SetDeadline(time.Time{})
	c := ssh.NewClient(sshConn, chans, reqs)
	fs, err := pkgsftp.NewClient(c)
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("sftp subsystem: %w", err)
	}
	return &session{cfg: cfg, ssh: c, fs: fs}, nil
}

func redact(msg string, secrets []string) string {
	for _, s := range secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "***")
		}
	}
	return msg
}

// Test connects, authenticates and stats the root path. The diagnostic never contains secrets.
func (*Connector) Test(ctx context.Context, req *connectorv1.TestRequest) (*connectorv1.TestResponse, error) {
	cfg, err := parseConfig(req.GetConnection().GetSettings())
	if err != nil {
		return &connectorv1.TestResponse{Diagnostic: redact(err.Error(), nil)}, nil
	}
	s, err := dial(ctx, cfg)
	if err != nil {
		return &connectorv1.TestResponse{Diagnostic: redact(err.Error(), cfg.secrets())}, nil
	}
	defer s.Close()
	if _, err := s.fs.Stat(cfg.remote("/")); err != nil {
		return &connectorv1.TestResponse{Diagnostic: redact("root path: "+err.Error(), cfg.secrets())}, nil
	}
	return &connectorv1.TestResponse{Ok: true, Diagnostic: "connected to " + cfg.addr()}, nil
}

// isApplicationError reports errors caused by the target system's state (missing file,
// permissions) as opposed to technical failures.
func isApplicationError(err error) bool {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrExist) || errors.Is(err, errBadInput) {
		return true
	}
	var se *pkgsftp.StatusError
	if errors.As(err, &se) {
		switch se.Code {
		case 2, 3, 4: // no such file, permission denied, generic failure
			return true
		}
	}
	return false
}

var errBadInput = errors.New("invalid document")

type docState struct {
	id    string
	props map[string]string
	// upload state
	w       *pkgsftp.File
	tmp     string
	dest    string
	failure error
	create  bool // CREATE: fail if the destination exists
}

// Execute runs one batch. See the package comment for the document conventions.
func (*Connector) Execute(stream connectorv1.ConnectorService_ExecuteServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil {
		return errors.New("sftp: first message must be ExecuteStart")
	}
	cfg, err := parseConfig(start.GetConnection().GetSettings())
	if err != nil {
		return err
	}
	op, err := resolveOp(start)
	if err != nil {
		return err
	}
	s, err := dial(stream.Context(), cfg)
	if err != nil {
		return redactErr(err, cfg)
	}
	defer s.Close()
	dir := str(start.GetOptions(), "directory")

	isPut := op == opPut || op == opCreate
	var cur *docState
	// Every document gets exactly one result: a document that never reaches its last chunk is
	// aborted (handle closed, .part removed) and reported as FAILURE.
	incomplete := func() error {
		if cur == nil {
			return nil
		}
		d := cur
		cur = nil
		s.abortUpload(d)
		return stream.Send(&connectorv1.ExecuteResponse{Msg: &connectorv1.ExecuteResponse_Result{Result: &connectorv1.DocumentResult{
			DocumentId: d.id, Status: connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE,
			Message: "document incomplete: no final chunk received",
		}}})
	}
	defer func() {
		if cur != nil {
			s.abortUpload(cur)
		}
	}()
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if req.GetEnd() != nil {
			break
		}
		ch := req.GetChunk()
		if ch == nil {
			continue
		}
		if cur != nil && cur.id != ch.GetDocumentId() {
			if err := incomplete(); err != nil {
				return err
			}
		}
		if cur == nil {
			cur = &docState{id: ch.GetDocumentId(), props: ch.GetProperties(), create: op == opCreate}
			if isPut {
				s.openUpload(cur, dir)
			}
		}
		if isPut && cur.failure == nil && len(ch.GetData()) > 0 {
			if _, err := cur.w.Write(ch.GetData()); err != nil {
				cur.failure = err
				s.abortUpload(cur)
			}
		}
		if ch.GetLast() {
			d := cur
			cur = nil
			res := s.finish(stream, op, d, dir)
			if err := stream.Send(&connectorv1.ExecuteResponse{Msg: &connectorv1.ExecuteResponse_Result{Result: res}}); err != nil {
				s.abortUpload(d)
				return err
			}
		}
	}
	return incomplete()
}

func redactErr(err error, cfg *config) error {
	return errors.New(redact(err.Error(), cfg.secrets()))
}

type opKind int

const (
	opList opKind = iota
	opGet
	opPut // UPSERT: replaces an existing destination
	opCreate
	opDelete
	opRename
)

func resolveOp(st *connectorv1.ExecuteStart) (opKind, error) {
	switch st.GetOperationType() {
	case connectorv1.OperationType_OPERATION_TYPE_QUERY:
		return opList, nil
	case connectorv1.OperationType_OPERATION_TYPE_GET:
		return opGet, nil
	case connectorv1.OperationType_OPERATION_TYPE_CREATE:
		return opCreate, nil
	case connectorv1.OperationType_OPERATION_TYPE_UPSERT:
		return opPut, nil
	case connectorv1.OperationType_OPERATION_TYPE_DELETE:
		return opDelete, nil
	case connectorv1.OperationType_OPERATION_TYPE_EXECUTE:
		if strings.EqualFold(st.GetCustomType(), "RENAME") {
			return opRename, nil
		}
	}
	return 0, fmt.Errorf("sftp: unsupported operation %v %q", st.GetOperationType(), st.GetCustomType())
}

// errOutsideRoot marks a path that resolves (through symlinks) outside the configured root.
func errOutsideRoot(p string) error {
	return fmt.Errorf("%w: %s resolves outside the root path", errBadInput, p)
}

const maxSymlinkHops = 40

// walk resolves p (absolute) component by component with Lstat/Readlink, following symlinks.
// Components that do not exist are appended verbatim (they cannot be links).
func (s *session) walk(p string) (string, error) {
	cur := "/"
	parts := splitPath(p)
	hops := 0
	for len(parts) > 0 {
		c := parts[0]
		parts = parts[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			cur = path.Dir(cur)
			continue
		}
		next := path.Join(cur, c)
		fi, err := s.fs.Lstat(next)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return path.Join(append([]string{next}, parts...)...), nil
			}
			return "", err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			if hops++; hops > maxSymlinkHops {
				return "", fmt.Errorf("%w: too many symbolic links", errBadInput)
			}
			tgt, err := s.fs.ReadLink(next)
			if err != nil {
				return "", err
			}
			if path.IsAbs(tgt) {
				cur = "/"
			}
			parts = append(splitPath(tgt), parts...)
			continue
		}
		cur = next
	}
	return cur, nil
}

func splitPath(p string) []string { return strings.Split(strings.ReplaceAll(p, "\\", "/"), "/") }

func within(root, p string) bool {
	return root == "/" || p == root || strings.HasPrefix(p, root+"/")
}

// confine maps a client path below the root and resolves symlinks so a link inside the root that
// points outside is refused. The root itself is refused unless allowRoot. (There is an unavoidable
// check-then-use window; the SFTP protocol has no openat.)
func (s *session) confine(p string, allowRoot bool) (string, error) {
	lex := s.cfg.remote(p)
	root := path.Clean(s.cfg.Root)
	realRoot, err := s.walk(root)
	if err != nil {
		return "", err
	}
	real, err := s.walk(lex)
	if err != nil {
		return "", err
	}
	if !within(realRoot, real) {
		return "", errOutsideRoot(p)
	}
	if !allowRoot && (real == realRoot || lex == root) {
		return "", fmt.Errorf("%w: refusing to operate on the root path itself", errBadInput)
	}
	return real, nil
}

func (s *session) target(d *docState, dir string, allowRoot bool) (string, error) {
	p := d.props["path"]
	if p == "" && d.props["filename"] != "" && dir != "" {
		p = path.Join(dir, path.Base(d.props["filename"]))
	}
	if p == "" {
		return "", fmt.Errorf("%w: property \"path\" is required", errBadInput)
	}
	return s.confine(p, allowRoot)
}

// maxTempBase bounds the destination-derived part of temp names.
const maxTempBase = 64

func tempName(dest string) string {
	var r [4]byte
	_, _ = rand.Read(r[:])
	base := path.Base(dest)
	if len(base) > maxTempBase {
		base = base[:maxTempBase]
	}
	return ".nurtuo-" + base + "." + hex.EncodeToString(r[:]) + ".part"
}

func isTempName(n string) bool {
	return strings.HasPrefix(n, ".nurtuo-") && strings.HasSuffix(n, ".part")
}

func (s *session) openUpload(d *docState, dir string) {
	dest, err := s.target(d, dir, false)
	if err != nil {
		d.failure = err
		return
	}
	d.dest = dest
	if d.create {
		if _, err := s.fs.Lstat(dest); err == nil {
			d.failure = fmt.Errorf("%w: %s", os.ErrExist, d.props["path"])
			return
		}
	}
	_ = s.fs.MkdirAll(path.Dir(dest)) // best effort; a real failure surfaces on open
	d.tmp = path.Join(path.Dir(dest), tempName(dest))
	d.w, d.failure = s.fs.OpenFile(d.tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
}

func (s *session) abortUpload(d *docState) {
	if d.w != nil {
		_ = d.w.Close()
		_ = s.fs.Remove(d.tmp)
		d.w = nil
	}
}

func (s *session) finish(stream connectorv1.ConnectorService_ExecuteServer, op opKind, d *docState, dir string) *connectorv1.DocumentResult {
	res := &connectorv1.DocumentResult{DocumentId: d.id, Tracked: map[string]string{}}
	var err error
	switch op {
	case opPut, opCreate:
		err = s.finishPut(d, res)
	case opList:
		err = s.list(stream, d, dir, res)
	case opGet:
		err = s.get(stream, d, dir, res)
	case opDelete:
		var p string
		if p, err = s.target(d, dir, false); err == nil {
			err = s.fs.Remove(p)
			res.Tracked["path"] = d.props["path"]
		}
	case opRename:
		var p string
		if p, err = s.target(d, dir, false); err == nil {
			np := d.props["newPath"]
			if np == "" {
				err = fmt.Errorf("%w: property \"newPath\" is required", errBadInput)
			} else {
				var q string
				if q, err = s.confine(np, false); err == nil {
					err = s.rename(p, q)
					res.Tracked["path"] = d.props["path"]
				}
			}
		}
	}
	switch {
	case err == nil:
		res.Status = connectorv1.DocumentStatus_DOCUMENT_STATUS_SUCCESS
	case isApplicationError(err):
		res.Status = connectorv1.DocumentStatus_DOCUMENT_STATUS_APPLICATION_ERROR
		res.Message = redact(err.Error(), s.cfg.secrets())
	default:
		res.Status = connectorv1.DocumentStatus_DOCUMENT_STATUS_FAILURE
		res.Message = redact(err.Error(), s.cfg.secrets())
	}
	return res
}

func (s *session) finishPut(d *docState, res *connectorv1.DocumentResult) error {
	if d.failure != nil {
		s.abortUpload(d)
		return d.failure
	}
	if d.w == nil {
		return d.failure
	}
	st, _ := d.w.Stat()
	if err := d.w.Close(); err != nil {
		_ = s.fs.Remove(d.tmp)
		return err
	}
	d.w = nil
	var err error
	if d.create {
		// SFTP rename never replaces an existing destination, which is what CREATE needs.
		if err = s.fs.Rename(d.tmp, d.dest); err != nil {
			if _, serr := s.fs.Lstat(d.dest); serr == nil {
				err = fmt.Errorf("%w: %s", os.ErrExist, d.props["path"])
			}
		}
	} else {
		err = s.rename(d.tmp, d.dest)
	}
	if err != nil {
		_ = s.fs.Remove(d.tmp)
		return err
	}
	res.Tracked["path"] = d.props["path"]
	if st != nil {
		res.Tracked["size"] = fmt.Sprint(st.Size())
	}
	return nil
}

type entry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"isDir"`
	ModTime time.Time `json:"modTime"`
	Mode    string    `json:"mode"`
}

func (s *session) list(stream connectorv1.ConnectorService_ExecuteServer, d *docState, dir string, res *connectorv1.DocumentResult) error {
	p := d.props["path"]
	if p == "" {
		p = dir
	}
	if p == "" {
		p = "/"
	}
	rp, err := s.confine(p, true)
	if err != nil {
		return err
	}
	infos, err := s.fs.ReadDir(rp)
	if err != nil {
		return err
	}
	out := make([]entry, 0, len(infos))
	for _, fi := range infos {
		if isTempName(fi.Name()) {
			continue // in-flight upload
		}
		if len(out) >= s.cfg.MaxListEntries {
			res.Tracked["truncated"] = "true"
			break
		}
		out = append(out, entry{Name: fi.Name(), Path: path.Join("/", p, fi.Name()), Size: fi.Size(), IsDir: fi.IsDir(), ModTime: fi.ModTime().UTC(), Mode: fi.Mode().String()})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	outID := d.id + ".out"
	res.OutputDocumentIds = []string{outID}
	res.Tracked["path"] = p
	return sendOutput(stream, outID, map[string]string{"contentType": "application/json"}, bytes.NewReader(b))
}

func (s *session) get(stream connectorv1.ConnectorService_ExecuteServer, d *docState, dir string, res *connectorv1.DocumentResult) error {
	p, err := s.target(d, dir, true)
	if err != nil {
		return err
	}
	f, err := s.fs.Open(p)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if st, err := f.Stat(); err == nil {
		if st.IsDir() {
			return fmt.Errorf("%w: %s is a directory", errBadInput, d.props["path"])
		}
		res.Tracked["size"] = fmt.Sprint(st.Size())
	}
	res.Tracked["path"] = d.props["path"]
	outID := d.id + ".out"
	res.OutputDocumentIds = []string{outID}
	return sendOutput(stream, outID, map[string]string{"path": d.props["path"], "filename": path.Base(p)}, f)
}

// rename moves a file, replacing the destination. It prefers the atomic posix-rename extension
// and otherwise falls back to a backup-based replace: an existing destination is first moved to a
// backup name and only deleted after the new file is in place, and restored if the move fails, so
// a failure never leaves the data deleted.
func (s *session) rename(from, to string) error {
	if err := s.fs.PosixRename(from, to); err == nil {
		return nil
	}
	err := s.fs.Rename(from, to)
	if err == nil {
		return nil
	}
	if _, serr := s.fs.Lstat(to); serr != nil {
		return err // destination absent: the failure is not about replacing
	}
	var r [4]byte
	_, _ = rand.Read(r[:])
	bak := to + ".nurtuo-bak-" + hex.EncodeToString(r[:])
	if berr := s.fs.Rename(to, bak); berr != nil {
		return err
	}
	if rerr := s.fs.Rename(from, to); rerr != nil {
		if undo := s.fs.Rename(bak, to); undo != nil {
			return fmt.Errorf("%w (destination preserved as %s: %v)", rerr, bak, undo)
		}
		return rerr
	}
	_ = s.fs.Remove(bak)
	return nil
}

// sendOutput streams r as chunks with bounded memory.
func sendOutput(stream connectorv1.ConnectorService_ExecuteServer, id string, props map[string]string, r io.Reader) error {
	buf := make([]byte, ChunkSize)
	first := true
	for {
		n, err := io.ReadFull(r, buf)
		last := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
		if err != nil && !last {
			return err
		}
		ch := &connectorv1.DocumentChunk{DocumentId: id, Data: append([]byte(nil), buf[:n]...), Last: last}
		if first {
			ch.Properties = props
			first = false
		}
		if serr := stream.Send(&connectorv1.ExecuteResponse{Msg: &connectorv1.ExecuteResponse_Output{Output: ch}}); serr != nil {
			return serr
		}
		if last {
			return nil
		}
	}
}
