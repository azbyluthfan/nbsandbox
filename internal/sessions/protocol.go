// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package sessions

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// MaxClientMessage bounds a message from a client (a cell's code).
const MaxClientMessage = 1 << 20

// clientMessages are the message types clients may send, by channel. The
// bridge allows the same and nbsandboxd's own control messages; a client
// can't restart the kernel this way (the API has an endpoint for it) or
// send anything else (input, comms, history).
var clientMessages = map[string]map[string]bool{
	"shell": {"execute_request": true, "complete_request": true, "inspect_request": true,
		"is_complete_request": true, "kernel_info_request": true},
	"control": {"interrupt_request": true},
}

// RefusedError is a client message nbsandboxd won't pass on.
type RefusedError struct{ Msg string }

func (e *RefusedError) Error() string { return e.Msg }

// CheckClientMessage checks a message from a client and returns it on one
// line: a Jupyter message (channel, header with msg_id and msg_type,
// content) of an allowed type, at most MaxClientMessage bytes.
func CheckClientMessage(msg []byte) ([]byte, error) {
	if len(msg) > MaxClientMessage {
		return nil, &RefusedError{Msg: fmt.Sprintf("message too large: %d bytes, at most %d", len(msg), MaxClientMessage)}
	}
	var m struct {
		Channel string `json:"channel"`
		Header  struct {
			MsgID   string `json:"msg_id"`
			MsgType string `json:"msg_type"`
		} `json:"header"`
		Content json.RawMessage `json:"content"`
		Buffers json.RawMessage `json:"buffers"`
	}
	if err := json.Unmarshal(msg, &m); err != nil {
		return nil, &RefusedError{Msg: "not a JSON Jupyter message: " + err.Error()}
	}
	if m.Header.MsgID == "" || m.Header.MsgType == "" {
		return nil, &RefusedError{Msg: "a message needs header.msg_id and header.msg_type"}
	}
	if !clientMessages[m.Channel][m.Header.MsgType] {
		return nil, &RefusedError{Msg: fmt.Sprintf("%s on %q isn't allowed in a session", m.Header.MsgType, m.Channel)}
	}
	if len(m.Buffers) > 0 && string(m.Buffers) != "[]" && string(m.Buffers) != "null" {
		return nil, &RefusedError{Msg: "binary buffers aren't supported"}
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, msg); err != nil {
		return nil, &RefusedError{Msg: err.Error()}
	}
	return buf.Bytes(), nil
}
