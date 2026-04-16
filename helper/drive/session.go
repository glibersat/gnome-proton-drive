package drive

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	proton "github.com/ProtonMail/go-proton-api"
	"github.com/ProtonMail/gluon/async"
	"github.com/ProtonMail/gopenpgp/v2/crypto"
)

// Session holds a live authenticated Proton Drive client and the resolved
// root of the user's main share. Crypto and HTTP work is delegated to
// go-proton-api; this file only resolves POSIX paths to LinkIDs.
type Session struct {
	client  *proton.Client
	shareID string
	rootID  string
	addrKR  *crypto.KeyRing           // primary address keyring
	nodeKRs map[string]*crypto.KeyRing // linkID → decrypted node keyring (cache)
}

func NewSession(ctx context.Context, mgr *proton.Manager, username, password string) (*Session, error) {
	c, _, err := mgr.NewClientWithLogin(ctx, username, []byte(password))
	if err != nil {
		return nil, err
	}

	addrKR, err := unlockPrimaryAddress(ctx, c, []byte(password))
	if err != nil {
		_ = c.AuthDelete(ctx)
		return nil, err
	}

	shareID, rootID, err := findMainShare(ctx, c)
	if err != nil {
		_ = c.AuthDelete(ctx)
		return nil, err
	}

	return &Session{
		client:  c,
		shareID: shareID,
		rootID:  rootID,
		addrKR:  addrKR,
		nodeKRs: make(map[string]*crypto.KeyRing),
	}, nil
}

func (s *Session) Close(ctx context.Context) error {
	return s.client.AuthDelete(ctx)
}

func (s *Session) AddrKR() *crypto.KeyRing { return s.addrKR }

// ListChildren returns active children of the directory at the given path,
// along with the parent's keyring (needed to decrypt child names).
func (s *Session) ListChildren(ctx context.Context, dirPath string) ([]proton.Link, *crypto.KeyRing, error) {
	linkID, kr, err := s.resolvePath(ctx, dirPath)
	if err != nil {
		return nil, nil, err
	}

	links, err := s.client.ListChildren(ctx, s.shareID, linkID, false)
	if err != nil {
		return nil, nil, err
	}

	return links, kr, nil
}

// Stat returns the Link and its parent's keyring for a given path.
func (s *Session) Stat(ctx context.Context, p string) (proton.Link, *crypto.KeyRing, error) {
	p = path.Clean("/" + p)
	if p == "/" {
		link, err := s.client.GetLink(ctx, s.shareID, s.rootID)
		kr, _ := s.shareKeyRing(ctx)
		return link, kr, err
	}

	parentKR, err := s.parentKRFor(ctx, p)
	if err != nil {
		return proton.Link{}, nil, err
	}

	parentPath := path.Dir(p)
	parentID, _, err := s.resolvePath(ctx, parentPath)
	if err != nil {
		return proton.Link{}, nil, err
	}

	name := path.Base(p)
	children, err := s.client.ListChildren(ctx, s.shareID, parentID, false)
	if err != nil {
		return proton.Link{}, nil, err
	}

	for _, l := range children {
		n, err := l.GetName(parentKR, s.addrKR)
		if err != nil {
			continue
		}
		if n == name {
			return l, parentKR, nil
		}
	}

	return proton.Link{}, nil, fmt.Errorf("not found: %s", p)
}

// GetRevision exposes the underlying API call for file reads.
func (s *Session) GetRevision(ctx context.Context, linkID, revisionID string, fromBlock, pageSize int) (proton.Revision, error) {
	return s.client.GetRevision(ctx, s.shareID, linkID, revisionID, fromBlock, pageSize)
}

// GetBlock fetches a raw encrypted block and returns its bytes.
func (s *Session) GetBlock(ctx context.Context, url, token string) ([]byte, error) {
	rc, err := s.client.GetBlock(ctx, url, token)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	return io.ReadAll(rc)
}

// MakeDir creates a folder at the given path.
// TODO: key generation for NodeKey/NodePassphrase/NodeHashKey requires
// crypto helpers not yet exposed by go-proton-api. Implement once available.
func (s *Session) MakeDir(_ context.Context, _ string) error {
	return fmt.Errorf("MakeDir: not yet implemented")
}

