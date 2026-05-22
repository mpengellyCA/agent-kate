// Package ipc implements the newline-delimited JSON-RPC 2.0 bus that connects
// the akcore orchestration core to the agentkate UI.
package ipc

import "encoding/json"

// Frame is a single JSON-RPC 2.0 message. Whether it is a request, response or
// notification is determined by which fields are populated:
//
//	request:      Method + ID set
//	notification: Method set, ID nil
//	response:     ID set, Result or Error set
type Frame struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`
}

// RPCError is the JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

// Standard JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Errorf builds an *RPCError. Returning one from a Handler sets the JSON-RPC
// error verbatim; any other error becomes a CodeInternalError.
func Errorf(code int, msg string) *RPCError {
	return &RPCError{Code: code, Message: msg}
}
