package drive

import (
	"context"
	"errors"
	"net"
)

// ErrOffline is returned by ReadFileContent (and propagated through Stat /
// ListChildren) when the Proton API is unreachable AND no cached data is
// available to serve the request.
//
// Callers in main.go map this to rpc.ErrOffline so the GVfs backend can
// surface G_IO_ERROR_NOT_CONNECTED to the file manager.
var ErrOffline = errors.New("offline: no cached data available")

// isOfflineError reports whether err represents a network-level failure.
//
// go-proton-api uses resty, which wraps transport errors in *url.Error.
// *url.Error implements net.Error, so errors.As covers DNS failures,
// connection-refused, and connection-reset.  context.DeadlineExceeded is
// included for dial timeouts that bypass the net.Error wrapping.
func isOfflineError(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}
