// SPDX-License-Identifier: Apache-2.0

package sftp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"

	pkgsftp "github.com/pkg/sftp"
)

type outputBudget interface {
	OutputBytesRemaining() (int64, bool)
	RejectOutputLimit() error
}

var errDirectoryBudget = errors.New("sftp: directory response exceeds the wire budget")

// SFTP v3 packet types used by the streaming directory reader.
const (
	fxpInit     = 1
	fxpVersion  = 2
	fxpLstat    = 7
	fxpOpenDir  = 11
	fxpReadDir  = 12
	fxpReadLink = 19
	fxpStatus   = 101
	fxpHandle   = 102
	fxpName     = 104
	fxpAttrs    = 105
)

// pkg/sftp's ReadDirContext accumulates the entire listing. Use a dedicated
// streaming subsystem for both path confinement and listing. Closing it also
// discards any unread bytes in the last NAME packet.
func (s *session) readBoundedDir(dir string, remaining int64, visit func(string, *pkgsftp.FileStat) (bool, error)) (err error) {
	channel, err := s.ssh.NewSession()
	if err != nil {
		return err
	}
	defer func() { _ = channel.Close() }()
	input, err := channel.StdinPipe()
	if err != nil {
		return err
	}
	output, err := channel.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := channel.StderrPipe()
	if err != nil {
		return err
	}
	if err := channel.RequestSubsystem("sftp"); err != nil {
		return err
	}
	go func() { _, _ = io.Copy(io.Discard, stderr) }()
	const overhead = 64 << 10
	wire := &io.LimitedReader{R: output, N: 2*min(remaining, (math.MaxInt64-overhead)/2) + overhead}
	defer func() {
		if wire.N == 0 && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
			err = errDirectoryBudget
		}
	}()

	request := func(kind byte, id uint32, value []byte) error {
		payload := binary.BigEndian.AppendUint32([]byte{kind}, id)
		if kind != fxpInit {
			payload = binary.BigEndian.AppendUint32(payload, uint32(len(value)))
			payload = append(payload, value...)
		}
		_, err := input.Write(append(binary.BigEndian.AppendUint32(nil, uint32(len(payload))), payload...))
		return err
	}

	if err := request(fxpInit, 3, nil); err != nil {
		return err
	}
	kind, packet, err := readDirectoryPacket(wire, 3)
	if err != nil {
		return err
	}
	if kind != fxpVersion {
		return errors.New("sftp: expected directory subsystem version")
	}
	if err := packet.skip(packet.N); err != nil {
		return err
	}
	id := uint32(0)
	call := func(kind byte, value string) (byte, *directoryPacket, error) {
		id++
		if err := request(kind, id, []byte(value)); err != nil {
			return 0, nil, err
		}
		return readDirectoryPacket(wire, id)
	}
	symlink := func(path string) (bool, error) {
		kind, packet, err := call(fxpLstat, path)
		if err != nil {
			return false, err
		}
		if kind != fxpAttrs {
			return false, packet.statusError(kind)
		}
		stat := packet.attributes()
		if packet.err == nil && packet.N != 0 {
			return false, errors.New("sftp: trailing data in directory attributes")
		}
		return stat.FileMode()&os.ModeSymlink != 0, packet.err
	}
	readLink := func(path string) (string, error) {
		kind, packet, err := call(fxpReadLink, path)
		if err != nil {
			return "", err
		}
		if kind != fxpName {
			return "", packet.statusError(kind)
		}
		if packet.uint32() != 1 {
			return "", errors.New("sftp: invalid symlink response count")
		}
		target := string(packet.readString())
		_ = packet.skip(int64(packet.uint32()))
		_ = packet.attributes()
		if packet.err == nil && packet.N != 0 {
			return "", errors.New("sftp: trailing data in symlink response")
		}
		return target, packet.err
	}
	confined, err := s.confineWith(dir, true, symlink, readLink)
	if err != nil {
		return err
	}
	kind, packet, err = call(fxpOpenDir, confined)
	if err != nil {
		return err
	}
	if kind != fxpHandle {
		return packet.statusError(kind)
	}
	handle := packet.readString()
	if packet.err != nil {
		return packet.err
	}
	if packet.N != 0 {
		return errors.New("sftp: trailing data in directory handle")
	}

	for {
		kind, packet, err := call(fxpReadDir, string(handle))
		if err != nil {
			return err
		}
		if kind == fxpStatus {
			err := packet.statusError(kind)
			if errors.Is(err, pkgsftp.ErrSSHFxEOF) {
				return nil
			}
			return err
		}
		if kind != fxpName {
			return fmt.Errorf("sftp: unexpected directory response type %d", kind)
		}
		more, err := packet.visitEntries(visit)
		if err != nil || !more {
			return err
		}
	}
}

