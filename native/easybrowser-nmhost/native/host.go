package native

import (
	"encoding/binary"
	"io"
)

// ReadMessage reads a single Native Messaging message from r.
// The Chrome Native Messaging protocol uses a 4-byte little-endian length
// prefix followed by the raw message payload.
func ReadMessage(r io.Reader) ([]byte, error) {
	var length uint32
	if err := binary.Read(r, binary.LittleEndian, &length); err != nil {
		return nil, err
	}
	if length == 0 {
		return []byte{}, nil
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// WriteMessage writes a single Native Messaging message to w.
// It writes the 4-byte little-endian length prefix followed by the payload.
func WriteMessage(w io.Writer, msg []byte) error {
	length := uint32(len(msg))
	if err := binary.Write(w, binary.LittleEndian, length); err != nil {
		return err
	}
	if length > 0 {
		if _, err := w.Write(msg); err != nil {
			return err
		}
	}
	return nil
}
