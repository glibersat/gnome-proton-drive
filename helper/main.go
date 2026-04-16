package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net"
	"os"

	proton "github.com/ProtonMail/go-proton-api"

	"github.com/glibersat/gnome-proton/helper/drive"
	"github.com/glibersat/gnome-proton/helper/rpc"
)

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

	mgr := proton.New(proton.WithAppVersion("Other@1.0.0"))
	srv := rpc.NewServer()

	var session *drive.Session

	requireSession := func() (*drive.Session, error) {
		if session == nil {
			return nil, rpc.NotAuth()
		}
		return session, nil
	}

	// Auth performs a full SRP login. Returns credentials that the caller
	// should store in libsecret; the password must not be stored.
	srv.Register("Auth", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p rpc.AuthParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidArg, Message: err.Error()}
		}
		s, creds, err := drive.NewSession(ctx, mgr, p.Username, p.Password)
		if err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrAuthFailed, Message: err.Error()}
		}
		session = s
		return rpc.AuthResult{
			UID:              creds.UID,
			RefreshToken:     creds.RefreshToken,
			SaltedPassphrase: creds.SaltedPassphrase,
		}, nil
	})

	// ResumeSession restores a session from stored credentials — no password.
	// This is what gvfsd-proton calls on every mount.
	srv.Register("ResumeSession", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p rpc.ResumeParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidArg, Message: err.Error()}
		}
		s, err := drive.ResumeSession(ctx, mgr, drive.SessionCredentials{
			UID:              p.UID,
			RefreshToken:     p.RefreshToken,
			SaltedPassphrase: p.SaltedPassphrase,
		})
		if err != nil {
			return nil, &rpc.RPCError{Code: rpc.ErrAuthFailed, Message: err.Error()}
		}
		session = s
		return map[string]bool{"ok": true}, nil
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

		result := rpc.ListDirResult{Entries: make([]rpc.Entry, 0, len(links))}
		for _, l := range links {
			if l.State != proton.LinkStateActive {
				continue
			}
			name, err := l.GetName(parentKR, s.AddrKR())
			if err != nil {
				continue
			}
			result.Entries = append(result.Entries, rpc.Entry{
				Name:  name,
				IsDir: l.Type == proton.LinkTypeFolder,
				Size:  l.Size,
				MTime: l.ModifyTime,
			})
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
		return rpc.Entry{
			Name:  name,
			IsDir: link.Type == proton.LinkTypeFolder,
			Size:  link.Size,
			MTime: link.ModifyTime,
		}, nil
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

		nodeKR, err := link.GetKeyRing(parentKR, s.AddrKR())
		if err != nil {
			return nil, err
		}
		sessionKey, err := link.GetSessionKey(nodeKR)
		if err != nil {
			return nil, err
		}

		rev, err := s.GetRevision(ctx, link.LinkID, link.FileProperties.ActiveRevision.ID, 1, 100)
		if err != nil {
			return nil, err
		}

		var data []byte
		for _, block := range rev.Blocks {
			enc, err := s.GetBlock(ctx, block.BareURL, block.Token)
			if err != nil {
				return nil, err
			}
			plain, err := sessionKey.Decrypt(enc)
			if err != nil {
				return nil, err
			}
			data = append(data, plain.GetBinary()...)
		}

		if p.Offset > int64(len(data)) {
			p.Offset = int64(len(data))
		}
		data = data[p.Offset:]
		eof := true
		if p.Length > 0 && p.Length < int64(len(data)) {
			data = data[:p.Length]
			eof = false
		}

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
