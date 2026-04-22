package drive

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

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
// authState holds the rolling access credentials used for raw HTTP calls
// (e.g. thumbnail endpoint) that bypass go-proton-api's request machinery.
type authState struct {
	mu      sync.RWMutex
	uid     string
	bearer  string // access token, updated on every 401-refresh
}

type Session struct {
	client    *proton.Client
	shareID   string
	volumeID  string   // volume that owns the main share (for volume-level events)
	rootID    string
	account   string   // email address — scopes the block cache directory
	cacheBase string   // overrides blockCacheBase(account) when set (used in tests)
	apiBase   string   // Proton API base URL, e.g. https://mail.proton.me/api
	auth      authState // rolling access token captured from the proton client
	addrKR    *crypto.KeyRing            // primary address keyring
	addrEmail string                     // email of the primary address (used as SignatureAddress)
	shareKR  *crypto.KeyRing            // share keyring (unlocked with addrKR)
	nodeKRs   map[string]*crypto.KeyRing // linkID → decrypted node keyring (cache)
	linkPaths map[string]string          // linkID → absolute path (reverse map for event resolution)
	pathLinks map[string]string          // absolute path → linkID (forward map for resolvePath short-circuit)
	meta     *MetaCache                 // short-lived metadata cache
	blocks   *BlockCache                // persistent block cache (nil if unavailable)
	poller   *EventPoller               // background share-event poller
}

// currentAuth returns a snapshot of the latest access credentials.
func (s *Session) currentAuth() (uid, bearer string) {
	s.auth.mu.RLock()
	defer s.auth.mu.RUnlock()
	return s.auth.uid, s.auth.bearer
}

// setAuth stores updated access credentials. Called at session creation and
// whenever go-proton-api refreshes the access token after a 401.
func (s *Session) setAuth(uid, accessToken string) {
	s.auth.mu.Lock()
	defer s.auth.mu.Unlock()
	if uid != "" {
		s.auth.uid = uid
	}
	if accessToken != "" {
		s.auth.bearer = accessToken
	}
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

	addrKR, addrEmail, saltedPass, err := unlockPrimaryAddress(ctx, c, []byte(password))
	if err != nil {
		_ = c.AuthDelete(ctx)
		return nil, SessionCredentials{}, err
	}

	shareID, volumeID, rootID, err := findMainShare(ctx, c)
	if err != nil {
		_ = c.AuthDelete(ctx)
		return nil, SessionCredentials{}, err
	}

	creds := SessionCredentials{
		UID:              auth.UID,
		RefreshToken:     auth.RefreshToken,
		SaltedPassphrase: saltedPass,
	}
	s := newSession(c, shareID, volumeID, rootID, username, addrEmail, addrKR)
	s.setAuth(auth.UID, auth.AccessToken)
	return s, creds, nil
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

	addrKR, addrEmail, saltedPass, err := unlockPrimaryAddress(ctx, c, []byte(password))
	if err != nil {
		_ = c.AuthDelete(ctx)
		return nil, SessionCredentials{}, err
	}

	shareID, volumeID, rootID, err := findMainShare(ctx, c)
	if err != nil {
		_ = c.AuthDelete(ctx)
		return nil, SessionCredentials{}, err
	}

	creds := SessionCredentials{
		UID:              auth.UID,
		RefreshToken:     auth.RefreshToken,
		SaltedPassphrase: saltedPass,
	}
	s := newSession(c, shareID, volumeID, rootID, username, addrEmail, addrKR)
	s.setAuth(auth.UID, auth.AccessToken)
	return s, creds, nil
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

	addrKR, addrEmail, err := unlockWithSaltedPassphrase(ctx, c, creds.SaltedPassphrase)
	if err != nil {
		_ = c.AuthDelete(ctx)
		return nil, SessionCredentials{}, err
	}

	shareID, volumeID, rootID, err := findMainShare(ctx, c)
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
	s := newSession(c, shareID, volumeID, rootID, account, addrEmail, addrKR)
	s.setAuth(auth.UID, auth.AccessToken)
	return s, newCreds, nil
}

