package rpc

import "encoding/json"

type Request struct {
	ID     uint64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	ID     uint64    `json:"id"`
	Result any       `json:"result,omitempty"`
	Error  *RPCError `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return e.Message }

const (
	ErrInternal   = -32603
	ErrInvalidArg = -32602
	ErrNotFound   = -32001
	ErrAuthFailed = -32002
	ErrNotAuthed  = -32003
)

// Wire types — one struct per method.

type AuthParams struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type ListDirParams struct{ Path string `json:"path"` }
type ListDirResult struct {
	Entries []Entry `json:"entries"`
}

type StatParams struct{ Path string `json:"path"` }

type Entry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
}

type ReadParams struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
}
type ReadResult struct {
	Data []byte `json:"data"`
	EOF  bool   `json:"eof"`
}

type WriteParams struct {
	Path     string `json:"path"`
	Data     []byte `json:"data"`
	Offset   int64  `json:"offset"`
	Truncate bool   `json:"truncate"`
}

type MkdirParams struct{ Path string `json:"path"` }

type DeleteParams struct {
	Path  string `json:"path"`
	Trash bool   `json:"trash"`
}

type MoveParams struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
}
