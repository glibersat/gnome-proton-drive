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
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

const (
	ErrInternal   = -32603
	ErrInvalidArg = -32602
	ErrNotFound   = -32001
	ErrAuthFailed = -32002
	ErrNotAuthed  = -32003
	ErrHVRequired = -32004 // human verification (CAPTCHA) needed
	ErrOffline    = -32005 // network unreachable and no cached data available
)

// HVDetails is embedded in RPCError.Details when Code == ErrHVRequired.
type HVDetails struct {
	Token   string   `json:"token"`
	Methods []string `json:"methods"`
}

// Wire types — one struct per method.

type AuthParams struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// AuthResult is returned by Auth. The caller should persist all three fields
// in the keyring and discard the password. SaltedPassphrase is the output of
// Proton's KDF (bcrypt over password + server salt); it unlocks the address
// keyring but cannot be used for SRP login, making it safer to store than
// the raw password.
type AuthResult struct {
	UID              string `json:"uid"`
	RefreshToken     string `json:"refresh_token"`
	SaltedPassphrase []byte `json:"salted_passphrase"`
}

// ResumeParams allows restoring a session from stored credentials without
// re-doing SRP. Username is optional but, when provided, scopes the on-disk
// block cache to the account's email address rather than the UID.
type ResumeParams struct {
	Username         string `json:"username,omitempty"`
	UID              string `json:"uid"`
	RefreshToken     string `json:"refresh_token"`
	SaltedPassphrase []byte `json:"salted_passphrase"`
}

// AuthWithHVParams retries Auth after the user completes a human verification
// challenge. Type is the verification method: "captcha", "email", or "sms".
// HVToken is the solved token (composite captcha string, or the plain code
// delivered by email/SMS).
type AuthWithHVParams struct {
	Username string `json:"username"`
	Password string `json:"password"`
	HVToken  string `json:"hv_token"`
	Type     string `json:"type"` // "captcha" | "email" | "sms"
}

// SendCodeParams asks Proton to deliver a verification code to the user's
// registered email address. Type must be "email".
type SendCodeParams struct {
	Username string `json:"username"`
	Type     string `json:"type"`    // "email"
	Address  string `json:"address"` // must equal the Proton account address
}

// GetCaptchaParams fetches the captcha HTML for a given HV token.
type GetCaptchaParams struct {
	HVToken string `json:"hv_token"`
}

type GetCaptchaResult struct {
	HTML string `json:"html"`
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

// GetEventsResult is returned by GetEvents.  Events is nil (not an empty
// array) when nothing is pending, so the C backend can distinguish "no events"
// from "events array present but empty".
type GetEventsResult struct {
	Events []Event `json:"events"`
}

// Event is a single remote-change notification delivered to the C backend.
// Path is the absolute POSIX path; it may be empty when the changed link was
// never visited in the current session.
type Event struct {
	Type   string `json:"type"`    // "changed" | "deleted" | "created"
	LinkID string `json:"link_id"`
	Path   string `json:"path,omitempty"`
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
