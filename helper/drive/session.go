package drive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	proton "github.com/ProtonMail/go-proton-api"
	"github.com/ProtonMail/gluon/async"
	"github.com/ProtonMail/gopenpgp/v2/crypto"
)

// Session holds a live authenticated Proton Drive client and the resolved
// root of the user's main share. Crypto and HTTP work is delegated to
// go-proton-api; this file only resolves POSIX paths to LinkIDs.
//
// Each Session owns a MetaCache (in-memory, TTL-based), a BlockCache
// (persistent on-disk), and an EventPoller that polls the share event stream
// and invalidates stale cache entries as changes arrive.
// See metacache.go, blockcache.go, and events.go for details.
type Session struct {
	client  *proton.Client
	shareID string
	rootID  string
	account string                     // email address — scopes the block cache directory
	addrKR  *crypto.KeyRing            // primary address keyring
	shareKR *crypto.KeyRing            // share keyring (unlocked with addrKR)
	nodeKRs map[string]*crypto.KeyRing // linkID → decrypted node keyring (cache)
	linkPaths map[string]string        // linkID → absolute path (reverse map for event resolution)
	meta    *MetaCache                 // short-lived metadata cache
	blocks  *BlockCache                // persistent block cache (nil if unavailable)
	poller  *EventPoller               // background share-event poller
}

// HVRequiredError is returned by NewSession when Proton requires a CAPTCHA.
// The caller should fetch the captcha HTML (via Manager.GetCaptcha), complete
// it in a browser, then retry with NewSessionWithHV.
type HVRequiredError struct {
	Token   string
	Methods []string
}

func (e *HVRequiredError) Error() string { return "human verification required" }

// SessionCredentials holds the values that should be stored in libsecret
// so that future mounts can resume without re-doing SRP.
type SessionCredentials struct {
	UID              string
	RefreshToken     string
	SaltedPassphrase []byte // KDF output — unlocks address keys but not SRP login
}

// NewSession authenticates via SRP. Returns HVRequiredError if Proton requires
// a CAPTCHA — the caller should complete it and retry via NewSessionWithHV.
func NewSession(ctx context.Context, mgr *proton.Manager, username, password string) (*Session, SessionCredentials, error) {
	c, auth, err := mgr.NewClientWithLogin(ctx, username, []byte(password))
	if err != nil {
		var apiErr *proton.APIError
		if errors.As(err, &apiErr) && apiErr.IsHVError() {
			hv, hvErr := apiErr.GetHVDetails()
			if hvErr == nil {
				return nil, SessionCredentials{}, &HVRequiredError{
					Token:   hv.Token,
					Methods: hv.Methods,
				}
			}
		}
		return nil, SessionCredentials{}, err
	}

	addrKR, saltedPass, err := unlockPrimaryAddress(ctx, c, []byte(password))
	if err != nil {
		_ = c.AuthDelete(ctx)
		return nil, SessionCredentials{}, err
	}

	shareID, rootID, err := findMainShare(ctx, c)
	if err != nil {
		_ = c.AuthDelete(ctx)
		return nil, SessionCredentials{}, err
	}

	creds := SessionCredentials{
		UID:              auth.UID,
		RefreshToken:     auth.RefreshToken,
		SaltedPassphrase: saltedPass,
	}
	return newSession(c, shareID, rootID, username, addrKR), creds, nil
}