func readDirectoryPacket(wire io.Reader, id uint32) (byte, *directoryPacket, error) {
	var header [5]byte
	if _, err := io.ReadFull(wire, header[:]); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(header[:4])
	if length < 5 || length > 256<<10 {
		return 0, nil, errors.New("sftp: invalid directory packet length")
	}
	packet := &directoryPacket{LimitedReader: io.LimitedReader{R: wire, N: int64(length) - 1}}
	if got := packet.uint32(); packet.err == nil && got != id {
		return 0, nil, errors.New("sftp: unexpected directory response identifier")
	}
	return header[4], packet, packet.err
}
func (packet *directoryPacket) visitEntries(visit func(string, *pkgsftp.FileStat) (bool, error)) (bool, error) {
	count := packet.uint32()
	if packet.err == nil && int64(count) > packet.N/12 {
		return false, errors.New("sftp: invalid directory entry count")
	}
	for range count {
		name := string(packet.readString())
		_ = packet.skip(int64(packet.uint32())) // longname
		stat := packet.attributes()
		if packet.err != nil {
			return false, packet.err
		}
		if name == "." || name == ".." {
			continue
		}
		more, err := visit(path.Base(name), &stat)
		if err != nil || !more {
			return false, err
		}
	}
	if packet.err != nil {
		return false, packet.err
	}
	if packet.N != 0 {
		return false, errors.New("sftp: trailing data in directory listing")
	}
	return true, nil
}

type directoryPacket struct {
	io.LimitedReader
	err error
}

func (p *directoryPacket) uint32() uint32 {
	var value [4]byte
	if p.err == nil {
		_, p.err = io.ReadFull(p, value[:])
	}
	return binary.BigEndian.Uint32(value[:])
}

func (p *directoryPacket) skip(n int64) error {
	if p.err == nil {
		_, p.err = io.CopyN(io.Discard, p, n)
	}
	return p.err
}

func (p *directoryPacket) readString() []byte {
	size := int64(p.uint32())
	if p.err != nil {
		return nil
	}
	if size > p.N {
		p.err = errors.New("sftp: invalid directory string length")
		return nil
	}
	value := make([]byte, size)
	_, p.err = io.ReadFull(p, value)
	return value
}

func (p *directoryPacket) statusError(kind byte) error {
	if kind != fxpStatus {
		return fmt.Errorf("sftp: unexpected directory response type %d", kind)
	}
	code := p.uint32()
	message := p.readString()
	_ = p.skip(int64(p.uint32())) // language tag
	if p.err != nil {
		return p.err
	}
	switch code {
	case 1:
		return pkgsftp.ErrSSHFxEOF
	case 2:
		return os.ErrNotExist
	case 3:
		return os.ErrPermission
	default:
		// Typed so isApplicationError classifies code 4 like the library path.
		return fmt.Errorf("sftp: directory status %d: %s: %w", code, message, &pkgsftp.StatusError{Code: code})
	}
}

func (p *directoryPacket) attributes() pkgsftp.FileStat {
	flags := p.uint32()
	var stat pkgsftp.FileStat
	if flags&1 != 0 { // size
		stat.Size = uint64(p.uint32())<<32 | uint64(p.uint32())
	}
	if flags&2 != 0 { // UID, GID
		_ = p.skip(8)
	}
	if flags&4 != 0 { // permissions
		stat.Mode = p.uint32()
	}
	if flags&8 != 0 { // atime, mtime
		_ = p.uint32()
		stat.Mtime = p.uint32()
	}
	if flags&0x80000000 != 0 { // extended attributes
		count := p.uint32()
		for i := uint32(0); i < count && p.err == nil; i++ {
			_ = p.skip(int64(p.uint32()))
			_ = p.skip(int64(p.uint32()))
		}
	}
	return stat
}