// Move renames or moves a link.
// TODO: go-proton-api does not yet expose a MoveLink endpoint.
func (s *Session) Move(_ context.Context, _, _ string) error {
	return fmt.Errorf("Move: not yet implemented")
}

// Delete permanently removes or trashes a link.
func (s *Session) Delete(ctx context.Context, p string, trash bool) error {
	link, _, err := s.Stat(ctx, p)
	if err != nil {
		return err
	}

	parentID, _, err := s.resolvePath(ctx, path.Dir(path.Clean("/"+p)))
	if err != nil {
		return err
	}

	if trash {
		return s.client.TrashChildren(ctx, s.shareID, parentID, link.LinkID)
	}
	return s.client.DeleteChildren(ctx, s.shareID, parentID, link.LinkID)
}

// --- internal helpers ---

func (s *Session) resolvePath(ctx context.Context, p string) (string, *crypto.KeyRing, error) {
	parts := splitPath(p)
	linkID := s.rootID

	kr, err := s.shareKeyRing(ctx)
	if err != nil {
		return "", nil, err
	}

	for _, part := range parts {
		children, err := s.client.ListChildren(ctx, s.shareID, linkID, false)
		if err != nil {
			return "", nil, err
		}

		found := false
		for _, child := range children {
			name, err := child.GetName(kr, s.addrKR)
			if err != nil {
				continue
			}
			if name != part {
				continue
			}

			childKR, err := child.GetKeyRing(kr, s.addrKR)
			if err != nil {
				return "", nil, err
			}
			s.nodeKRs[child.LinkID] = childKR
			linkID = child.LinkID
			kr = childKR
			found = true
			break
		}

		if !found {
			return "", nil, fmt.Errorf("not found: %s", p)
		}
	}

	return linkID, kr, nil
}

func (s *Session) parentKRFor(ctx context.Context, p string) (*crypto.KeyRing, error) {
	_, kr, err := s.resolvePath(ctx, path.Dir(path.Clean("/"+p)))
	return kr, err
}

func (s *Session) shareKeyRing(ctx context.Context) (*crypto.KeyRing, error) {
	if kr, ok := s.nodeKRs[s.rootID]; ok {
		return kr, nil
	}

	share, err := s.client.GetShare(ctx, s.shareID)
	if err != nil {
		return nil, err
	}

	kr, err := share.GetKeyRing(s.addrKR)
	if err != nil {
		return nil, err
	}

	s.nodeKRs[s.rootID] = kr
	return kr, nil
}

func splitPath(p string) []string {
	p = path.Clean("/" + p)
	if p == "/" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(p, "/"), "/")
}

func unlockPrimaryAddress(ctx context.Context, c *proton.Client, password []byte) (*crypto.KeyRing, error) {
	user, err := c.GetUser(ctx)
	if err != nil {
		return nil, err
	}

	salts, err := c.GetSalts(ctx)
	if err != nil {
		return nil, err
	}

	saltedPass, err := salts.SaltForKey(password, user.Keys[0].ID)
	if err != nil {
		return nil, err
	}

	addrs, err := c.GetAddresses(ctx)
	if err != nil {
		return nil, err
	}

	_, addrKRs, err := proton.Unlock(user, addrs, saltedPass, async.NoopPanicHandler{})
	if err != nil {
		return nil, err
	}

	// Return the keyring for the primary (first) address.
	for _, addr := range addrs {
		if kr, ok := addrKRs[addr.ID]; ok {
			return kr, nil
		}
	}

	return nil, fmt.Errorf("no address keyring found")
}

func findMainShare(ctx context.Context, c *proton.Client) (string, string, error) {
	shares, err := c.ListShares(ctx, false)
	if err != nil {
		return "", "", err
	}

	for _, s := range shares {
		if s.Type == proton.ShareTypeMain {
			return s.ShareID, s.LinkID, nil
		}
	}

	return "", "", fmt.Errorf("no main Drive share found")
}