// NewSessionWithHV retries SRP login after the user completes a human
// verification challenge. hvType is the method used ("captcha", "email",
// "sms") and hvToken is the solved token or delivered code.
func NewSessionWithHV(ctx context.Context, mgr *proton.Manager, username, password, hvToken, hvType string) (*Session, SessionCredentials, error) {
	hv := &proton.APIHVDetails{
		Methods: []string{hvType},
		Token:   hvToken,
	}
	c, auth, err := mgr.NewClientWithLoginWithHVToken(ctx, username, []byte(password), hv)
	if err != nil {
		return nil, SessionCredentials{}, err
	}

	addrKR, saltedPass, err := unlockPrimaryAddress(ctx, c, []byte(password))
	if err != nil {
		_ = c.AuthDelete(ctx)
		return nil, SessionCredentials{}, err
	}

	shareID, rootID, err := findMainShare(ctx, c)
	if err != nil {
		_ = c.AuthDelete(ctx)
		return nil, SessionCredentials{}, err
	}

	creds := SessionCredentials{
		UID:              auth.UID,
		RefreshToken:     auth.RefreshToken,
		SaltedPassphrase: saltedPass,
	}
	return newSession(c, shareID, rootID, username, addrKR), creds, nil
}

// ResumeSession restores a fully authenticated session from the three values
// stored in the keyring — no SRP re-auth needed.  account is the user's email
// address, used to scope the on-disk block cache; if empty the UID is used as
// a fallback so that callers that cannot supply the email still get a cache.
func ResumeSession(ctx context.Context, mgr *proton.Manager, creds SessionCredentials, account string) (*Session, SessionCredentials, error) {
	c, auth, err := mgr.NewClientWithRefresh(ctx, creds.UID, creds.RefreshToken)
	if err != nil {
		return nil, SessionCredentials{}, fmt.Errorf("token refresh failed: %w", err)
	}

	addrKR, err := unlockWithSaltedPassphrase(ctx, c, creds.SaltedPassphrase)
	if err != nil {
		_ = c.AuthDelete(ctx)
		return nil, SessionCredentials{}, err
	}

	shareID, rootID, err := findMainShare(ctx, c)
	if err != nil {
		_ = c.AuthDelete(ctx)
		return nil, SessionCredentials{}, err
	}

	if account == "" {
		account = creds.UID
	}

	newCreds := SessionCredentials{
		UID:              auth.UID,
		RefreshToken:     auth.RefreshToken,
		SaltedPassphrase: creds.SaltedPassphrase,
	}
	return newSession(c, shareID, rootID, account, addrKR), newCreds, nil
}

// newSession is the single place where Session is constructed.  It
// initialises the metadata cache and, if the cache directory is reachable, the
// persistent block cache.
func newSession(c *proton.Client, shareID, rootID, account string, addrKR *crypto.KeyRing) *Session {
	s := &Session{
		client:    c,
		shareID:   shareID,
		rootID:    rootID,
		account:   account,
		addrKR:    addrKR,
		nodeKRs:   make(map[string]*crypto.KeyRing),
		linkPaths: make(map[string]string),
		meta:      NewMetaCache(0),
	}
	base, err := blockCacheBase(account)
	if err != nil {
		log.Printf("block cache disabled: %v", err)
		return s
	}
	bc, err := NewBlockCache(base, 0)
	if err != nil {
		log.Printf("block cache init failed: %v", err)
		return s
	}
	s.blocks = bc
	return s
}

// StartPoller launches the background share-event poller.  It must be called
// after the session is fully initialised (i.e. after newSession returns) so
// that the session context is ready for API calls.  Separated from newSession
// so that callers can hold a reference to the session before polling begins.
func (s *Session) StartPoller(ctx context.Context) {
	s.poller = newEventPoller(s, 0)
	s.poller.Start(ctx)
}

// blockCacheBase returns the account-scoped base directory for the block cache.
// The account email is URL-path-escaped so "@" becomes "%40" — safe for all
// filesystems while remaining human-readable.
func blockCacheBase(account string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "proton-drive", url.PathEscape(account)), nil
}

func (s *Session) Close(ctx context.Context) error {
	if s.poller != nil {
		s.poller.Stop()
	}
	return s.client.AuthDelete(ctx)
}

// DrainEvents returns all events queued by the poller since the last call and
// resets the queue.  Returns nil when nothing is pending or the poller is not
// running.  This is the backing implementation of the GetEvents RPC method.
func (s *Session) DrainEvents() []DriveEvent {
	if s.poller == nil {
		return nil
	}
	return s.poller.Drain()
}