// newSession is the single place where Session is constructed.  It
// initialises the metadata cache and, if the cache directory is reachable, the
// persistent block cache.
func newSession(c *proton.Client, shareID, volumeID, rootID, account, addrEmail string, addrKR *crypto.KeyRing) *Session {
	s := &Session{
		client:    c,
		shareID:   shareID,
		volumeID:  volumeID,
		rootID:    rootID,
		account:   account,
		addrEmail: addrEmail,
		apiBase:   proton.DefaultHostURL,
		addrKR:    addrKR,
		nodeKRs:   make(map[string]*crypto.KeyRing),
		linkPaths: make(map[string]string),
		pathLinks: make(map[string]string),
		meta:      NewMetaCache(0),
	}

	// Keep rolling auth credentials in sync with the client.
	// AddAuthHandler fires whenever go-proton-api refreshes the access token
	// after a 401, ensuring thumbnail API calls always use a fresh token.
	c.AddAuthHandler(func(a proton.Auth) {
		s.setAuth(a.UID, a.AccessToken)
	})

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

// cacheBaseFor returns the account-scoped base directory for all caches.
// If the session has a cacheBase override (set in tests), it is used directly.
// Otherwise the directory is derived from the OS user cache dir.
func (s *Session) cacheBaseFor() (string, error) {
	if s.cacheBase != "" {
		return s.cacheBase, nil
	}
	return blockCacheBase(s.account)
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
	name := path.Base(p)
	children, _, err := s.ListChildren(ctx, parentPath)
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

// protonBlockSize is the nominal plaintext size of each encrypted block.
// Blocks are 4 MiB except the last block of a file, which may be smaller.
// The Proton Drive API does not expose per-block plaintext sizes in download
// responses, so we rely on this constant when computing block indices from
// byte offsets.
const protonBlockSize = 4 * 1024 * 1024

// ReadFileContent returns decrypted file bytes in [offset, offset+length).
//
// offset and length follow the same semantics as io.ReaderAt: offset is the
// byte position within the file, length 0 means "to EOF".  The caller is
// responsible for ensuring offset ≤ link.Size.
//
// Block fetching starts at the block that contains offset, so only the blocks
// overlapping the requested range are downloaded and decrypted.  Each fetched
// block is stored individually in the block cache; subsequent reads of
// overlapping or adjacent ranges benefit from cached blocks.
//
// Offline fallback: if the API is unreachable the method attempts to serve the
// requested range entirely from cached blocks, deriving the block count from
// link.Size.  If any required block is absent from the cache, ErrOffline is
// returned.
func (s *Session) ReadFileContent(ctx context.Context, link proton.Link, parentKR *crypto.KeyRing, offset, length int64) ([]byte, error) {
	linkID := link.LinkID
	revID := link.FileProperties.ActiveRevision.ID

	nodeKR, err := link.GetKeyRing(parentKR, s.addrKR)
	if err != nil {
		return nil, err
	}
	sessionKey, err := link.GetSessionKey(nodeKR)
	if err != nil {
		return nil, err
	}

	// startBlock0 is the 0-based index of the first block that contains offset.
	startBlock0 := int(offset / protonBlockSize)
	// The API uses 1-based FromBlockIndex.
	apiFromBlock := startBlock0 + 1

	rev, err := s.GetRevision(ctx, linkID, revID, apiFromBlock, 500)
	if err != nil {
		if !isOfflineError(err) {
			return nil, err
		}
		// Offline: try to serve the range from per-block cache alone.
		return s.readFromCache(linkID, revID, link.Size, offset, length)
	}

	var buf []byte
	for _, block := range rev.Blocks {
		// block.Index is 1-based; convert to 0-based for the cache key.
		idx := block.Index - 1
		if s.blocks != nil {
			if cached, ok := s.blocks.GetBlock(linkID, revID, idx); ok {
				log.Printf("cache hit  block %s rev %s idx %d (%d bytes)", linkID, revID, idx, len(cached))
				buf = append(buf, cached...)
				continue
			}
		}
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
		plainBytes := plain.GetBinary()
		if s.blocks != nil {
			if err := s.blocks.PutBlock(linkID, revID, idx, plainBytes); err != nil {
				log.Printf("cache store block %s rev %s idx %d: %v", linkID, revID, idx, err)
			} else {
				log.Printf("cache store block %s rev %s idx %d (%d bytes)", linkID, revID, idx, len(plainBytes))
			}
		}
		buf = append(buf, plainBytes...)
	}

	return sliceRange(buf, startBlock0, offset, length), nil
}

// readFromCache serves [offset, offset+length) from per-block cache entries,
// deriving the expected block count from fileSize.  Returns ErrOffline if any
// required block is absent.
func (s *Session) readFromCache(linkID, revID string, fileSize, offset, length int64) ([]byte, error) {
	if s.blocks == nil {
		return nil, ErrOffline
	}
	startBlock0 := int(offset / protonBlockSize)
	endByte := fileSize
	if length > 0 && offset+length < endByte {
		endByte = offset + length
	}
	endBlock0 := int((endByte - 1) / protonBlockSize)

	var buf []byte
	for idx := startBlock0; idx <= endBlock0; idx++ {
		data, ok := s.blocks.GetBlock(linkID, revID, idx)
		if !ok {
			log.Printf("offline cache miss block %s rev %s idx %d", linkID, revID, idx)
			return nil, ErrOffline
		}
		buf = append(buf, data...)
	}
	return sliceRange(buf, startBlock0, offset, length), nil
}

// sliceRange trims buf (which starts at the beginning of startBlock0) to the
// byte range [offset, offset+length).  length 0 means keep everything.
func sliceRange(buf []byte, startBlock0 int, offset, length int64) []byte {
	blockStartByte := int64(startBlock0) * protonBlockSize
	trimStart := offset - blockStartByte
	if trimStart < 0 {
		trimStart = 0
	}
	if trimStart >= int64(len(buf)) {
		return nil
	}
	buf = buf[trimStart:]
	if length > 0 && length < int64(len(buf)) {
		buf = buf[:length]
	}
	return buf
}

// ErrAlreadyExists is returned when a Mkdir target name already exists.
var ErrAlreadyExists = errors.New("already exists")

// isAlreadyExistsError returns true when the Proton API reports code 2500.
func isAlreadyExistsError(err error) bool {
	var apiErr proton.APIError
	return errors.As(err, &apiErr) && apiErr.Code == 2500
}

// MakeDir creates a folder at the given path.
func (s *Session) MakeDir(ctx context.Context, p string) error {
	parent := path.Dir(p)
	name := path.Base(p)

	// Resolve parent to get its linkID and keyring.
	parentLink, _, err := s.Stat(ctx, parent)
	if err != nil {
		return err
	}
	parentKR, err := s.nodeKeyRing(ctx, parentLink)
	if err != nil {
		return fmt.Errorf("MakeDir: parent keyring: %w", err)
	}

	// Get parent hash key for NameHash computation.
	hashKey, err := parentLink.GetHashKey(parentKR)
	if err != nil {
		return fmt.Errorf("MakeDir: hash key: %w", err)
	}

	// Generate a fresh x25519 NodeKey for the new folder.
	nodeKey, err := crypto.GenerateKey(s.addrEmail, s.addrEmail, "x25519", 0)
	if err != nil {
		return fmt.Errorf("MakeDir: generate node key: %w", err)
	}

	// Generate a random 32-byte NodePassphrase, base64-encoded.
	// The passphrase is stored and transmitted as a base64 string so that
	// all clients (web, mobile) can decode it as UTF-8 text.
	// The NodeKey is locked with the UTF-8 bytes of this base64 string.
	rawPass := make([]byte, 32)
	if _, err := rand.Read(rawPass); err != nil {
		return fmt.Errorf("MakeDir: generate passphrase: %w", err)
	}
	passB64 := base64.StdEncoding.EncodeToString(rawPass)

	// Lock the NodeKey with the UTF-8 bytes of the base64 passphrase.
	lockedKey, err := nodeKey.Lock([]byte(passB64))
	if err != nil {
		return fmt.Errorf("MakeDir: lock node key: %w", err)
	}
	armoredNodeKey, err := lockedKey.Armor()
	if err != nil {
		return fmt.Errorf("MakeDir: armor node key: %w", err)
	}

	// Encrypt the passphrase with the parent's NodeKey.
	encPass, err := parentKR.Encrypt(
		crypto.NewPlainMessageFromString(passB64),
		nil,
	)
	if err != nil {
		return fmt.Errorf("MakeDir: encrypt passphrase: %w", err)
	}
	armoredPass, err := encPass.GetArmored()
	if err != nil {
		return fmt.Errorf("MakeDir: armor passphrase: %w", err)
	}

	// Sign the passphrase with the address key.
	passSig, err := s.addrKR.SignDetached(crypto.NewPlainMessageFromString(passB64))
	if err != nil {
		return fmt.Errorf("MakeDir: sign passphrase: %w", err)
	}
	armoredPassSig, err := passSig.GetArmored()
	if err != nil {
		return fmt.Errorf("MakeDir: armor passphrase sig: %w", err)
	}

	// Build the new folder's NodeKeyRing so we can encrypt the NodeHashKey.
	nodeKR, err := crypto.NewKeyRing(nodeKey)
	if err != nil {
		return fmt.Errorf("MakeDir: new node keyring: %w", err)
	}

	// Generate a random 32-byte NodeHashKey and encrypt it with the new NodeKey.
	rawHashKey := make([]byte, 32)
	if _, err := rand.Read(rawHashKey); err != nil {
		return fmt.Errorf("MakeDir: generate hash key: %w", err)
	}
	encHashKey, err := nodeKR.Encrypt(crypto.NewPlainMessage(rawHashKey), s.addrKR)
	if err != nil {
		return fmt.Errorf("MakeDir: encrypt hash key: %w", err)
	}
	armoredHashKey, err := encHashKey.GetArmored()
	if err != nil {
		return fmt.Errorf("MakeDir: armor hash key: %w", err)
	}

	// Encrypt the folder name with the parent's NodeKey, signed by address key.
	encName, err := parentKR.Encrypt(
		crypto.NewPlainMessageFromString(name),
		s.addrKR,
	)
	if err != nil {
		return fmt.Errorf("MakeDir: encrypt name: %w", err)
	}
	armoredName, err := encName.GetArmored()
	if err != nil {
		return fmt.Errorf("MakeDir: armor name: %w", err)
	}

	// Compute NameHash = hex(HMAC-SHA256(hashKey, name_utf8)).
	mac := hmac.New(sha256.New, hashKey)
	mac.Write([]byte(name))
	nameHash := hex.EncodeToString(mac.Sum(nil))

	req := proton.CreateFolderReq{
		ParentLinkID:            parentLink.LinkID,
		Name:                    armoredName,
		Hash:                    nameHash,
		NodeKey:                 armoredNodeKey,
		NodeHashKey:             armoredHashKey,
		NodePassphrase:          armoredPass,
		NodePassphraseSignature: armoredPassSig,
		SignatureAddress:        s.addrEmail,
	}

	_, err = s.client.CreateFolder(ctx, s.shareID, req)
	if isAlreadyExistsError(err) {
		s.meta.InvalidatePath(parent)
		return ErrAlreadyExists
	}
	if err != nil {
		s.meta.InvalidatePath(parent)
		return fmt.Errorf("MakeDir: API: %w", err)
	}

	// Invalidate parent so subsequent ListDir reflects the new entry.
	s.meta.InvalidatePath(parent)
	return nil
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
	p = path.Clean("/" + p)
	parts := splitPath(p)
	linkID := s.rootID
	currentPath := "/"

	// Fast path: full path already resolved.
	if id, ok := s.pathLinks[p]; ok {
		if kr, ok := s.nodeKRs[id]; ok {
			log.Printf("path cache hit  %s", p)
			return id, kr, nil
		}
	}
	log.Printf("path cache miss %s", p)

	// Seed the reverse map for the root.
	s.linkPaths[linkID] = "/"

	kr, err := s.rootNodeKeyRing(ctx)
	if err != nil {
		return "", nil, err
	}
	s.pathLinks["/"] = s.rootID

	for _, part := range parts {
		children, err := s.client.ListChildren(ctx, s.shareID, linkID, false)
		if err != nil {
			return "", nil, err
		}
		s.meta.SetList(currentPath, children, kr)

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
			s.pathLinks[currentPath] = linkID
			break
		}

		if !found {
			return "", nil, fmt.Errorf("not found: %s", p)
		}
	}

	return linkID, kr, nil
}

// InvalidatePath clears all cached state for p: MetaCache entries, the
// path→linkID forward map entry, and all descendant forward map entries.
// This is the single call site used by the event poller (events.go) so that
// every cache layer stays consistent.
func (s *Session) InvalidatePath(p string) {
	s.meta.InvalidatePath(p)
	delete(s.pathLinks, p)
	prefix := p + "/"
	for cached := range s.pathLinks {
		if strings.HasPrefix(cached, prefix) {
			delete(s.pathLinks, cached)
		}
	}
}

// invalidateLinkID resolves the path for linkID via linkPaths, then delegates
// to InvalidatePath.
func (s *Session) invalidateLinkID(linkID string) {
	s.meta.invalidateLinkID(linkID)
	if p, ok := s.linkPaths[linkID]; ok {
		delete(s.pathLinks, p)
	}
}

// invalidateAll clears all MetaCache entries and the entire path→linkID map.
func (s *Session) invalidateAll() {
	s.meta.invalidateAll()
	s.pathLinks = make(map[string]string)
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

// nodeKeyRing returns the cached keyring for link. Stat/resolvePath always
// populates nodeKRs before returning, so a cache miss here is unexpected.
func (s *Session) nodeKeyRing(_ context.Context, link proton.Link) (*crypto.KeyRing, error) {
	if kr, ok := s.nodeKRs[link.LinkID]; ok {
		return kr, nil
	}
	return nil, fmt.Errorf("node keyring not cached for link %s", link.LinkID)
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
func unlockPrimaryAddress(ctx context.Context, c *proton.Client, password []byte) (*crypto.KeyRing, string, []byte, error) {
	user, err := c.GetUser(ctx)
	if err != nil {
		return nil, "", nil, err
	}

	salts, err := c.GetSalts(ctx)
	if err != nil {
		return nil, "", nil, err
	}

	saltedPass, err := salts.SaltForKey(password, user.Keys[0].ID)
	if err != nil {
		return nil, "", nil, err
	}

	kr, email, err := unlockWithSaltedPassphrase(ctx, c, saltedPass)
	if err != nil {
		return nil, "", nil, err
	}
	return kr, email, saltedPass, nil
}

// unlockWithSaltedPassphrase restores the address keyring from the stored
// salted passphrase — no raw password needed.
// Also returns the email of the primary unlocked address for use as SignatureAddress.
func unlockWithSaltedPassphrase(ctx context.Context, c *proton.Client, saltedPass []byte) (*crypto.KeyRing, string, error) {
	user, err := c.GetUser(ctx)
	if err != nil {
		return nil, "", err
	}

	addrs, err := c.GetAddresses(ctx)
	if err != nil {
		return nil, "", err
	}

	_, addrKRs, err := proton.Unlock(user, addrs, saltedPass, async.NoopPanicHandler{})
	if err != nil {
		return nil, "", err
	}

	for _, addr := range addrs {
		if kr, ok := addrKRs[addr.ID]; ok {
			return kr, addr.Email, nil
		}
	}

	return nil, "", fmt.Errorf("no address keyring found")
}

func findMainShare(ctx context.Context, c *proton.Client) (shareID, volumeID, rootID string, err error) {
	shares, err := c.ListShares(ctx, false)
	if err != nil {
		return "", "", "", err
	}

	for _, s := range shares {
		if s.Type == proton.ShareTypeMain {
			return s.ShareID, s.VolumeID, s.LinkID, nil
		}
	}

	return "", "", "", fmt.Errorf("no main Drive share found")
}
