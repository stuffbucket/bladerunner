package control_test

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/control"
)

// fullMessage returns a Message with every exported field set to a distinct
// non-zero value. A round trip that drops a field, or that puts the value of
// one field into a different one, changes the result. A message that only sets
// two fields cannot show that.
func fullMessage() *control.Message {
	return &control.Message{
		Version:  7,
		Command:  "roundtrip.command",
		Response: "roundtrip.response",
		Error:    "roundtrip.error",
	}
}

// TestMessageJSONFormatRoundTrip writes a fully populated Message with the wire
// format that the package gives, reads it back with the same format, and
// compares the whole struct. JSONFormat is the complete serialization of the
// type: each field holds a json tag, so a tag that is absent or wrong shows
// here as a field that does not survive the trip.
func TestMessageJSONFormatRoundTrip(t *testing.T) {
	format := control.JSONFormat{}
	original := fullMessage()

	var buf bytes.Buffer
	if err := format.Encode(&buf, original); err != nil {
		t.Fatalf("Encode() error = %v", err)
	}

	decoded, err := format.Decode(&buf)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	// Compare the full struct. A field-by-field check passes when a new field
	// arrives with no tag; this does not.
	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round trip = %+v, want %+v", *decoded, *original)
	}
}

// TestMessageJSONFormatRoundTripThroughFile repeats the trip through a file on
// disk instead of a buffer. The control protocol crosses a process boundary, so
// the writer and the reader are never the same Message value in memory. This
// holds the trip in the form that a second process sees.
func TestMessageJSONFormatRoundTripThroughFile(t *testing.T) {
	format := control.JSONFormat{}
	original := fullMessage()

	path := filepath.Join(t.TempDir(), "message.json")
	out, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := format.Encode(out, original); err != nil {
		_ = out.Close()
		t.Fatalf("Encode() error = %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	in, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = in.Close() }()

	decoded, err := format.Decode(in)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round trip = %+v, want %+v", *decoded, *original)
	}
}

// TestMessageLineFormatRoundTrip holds the trip through the default wire
// format. LineFormat puts one payload field on each line, so it cannot carry
// all four fields together; Encode selects Error, then Response, then Command.
// Each case below therefore sends the shape that the protocol sends on the
// socket, and compares the whole decoded struct against the exact result that
// the format gives back. The Command case shows the one place where the trip is
// not an identity: Decode cannot know if a line is a command or a response, so
// it fills both fields. The client and the listener read only the field that
// their side of the connection uses.
func TestMessageLineFormatRoundTrip(t *testing.T) {
	format := control.LineFormat{}

	cases := []struct {
		name string
		msg  *control.Message
		want *control.Message
	}{
		{
			// The shape that Client.sendCommand writes.
			name: "command",
			msg:  &control.Message{Version: 7, Command: "roundtrip.command"},
			want: &control.Message{
				Version:  7,
				Command:  "roundtrip.command",
				Response: "roundtrip.command",
			},
		},
		{
			// The shape that a handler returns for success.
			name: "response",
			msg:  &control.Message{Version: 7, Response: "roundtrip.response"},
			want: &control.Message{
				Version:  7,
				Command:  "roundtrip.response",
				Response: "roundtrip.response",
			},
		},
		{
			// The shape that a handler returns for failure. This one is an
			// identity: the "error: " prefix keeps the field apart from the
			// other two.
			name: "error",
			msg:  &control.Message{Version: 7, Error: "roundtrip.error"},
			want: &control.Message{Version: 7, Error: "roundtrip.error"},
		},
		{
			// A message with no version keeps the legacy form on the wire and
			// comes back with version 0.
			name: "legacy no version",
			msg:  &control.Message{Response: "roundtrip.response"},
			want: &control.Message{
				Command:  "roundtrip.response",
				Response: "roundtrip.response",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := format.Encode(&buf, tc.msg); err != nil {
				t.Fatalf("Encode() error = %v", err)
			}

			decoded, err := format.Decode(&buf)
			if err != nil {
				t.Fatalf("Decode() error = %v", err)
			}

			if !reflect.DeepEqual(decoded, tc.want) {
				t.Errorf("round trip = %+v, want %+v", *decoded, *tc.want)
			}
		})
	}
}

// TestMessageDefaultWireFormatRoundTrip holds the trip through the format that
// the client and the listener use when the caller gives no format. If the
// default changes, this test shows which format the protocol now speaks.
func TestMessageDefaultWireFormatRoundTrip(t *testing.T) {
	original := &control.Message{Version: 7, Error: "roundtrip.error"}

	var buf bytes.Buffer
	if err := control.DefaultWireFormat.Encode(&buf, original); err != nil {
		t.Fatalf("Encode() error = %v", err)
	}

	decoded, err := control.DefaultWireFormat.Decode(&buf)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	if !reflect.DeepEqual(decoded, original) {
		t.Errorf("round trip = %+v, want %+v", *decoded, *original)
	}
}