// linkPath looks up the absolute path for a linkID in the reverse map.
// Returns an empty string if the link has not been visited in this session.
func (s *Session) linkPath(linkID string) string {
	return s.linkPaths[linkID]
}

func (s *Session) AddrKR() *crypto.KeyRing { return s.addrKR }

// ListChildren returns active children of the directory at the given path,
// along with the parent's keyring (needed to decrypt child names).
//
// Results are served from the metadata cache when fresh.  On API failure the
// stale cache is returned so that offline reads continue to work for
// previously-visited directories.
func (s *Session) ListChildren(ctx context.Context, dirPath string) ([]proton.Link, *crypto.KeyRing, error) {
	if links, kr, ok := s.meta.GetList(dirPath); ok {
		log.Printf("cache hit  list %s (%d entries)", dirPath, len(links))
		return links, kr, nil
	}
	log.Printf("cache miss list %s", dirPath)

	linkID, kr, err := s.resolvePath(ctx, dirPath)
	if err != nil {
		if isOfflineError(err) {
			if links, staleKR, ok := s.meta.GetListStale(dirPath); ok {
				log.Printf("cache stale list %s (offline)", dirPath)
				return links, staleKR, nil
			}
		}
		return nil, nil, err
	}

	links, err := s.client.ListChildren(ctx, s.shareID, linkID, false)
	if err != nil {
		if isOfflineError(err) {
			if links, staleKR, ok := s.meta.GetListStale(dirPath); ok {
				log.Printf("cache stale list %s (offline)", dirPath)
				return links, staleKR, nil
			}
		}
		return nil, nil, err
	}

	s.meta.SetList(dirPath, links, kr)
	return links, kr, nil
}

// Stat returns the Link and its parent's keyring for a given path.
//
// Results are served from the metadata cache when fresh.  On API failure the
// stale cache is returned so that offline reads continue to work for
// previously-visited paths.
func (s *Session) Stat(ctx context.Context, p string) (proton.Link, *crypto.KeyRing, error) {
	p = path.Clean("/" + p)

	if link, kr, ok := s.meta.GetStat(p); ok {
		log.Printf("cache hit  stat %s", p)
		return link, kr, nil
	}
	log.Printf("cache miss stat %s", p)

	link, kr, err := s.statUncached(ctx, p)
	if err != nil {
		if isOfflineError(err) {
			if link, staleKR, ok := s.meta.GetStatStale(p); ok {
				log.Printf("cache stale stat %s (offline)", p)
				return link, staleKR, nil
			}
		}
		return proton.Link{}, nil, err
	}

	s.meta.SetStat(p, link, kr)
	return link, kr, nil
}

