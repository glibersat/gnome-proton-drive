package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net"
	"os"

	proton "github.com/ProtonMail/go-proton-api"

	"github.com/glibersat/gnome-proton/helper/drive"
	"github.com/glibersat/gnome-proton/helper/rpc"
)

// linkToEntry converts a proton.Link to an rpc.Entry. For files, mtime is the
// revision creation time — stable across metadata changes, changing only when
// content changes. For directories, link.ModifyTime is used as-is.
func linkToEntry(l proton.Link, name string) rpc.Entry {
	mtime := l.ModifyTime
	var revID string
	if l.FileProperties != nil {
		revID = l.FileProperties.ActiveRevision.ID
		mtime = l.FileProperties.ActiveRevision.CreateTime
	}
	return rpc.Entry{
		Name:       name,
		IsDir:      l.Type == proton.LinkTypeFolder,
		Size:       l.Size,
		MTime:      mtime,
		LinkID:     l.LinkID,
		RevisionID: revID,
	}
}

func main() {
	socketPath := flag.String("socket", "", "unix socket path")
	flag.Parse()

	if *socketPath == "" {
		log.Fatal("--socket is required")
	}

	_ = os.Remove(*socketPath)
	ln, err := net.Listen("unix", *socketPath)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	mgr := proton.New(proton.WithAppVersion("web-drive@5.0.0"))
	srv := rpc.NewServer()

	var session *drive.Session

	requireSession := func() (*drive.Session, error) {
		if session == nil {
			return nil, rpc.NotAuth()
		}
		return session, nil
	}

	// Auth performs a full SRP login. On success returns credentials to store
	// in libsecret. On CAPTCHA challenge returns ErrHVRequired with HVDetails
	// so the caller can open a browser and retry via AuthWithHV.
	srv.Register("Auth", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p rpc.AuthParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidArg, Message: err.Error()}
		}
		s, creds, err := drive.NewSession(ctx, mgr, p.Username, p.Password)
		if err != nil {
			var hvErr *drive.HVRequiredError
			if errors.As(err, &hvErr) {
				details, _ := json.Marshal(rpc.HVDetails{
					Token:   hvErr.Token,
					Methods: hvErr.Methods,
				})
				return nil, &rpc.RPCError{
					Code:    rpc.ErrHVRequired,
					Message: "human verification required",
					Details: details,
				}
			}
			return nil, &rpc.RPCError{Code: rpc.ErrAuthFailed, Message: err.Error()}
		}
		session = s
		session.StartPoller(ctx)
		return rpc.AuthResult{
			UID:              creds.UID,
			RefreshToken:     creds.RefreshToken,
			SaltedPassphrase: creds.SaltedPassphrase,
		}, nil
	})

	// GetCaptcha fetches the hCaptcha HTML page for the given HV token.
	// The setup script serves this locally, intercepts the postMessage result,
	// and passes the response token back via AuthWithHV.
	srv.Register("GetCaptcha", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p rpc.GetCaptchaParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidArg, Message: err.Error()}
		}
		html, err := mgr.GetCaptcha(ctx, p.HVToken)
		if err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrInternal, Message: err.Error()}
		}
		return rpc.GetCaptchaResult{HTML: string(html)}, nil
	})

	// AuthWithHV retries SRP login after the user completes a human verification
	// challenge.  p.Type selects the method: "captcha", "email", or "sms".
	srv.Register("AuthWithHV", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p rpc.AuthWithHVParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidArg, Message: err.Error()}
		}
		hvType := p.Type
		if hvType == "" {
			hvType = "captcha"
		}
		s, creds, err := drive.NewSessionWithHV(ctx, mgr, p.Username, p.Password, p.HVToken, hvType)
		if err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrAuthFailed, Message: err.Error()}
		}
		session = s
		session.StartPoller(ctx)
		return rpc.AuthResult{
			UID:              creds.UID,
			RefreshToken:     creds.RefreshToken,
			SaltedPassphrase: creds.SaltedPassphrase,
		}, nil
	})

	// SendCode asks Proton to deliver a verification code to the user's email
	// address or phone number. The user then enters the code and retries via
	// AuthWithHV with the matching type.
	srv.Register("SendCode", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p rpc.SendCodeParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidArg, Message: err.Error()}
		}
		dest := proton.TokenDestination{
			Address: p.Address,
		}
		err := mgr.SendVerificationCode(ctx, proton.SendVerificationCodeReq{
			Username:    p.Username,
			Type:        proton.TokenType(p.Type),
			Destination: dest,
		})
		if err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrInternal, Message: err.Error()}
		}
		return map[string]bool{"ok": true}, nil
	})

	// ResumeSession restores a session from stored credentials — no password.
	// This is what gvfsd-proton calls on every mount.
	srv.Register("ResumeSession", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p rpc.ResumeParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidArg, Message: err.Error()}
		}
		resumeAccount := p.Username
		if resumeAccount == "" {
			resumeAccount = p.UID
		}
		s, creds, err := drive.ResumeSession(ctx, mgr, drive.SessionCredentials{
			UID:              p.UID,
			RefreshToken:     p.RefreshToken,
			SaltedPassphrase: p.SaltedPassphrase,
		}, resumeAccount)
		if err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrAuthFailed, Message: err.Error()}
		}
		session = s
		session.StartPoller(ctx)
		return rpc.AuthResult{
			UID:              creds.UID,
			RefreshToken:     creds.RefreshToken,
			SaltedPassphrase: creds.SaltedPassphrase,
		}, nil
	})

	// GetEvents drains the event queue accumulated by the background poller
	// since the last call.  Returns an empty list when nothing is pending.
	// The C backend calls this every 5 s to drive g_file_monitor_emit_event().
	srv.Register("GetEvents", func(ctx context.Context, raw json.RawMessage) (any, error) {
		s, err := requireSession()
		if err != nil {
			return nil, err
		}
		drained := s.DrainEvents()
		events := make([]rpc.Event, len(drained))
		for i, e := range drained {
			events[i] = rpc.Event{
				Type:   string(e.Type),
				LinkID: e.LinkID,
				Path:   e.Path,
			}
		}
		return rpc.GetEventsResult{Events: events}, nil
	})

	srv.Register("ListDir", func(ctx context.Context, raw json.RawMessage) (any, error) {
		s, err := requireSession()
		if err != nil {
			return nil, err
		}
		var p rpc.ListDirParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidArg, Message: err.Error()}
		}

		links, parentKR, err := s.ListChildren(ctx, p.Path)
		if err != nil {
			return nil, err
		}

		log.Printf("ListDir %q: %d raw links from API", p.Path, len(links))
		result := rpc.ListDirResult{Entries: make([]rpc.Entry, 0, len(links))}
		for _, l := range links {
			if l.State != proton.LinkStateActive {
				log.Printf("  skip %s: state=%d", l.LinkID, l.State)
				continue
			}
			name, err := l.GetName(parentKR, s.AddrKR())
			if err != nil {
				log.Printf("  skip %s: GetName error: %v", l.LinkID, err)
				continue
			}
			log.Printf("  entry %q type=%d dir=%v size=%d", name, l.Type, l.Type == proton.LinkTypeFolder, l.Size)
			result.Entries = append(result.Entries, linkToEntry(l, name))
		}
		return result, nil
	})

	srv.Register("Stat", func(ctx context.Context, raw json.RawMessage) (any, error) {
		s, err := requireSession()
		if err != nil {
			return nil, err
		}
		var p rpc.StatParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidArg, Message: err.Error()}
		}

		link, parentKR, err := s.Stat(ctx, p.Path)
		if err != nil {
			return nil, rpc.NotFound(err.Error())
		}

		name, _ := link.GetName(parentKR, s.AddrKR())
		return linkToEntry(link, name), nil
	})

	srv.Register("ReadFile", func(ctx context.Context, raw json.RawMessage) (any, error) {
		s, err := requireSession()
		if err != nil {
			return nil, err
		}
		var p rpc.ReadParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidArg, Message: err.Error()}
		}

		link, parentKR, err := s.Stat(ctx, p.Path)
		if err != nil {
			return nil, rpc.NotFound(err.Error())
		}
		if link.Type != proton.LinkTypeFile {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidArg, Message: "not a file"}
		}

		data, err := s.ReadFileContent(ctx, link, parentKR, p.Offset, p.Length)
		if err != nil {
			if errors.Is(err, drive.ErrOffline) {
				return nil, rpc.Offline("network unreachable and file not in cache")
			}
			return nil, err
		}

		eof := p.Length == 0 || int64(len(data)) < p.Length
		return rpc.ReadResult{Data: data, EOF: eof}, nil
	})

	srv.Register("Mkdir", func(ctx context.Context, raw json.RawMessage) (any, error) {
		s, err := requireSession()
		if err != nil {
			return nil, err
		}
		var p rpc.MkdirParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidArg, Message: err.Error()}
		}
		return nil, s.MakeDir(ctx, p.Path)
	})

	srv.Register("Delete", func(ctx context.Context, raw json.RawMessage) (any, error) {
		s, err := requireSession()
		if err != nil {
			return nil, err
		}
		var p rpc.DeleteParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidArg, Message: err.Error()}
		}
		return nil, s.Delete(ctx, p.Path, p.Trash)
	})

	srv.Register("Move", func(ctx context.Context, raw json.RawMessage) (any, error) {
		s, err := requireSession()
		if err != nil {
			return nil, err
		}
		var p rpc.MoveParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidArg, Message: err.Error()}
		}
		return nil, s.Move(ctx, p.Src, p.Dst)
	})

	log.Printf("proton-drive-helper listening on %s", *socketPath)
	srv.Serve(ln)
}
