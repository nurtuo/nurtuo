// SPDX-License-Identifier: Apache-2.0

package sftp

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	pkgsftp "github.com/pkg/sftp"
)

func TestMalformedDirectoryPackets(t *testing.T) {
	const expectedID = 2
	for _, tc := range []struct {
		name       string
		length, id uint32
		words      []uint32
		want       string
	}{
		{"huge entry count", 9, expectedID, []uint32{0xffffffff}, "entry count"},
		{"truncated attributes", 21, expectedID, []uint32{1, 0, 0, 1}, "EOF"},
		{"wrong identifier", 9, 3, []uint32{0}, "response identifier"},
		{"oversized NAME", 256<<10 + 1, expectedID, nil, "packet length"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := append(binary.BigEndian.AppendUint32(nil, tc.length), fxpName)
			data = binary.BigEndian.AppendUint32(data, tc.id)
			for _, word := range tc.words {
				data = binary.BigEndian.AppendUint32(data, word)
			}
			_, packet, err := readDirectoryPacket(bytes.NewReader(data), expectedID)
			if err == nil {
				_, err = packet.visitEntries(func(string, *pkgsftp.FileStat) (bool, error) {
					t.Fatal("malformed entry passed to visitor")
					return false, nil
				})
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestDirectoryStatusKeepsApplicationClassification(t *testing.T) {
	for _, tc := range []struct {
		code uint32
		app  bool
	}{{2, true}, {3, true}, {4, true}, {5, false}, {8, false}} {
		data := binary.BigEndian.AppendUint32(nil, 1+4+4+4+4)
		data = append(data, fxpStatus)
		data = binary.BigEndian.AppendUint32(data, 2)
		data = binary.BigEndian.AppendUint32(data, tc.code)
		data = binary.BigEndian.AppendUint32(data, 0) // message
		data = binary.BigEndian.AppendUint32(data, 0) // language
		kind, packet, err := readDirectoryPacket(bytes.NewReader(data), 2)
		if err != nil {
			t.Fatalf("code %d: %v", tc.code, err)
		}
		err = packet.statusError(kind)
		if err == nil {
			t.Fatalf("code %d: want error", tc.code)
		}
		if got := isApplicationError(err); got != tc.app {
			t.Fatalf("code %d: isApplicationError=%v want %v (%v)", tc.code, got, tc.app, err)
		}
	}
}