// statUncached performs the actual network Stat without consulting the cache.
func (s *Session) statUncached(ctx context.Context, p string) (proton.Link, *crypto.KeyRing, error) {
	if p == "/" {
		link, err := s.client.GetLink(ctx, s.shareID, s.rootID)
		if err != nil {
			return proton.Link{}, nil, err
		}
		kr, err := s.rootNodeKeyRing(ctx)
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

// ReadFileContent returns the fully decrypted content of a file link.
//
// The block cache is consulted first: on a hit the bytes are returned without
// touching the network.  On a miss the revision is fetched from the API, each
// encrypted block is decrypted with the session key, the result is stored in
// the block cache, and the plaintext is returned.
//
// On API failure ErrOffline is returned so that callers can map it to the
// appropriate RPC error code.
func (s *Session) ReadFileContent(ctx context.Context, link proton.Link, parentKR *crypto.KeyRing) ([]byte, error) {
	linkID := link.LinkID
	revID := link.FileProperties.ActiveRevision.ID

	if s.blocks != nil {
		if data, ok := s.blocks.Get(linkID, revID); ok {
			log.Printf("cache hit  block %s rev %s (%d bytes)", linkID, revID, len(data))
			return data, nil
		}
		log.Printf("cache miss block %s rev %s", linkID, revID)
	}

	nodeKR, err := link.GetKeyRing(parentKR, s.addrKR)
	if err != nil {
		return nil, err
	}
	sessionKey, err := link.GetSessionKey(nodeKR)
	if err != nil {
		return nil, err
	}

	rev, err := s.GetRevision(ctx, linkID, revID, 1, 100)
	if err != nil {
		if isOfflineError(err) {
			return nil, ErrOffline
		}
		return nil, err
	}

	var data []byte
	for _, block := range rev.Blocks {
		enc, err := s.GetBlock(ctx, block.BareURL, block.Token)
		if err != nil {
			if isOfflineError(err) {
				return nil, ErrOffline
			}
			return nil, err
		}
		plain, err := sessionKey.Decrypt(enc)
		if err != nil {
			return nil, err
		}
		data = append(data, plain.GetBinary()...)
	}

	if s.blocks != nil {
		if err := s.blocks.Put(linkID, revID, data); err != nil {
			log.Printf("cache store block %s rev %s: %v", linkID, revID, err)
		} else {
			log.Printf("cache store block %s rev %s (%d bytes)", linkID, revID, len(data))
		}
	}
	return data, nil
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
	currentPath := "/"

	// Seed the reverse map for the root.
	s.linkPaths[linkID] = "/"

	kr, err := s.rootNodeKeyRing(ctx)
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

			if currentPath == "/" {
				currentPath = "/" + part
			} else {
				currentPath = currentPath + "/" + part
			}
			s.linkPaths[linkID] = currentPath
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

// shareKeyRing returns the keyring for the Drive share itself (used to unlock
// the root node key). Cached after first fetch.
func (s *Session) shareKeyRing(ctx context.Context) (*crypto.KeyRing, error) {
	if s.shareKR != nil {
		return s.shareKR, nil
	}
	share, err := s.client.GetShare(ctx, s.shareID)
	if err != nil {
		return nil, err
	}
	kr, err := share.GetKeyRing(s.addrKR)
	if err != nil {
		return nil, err
	}
	s.shareKR = kr
	return kr, nil
}

// rootNodeKeyRing returns the node keyring for the root folder. Children's
// names and keys are encrypted with the parent's NODE keyring, so listing
// root's children requires the root link's node keyring, not the share keyring.
func (s *Session) rootNodeKeyRing(ctx context.Context) (*crypto.KeyRing, error) {
	if kr, ok := s.nodeKRs[s.rootID]; ok {
		return kr, nil
	}
	shareKR, err := s.shareKeyRing(ctx)
	if err != nil {
		return nil, err
	}
	rootLink, err := s.client.GetLink(ctx, s.shareID, s.rootID)
	if err != nil {
		return nil, err
	}
	kr, err := rootLink.GetKeyRing(shareKR, s.addrKR)
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

// unlockPrimaryAddress derives the salted passphrase from the raw password,
// unlocks the address keyring, and returns both so the passphrase can be
// stored in the keyring for future passwordless resumes.
func unlockPrimaryAddress(ctx context.Context, c *proton.Client, password []byte) (*crypto.KeyRing, []byte, error) {
	user, err := c.GetUser(ctx)
	if err != nil {
		return nil, nil, err
	}

	salts, err := c.GetSalts(ctx)
	if err != nil {
		return nil, nil, err
	}

	saltedPass, err := salts.SaltForKey(password, user.Keys[0].ID)
	if err != nil {
		return nil, nil, err
	}

	kr, err := unlockWithSaltedPassphrase(ctx, c, saltedPass)
	if err != nil {
		return nil, nil, err
	}
	return kr, saltedPass, nil
}

// unlockWithSaltedPassphrase restores the address keyring from the stored
// salted passphrase — no raw password needed.
func unlockWithSaltedPassphrase(ctx context.Context, c *proton.Client, saltedPass []byte) (*crypto.KeyRing, error) {
	user, err := c.GetUser(ctx)
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
