// Package media stores the images and metadata ZVM NFTs point at, and serves
// them as immutable objects.
//
// Every object is named by the SHA-256 of its bytes, so its URL can never mean
// anything else: a cache may keep it forever, and anyone who has the bytes can
// re-host them and be checked. That is what makes a CDN in front of this store
// a cache rather than an authority — the operator's domain is where the bytes
// are usually found, not what makes them the right bytes.
//
// Uploading needs a signature from the wallet that will own what is minted,
// once per session: a Sign-In with Ethereum (EIP-4361) message, signed with
// `personal_sign` by an EVM wallet. Zenon accounts signed with Ed25519 through
// Syrius until D31 retired that branch; the native lane stays in the protocol,
// but uploads are a page feature and the page is MetaMask-shaped now.
package media

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

var ErrNotFound = errors.New("no such object")

const (
	// SessionStatement is what the sign-in message says it is for.
	SessionStatement = "Open an upload session for NFT images and metadata on this ZVM node. This signs no transaction and costs nothing."
	// SessionMaxAge bounds how long a signed session may claim to last.
	SessionMaxAge = 48 * time.Hour
)

var (
	hashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// An object reference: a hash, optionally with an extension, optionally
	// followed by a token id for a metadata template.
	refRE = regexp.MustCompile(`^/?([0-9a-f]{64})(?:\.[a-z0-9]{2,5})?(?:/([0-9]{1,78})(?:\.json)?)?$`)

	extFor = map[string]string{
		"image/png":        "png",
		"image/jpeg":       "jpg",
		"image/gif":        "gif",
		"image/webp":       "webp",
		"image/avif":       "avif",
		"image/svg+xml":    "svg",
		"application/json": "json",
	}
)

type Object struct {
	Hash        string `json:"hash"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
	Uploader    string `json:"uploader,omitempty"`
	CreatedAt   int64  `json:"createdAt"`
	Hidden      bool   `json:"hidden,omitempty"`
	URL         string `json:"url"`
}

type Config struct {
	Dir string
	// PublicURL is the canonical base written into metadata and token URIs —
	// the CDN hostname when there is one.
	PublicURL string
	// CDN says whether a browser should load objects from PublicURL. Until the
	// hostname is actually in front of this node, the page loads them from it.
	CDN           bool
	ChainID       uint64
	MaxImageBytes int64
	MaxJSONBytes  int64
	// DailyObjects and DailyBytes bound one account's uploads in a UTC day.
	DailyObjects int
	DailyBytes   int64
	// NodeDailyObjects and NodeDailyBytes bound everyone's together. An
	// account is a key and one signature, both free, so a per-account limit
	// alone bounds nothing: a script with fresh keys had the whole disk, and a
	// full disk stops derivation along with the uploads.
	NodeDailyObjects int
	NodeDailyBytes   int64
	Log              *slog.Logger
}

type Store struct {
	cfg        Config
	db         *sql.DB
	secret     []byte
	publicHost string
	publicPath string

	mu    sync.Mutex
	day   string
	node  dayUsage
	usage map[string]*dayUsage
}

type dayUsage struct {
	day     string
	objects int
	bytes   int64
}

const schema = `
CREATE TABLE IF NOT EXISTS media_objects (
	hash         TEXT PRIMARY KEY,
	content_type TEXT    NOT NULL,
	size         INTEGER NOT NULL,
	uploader     TEXT    NOT NULL,
	created_at   INTEGER NOT NULL,
	hidden       INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS media_secret (id INTEGER PRIMARY KEY CHECK (id = 1), secret BLOB NOT NULL);
`

func Open(cfg Config, db *sql.DB) (*Store, error) {
	if cfg.Dir == "" {
		return nil, errors.New("media: no directory")
	}
	if cfg.MaxImageBytes == 0 {
		cfg.MaxImageBytes = 10 << 20
	}
	if cfg.MaxJSONBytes == 0 {
		cfg.MaxJSONBytes = 256 << 10
	}
	if cfg.DailyObjects == 0 {
		cfg.DailyObjects = 500
	}
	if cfg.DailyBytes == 0 {
		cfg.DailyBytes = 500 << 20
	}
	if cfg.NodeDailyObjects == 0 {
		cfg.NodeDailyObjects = 5000
	}
	if cfg.NodeDailyBytes == 0 {
		cfg.NodeDailyBytes = 2 << 30
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("media tables: %w", err)
	}
	s := &Store{cfg: cfg, db: db, usage: map[string]*dayUsage{}}
	if cfg.PublicURL != "" {
		u, err := url.Parse(strings.TrimRight(cfg.PublicURL, "/"))
		if err != nil {
			return nil, fmt.Errorf("media: public URL %q: %w", cfg.PublicURL, err)
		}
		s.publicHost, s.publicPath = u.Hostname(), strings.TrimRight(u.Path, "/")
	}
	// The session secret outlives a restart, so a signature keeps its session.
	if err := db.QueryRow("SELECT secret FROM media_secret WHERE id = 1").Scan(&s.secret); errors.Is(err, sql.ErrNoRows) {
		s.secret = make([]byte, 32)
		if _, err := rand.Read(s.secret); err != nil {
			return nil, err
		}
		if _, err := db.Exec("INSERT INTO media_secret (id, secret) VALUES (1, ?)", s.secret); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return s, nil
}

// PublicBase is where an object's canonical URL starts — what goes on chain.
func (s *Store) PublicBase() string {
	if s.cfg.PublicURL != "" {
		return strings.TrimRight(s.cfg.PublicURL, "/")
	}
	return "/media"
}

// LoadBase is where a browser should read objects from now: the CDN when it is
// live, this node otherwise. Both spell the same object, so a page can show
// what a token URI points at even before the hostname exists.
func (s *Store) LoadBase() string {
	if s.cfg.CDN && s.cfg.PublicURL != "" {
		return strings.TrimRight(s.cfg.PublicURL, "/")
	}
	return "/media"
}

func (s *Store) Info() map[string]any {
	return map[string]any{
		"publicUrl":     s.PublicBase(),
		"loadUrl":       s.LoadBase(),
		"cdn":           s.cfg.CDN,
		"host":          s.publicHost,
		"maxImageBytes": s.cfg.MaxImageBytes,
		"maxJsonBytes":  s.cfg.MaxJSONBytes,
		"uploads":       true,
	}
}

func (s *Store) path(hash string) string {
	return filepath.Join(s.cfg.Dir, hash[:2], hash)
}

// URL is an object's canonical address, extension included so a CDN and a
// browser both know what it is without asking.
func (s *Store) URL(hash, contentType string) string {
	if ext, ok := extFor[contentType]; ok {
		return s.PublicBase() + "/" + hash + "." + ext
	}
	return s.PublicBase() + "/" + hash
}

// check is what Put refuses, without storing anything: the upload handler asks
// it before charging an account, so a refused upload spends no quota.
func (s *Store) check(data []byte, declared string) (string, error) {
	ct, err := Sniff(data, declared)
	if err != nil {
		return "", err
	}
	limit := s.cfg.MaxImageBytes
	if ct == "application/json" {
		limit = s.cfg.MaxJSONBytes
	}
	if int64(len(data)) > limit {
		return "", fmt.Errorf("%s is limited to %d bytes here", ct, limit)
	}
	return ct, nil
}

// Put stores bytes under their own hash. Storing the same bytes twice is the
// same object, and the second time is free.
func (s *Store) Put(data []byte, declared, uploader string) (Object, error) {
	ct, err := s.check(data, declared)
	if err != nil {
		return Object{}, err
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	path := s.path(hash)
	if _, err := os.Stat(path); err != nil {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return Object{}, err
		}
		tmp, err := os.CreateTemp(filepath.Dir(path), hash+".*.tmp")
		if err != nil {
			return Object{}, err
		}
		_, werr := tmp.Write(data)
		cerr := tmp.Close()
		if werr != nil || cerr != nil {
			os.Remove(tmp.Name())
			return Object{}, errors.Join(werr, cerr)
		}
		if err := os.Rename(tmp.Name(), path); err != nil {
			os.Remove(tmp.Name())
			return Object{}, err
		}
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO media_objects (hash, content_type, size, uploader, created_at) VALUES (?,?,?,?,?)`,
		hash, ct, len(data), uploader, time.Now().Unix()); err != nil {
		return Object{}, err
	}
	return s.Stat(hash)
}

func (s *Store) Stat(hash string) (Object, error) {
	var o Object
	var hidden int
	err := s.db.QueryRow(`SELECT hash, content_type, size, uploader, created_at, hidden FROM media_objects WHERE hash = ?`, hash).
		Scan(&o.Hash, &o.ContentType, &o.Size, &o.Uploader, &o.CreatedAt, &hidden)
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrNotFound
	}
	if err != nil {
		return o, err
	}
	o.Hidden = hidden != 0
	o.URL = s.URL(o.Hash, o.ContentType)
	return o, nil
}

// SetHidden refuses to serve an object without forgetting it: what a node
// operator does about bytes they must not serve. A CDN in front keeps its own
// copy until it is purged.
func (s *Store) SetHidden(hash string, hidden bool) error { return SetHidden(s.db, hash, hidden) }

// SetHidden, Describe and List work on the node's database alone, so the
// operator's commands need no store and no directory.
func SetHidden(db *sql.DB, hash string, hidden bool) error {
	v := 0
	if hidden {
		v = 1
	}
	res, err := db.Exec("UPDATE media_objects SET hidden = ? WHERE hash = ?", v, hash)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func Describe(db *sql.DB, hash string) (Object, error) {
	var o Object
	var hidden int
	err := db.QueryRow(`SELECT hash, content_type, size, uploader, created_at, hidden FROM media_objects WHERE hash = ?`, hash).
		Scan(&o.Hash, &o.ContentType, &o.Size, &o.Uploader, &o.CreatedAt, &hidden)
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrNotFound
	}
	o.Hidden = hidden != 0
	return o, err
}

func List(db *sql.DB, limit int) ([]Object, error) {
	rows, err := db.Query(`SELECT hash, content_type, size, uploader, created_at, hidden FROM media_objects
		ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Object
	for rows.Next() {
		var o Object
		var hidden int
		if err := rows.Scan(&o.Hash, &o.ContentType, &o.Size, &o.Uploader, &o.CreatedAt, &hidden); err != nil {
			return nil, err
		}
		o.Hidden = hidden != 0
		out = append(out, o)
	}
	return out, rows.Err()
}

// Resolve reads the bytes a URI points at, if this store holds them: its own
// public URL, any node's /media path, or a metadata template rendered for one
// token id. It never fetches anything.
func (s *Store) Resolve(uri string) ([]byte, bool) {
	hash, id, ok := s.parseRef(uri)
	if !ok {
		return nil, false
	}
	o, err := s.Stat(hash)
	if err != nil || o.Hidden {
		return nil, false
	}
	data, err := os.ReadFile(s.path(hash))
	if err != nil {
		return nil, false
	}
	if id != "" {
		if o.ContentType != "application/json" {
			return nil, false
		}
		return render(data, id), true
	}
	return data, true
}

// render substitutes a token id into a metadata template, the way ERC-1155
// metadata URIs substitute {id}. The template is content-addressed, so what it
// renders for an id is fixed forever too.
func render(data []byte, id string) []byte {
	return []byte(strings.ReplaceAll(string(data), "{id}", id))
}

func (s *Store) parseRef(uri string) (hash, id string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(uri))
	if err != nil {
		return "", "", false
	}
	path := u.Path
	switch {
	case u.Scheme == "" && u.Host == "":
		if !strings.HasPrefix(path, "/media/") {
			return "", "", false
		}
		path = strings.TrimPrefix(path, "/media")
	case u.Scheme != "http" && u.Scheme != "https":
		return "", "", false
	case s.publicHost != "" && strings.EqualFold(u.Hostname(), s.publicHost):
		path = strings.TrimPrefix(path, s.publicPath)
	case strings.HasPrefix(path, "/media/"):
		// Another node's spelling of the same object. The hash is the name, so
		// trusting the path costs nothing.
		path = strings.TrimPrefix(path, "/media")
	default:
		return "", "", false
	}
	m := refRE.FindStringSubmatch(path)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// Sniff decides what an upload is from its bytes, not from what it claims.
func Sniff(data []byte, declared string) (string, error) {
	declared = strings.ToLower(strings.TrimSpace(strings.Split(declared, ";")[0]))
	if len(data) == 0 {
		return "", errors.New("nothing to store")
	}
	if declared == "application/json" || (declared == "" && looksLikeJSON(data)) {
		if !json.Valid(data) {
			return "", errors.New("not valid JSON")
		}
		return "application/json", nil
	}
	if len(data) >= 12 && string(data[4:12]) == "ftypavif" {
		return "image/avif", nil
	}
	switch ct := http.DetectContentType(data); ct {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return ct, nil
	}
	if declared == "image/svg+xml" {
		head := strings.ToLower(string(data[:min(len(data), 1024)]))
		if strings.Contains(head, "<svg") {
			return "image/svg+xml", nil
		}
	}
	return "", fmt.Errorf("unsupported upload (%s): images are PNG, JPEG, GIF, WebP, AVIF or SVG, metadata is JSON",
		http.DetectContentType(data))
}

func looksLikeJSON(data []byte) bool {
	for _, b := range data {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case '{', '[':
			return true
		default:
			return false
		}
	}
	return false
}

// --- sessions ---------------------------------------------------------------

type SessionRequest struct {
	Address   string `json:"address"`
	Message   string `json:"message"`
	Signature string `json:"signature"`
}

// Challenge is the Sign-In with Ethereum (EIP-4361) message a wallet signs to
// open an upload session: the format MetaMask and Rabby recognise and render
// as a sign-in, bound to this node's host so a page elsewhere cannot collect a
// signature that works here (D31).
func Challenge(domain, uri, address string, chainID uint64, issued, expires time.Time) (string, error) {
	if !common.IsHexAddress(strings.TrimSpace(address)) {
		return "", fmt.Errorf("uploads are signed by an EVM wallet; %q is not a 0x address", address)
	}
	if domain == "" || strings.ContainsAny(domain, " \n/") {
		return "", fmt.Errorf("%q is not a host", domain)
	}
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s wants you to sign in with your Ethereum account:\n%s\n\n%s\n\nURI: %s\nVersion: 1\nChain ID: %d\nNonce: %s\nIssued At: %s\nExpiration Time: %s",
		domain, common.HexToAddress(address).Hex(), SessionStatement, uri, chainID, hex.EncodeToString(nonce),
		issued.UTC().Format(time.RFC3339), expires.UTC().Format(time.RFC3339)), nil
}

// SIWE is the parsed form of an EIP-4361 message, as far as a session needs it.
type SIWE struct {
	Domain    string
	Address   common.Address
	URI       string
	ChainID   uint64
	Nonce     string
	Issued    time.Time
	Expires   time.Time
	Statement string
}

const siweSuffix = " wants you to sign in with your Ethereum account:"

// ParseSIWE reads an EIP-4361 message. It is strict about the parts a session
// depends on and ignores fields it does not use (Resources, Request ID).
func ParseSIWE(message string) (*SIWE, error) {
	lines := strings.Split(strings.ReplaceAll(message, "\r\n", "\n"), "\n")
	if len(lines) < 7 || !strings.HasSuffix(lines[0], siweSuffix) {
		return nil, errors.New("not a Sign-In with Ethereum message")
	}
	m := &SIWE{Domain: strings.TrimSuffix(lines[0], siweSuffix)}
	if !common.IsHexAddress(lines[1]) || common.HexToAddress(lines[1]).Hex() != lines[1] {
		return nil, errors.New("the message's address must be EIP-55 checksummed")
	}
	m.Address = common.HexToAddress(lines[1])
	if lines[2] != "" {
		return nil, errors.New("the address line must be followed by a blank line")
	}
	rest := lines[3:]
	if len(rest) > 0 && !strings.Contains(rest[0], ": ") {
		m.Statement = rest[0]
		if len(rest) < 2 || rest[1] != "" {
			return nil, errors.New("the statement must be followed by a blank line")
		}
		rest = rest[2:]
	}
	fields := map[string]string{}
	for _, line := range rest {
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		fields[k] = v
	}
	for _, k := range []string{"URI", "Version", "Chain ID", "Nonce", "Issued At", "Expiration Time"} {
		if fields[k] == "" {
			return nil, fmt.Errorf("the message has no %s", k)
		}
	}
	if fields["Version"] != "1" {
		return nil, fmt.Errorf("Sign-In with Ethereum version %s is not supported", fields["Version"])
	}
	var err error
	if m.ChainID, err = strconv.ParseUint(fields["Chain ID"], 10, 64); err != nil {
		return nil, errors.New("the message has no readable chain id")
	}
	if m.Issued, err = time.Parse(time.RFC3339, fields["Issued At"]); err != nil {
		return nil, errors.New("the message has no readable issue time")
	}
	if m.Expires, err = time.Parse(time.RFC3339, fields["Expiration Time"]); err != nil {
		return nil, errors.New("the message has no readable expiry")
	}
	if len(fields["Nonce"]) < 8 {
		return nil, errors.New("the nonce is shorter than EIP-4361 allows")
	}
	m.URI, m.Nonce = fields["URI"], fields["Nonce"]
	return m, nil
}

// VerifySession checks a signed sign-in message and returns the account it
// proves and when the session ends. domain is the host the request came to,
// which the message must name.
func VerifySession(req SessionRequest, chainID uint64, domain string, now time.Time) (common.Address, time.Time, error) {
	var zero common.Address
	m, err := ParseSIWE(req.Message)
	if err != nil {
		return zero, time.Time{}, err
	}
	if !strings.EqualFold(m.Domain, domain) {
		return zero, time.Time{}, fmt.Errorf("the message is for %s, this is %s", m.Domain, domain)
	}
	if !common.IsHexAddress(strings.TrimSpace(req.Address)) || common.HexToAddress(req.Address) != m.Address {
		return zero, time.Time{}, errors.New("the message names another address")
	}
	if m.ChainID != chainID {
		return zero, time.Time{}, fmt.Errorf("the message is for chain %d, this is chain %d", m.ChainID, chainID)
	}
	if m.Issued.After(now.Add(5 * time.Minute)) {
		return zero, time.Time{}, errors.New("the message is issued in the future")
	}
	if !m.Expires.After(now) {
		return zero, time.Time{}, errors.New("that session has expired")
	}
	if m.Expires.After(now.Add(SessionMaxAge)) {
		return zero, time.Time{}, fmt.Errorf("a session may last at most %s", SessionMaxAge)
	}
	sig, err := unhex(req.Signature)
	if err != nil {
		return zero, time.Time{}, fmt.Errorf("signature: %w", err)
	}
	if len(sig) != 65 {
		return zero, time.Time{}, errors.New("an EVM signature is 65 bytes")
	}
	if sig[64] >= 27 {
		sig[64] -= 27
	}
	pub, err := crypto.SigToPub(accounts.TextHash([]byte(req.Message)), sig)
	if err != nil {
		return zero, time.Time{}, errors.New("the signature does not verify")
	}
	if crypto.PubkeyToAddress(*pub) != m.Address {
		return zero, time.Time{}, errors.New("the signature is another account's")
	}
	return m.Address, m.Expires, nil
}

// requestOrigin is the host and origin a request reached this node at, which a
// sign-in message is bound to.
func requestOrigin(r *http.Request) (domain, uri string) {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return r.Host, scheme + "://" + r.Host
}

func unhex(s string) ([]byte, error) {
	return hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(s), "0x"))
}

func (s *Store) issue(account common.Address, expires time.Time) string {
	payload := strings.ToLower(account.Hex()) + "|" + strconv.FormatInt(expires.Unix(), 10)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Authenticate reads the bearer token an upload carries.
func (s *Store) Authenticate(header string) (common.Address, error) {
	var zero common.Address
	token := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(header), "Bearer "))
	body, sig, ok := strings.Cut(token, ".")
	if !ok {
		return zero, errors.New("no session token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return zero, errors.New("malformed session token")
	}
	want, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return zero, errors.New("malformed session token")
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(payload)
	if !hmac.Equal(mac.Sum(nil), want) {
		return zero, errors.New("that session token is not this node's")
	}
	addr, expiry, ok := strings.Cut(string(payload), "|")
	if !ok {
		return zero, errors.New("malformed session token")
	}
	unix, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil {
		return zero, errors.New("malformed session token")
	}
	if time.Now().After(time.Unix(unix, 0)) {
		return zero, errors.New("that session has expired; sign again")
	}
	return common.HexToAddress(addr), nil
}

// charge counts an upload against the account's day and the node's, and
// refuses when either is over. The day's accounts are forgotten when the day
// turns, so the map holds one day of uploaders rather than every one ever.
func (s *Store) charge(account common.Address, size int64, now time.Time) error {
	day := now.UTC().Format("2006-01-02")
	key := strings.ToLower(account.Hex())
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.day != day {
		s.day, s.node, s.usage = day, dayUsage{day: day}, map[string]*dayUsage{}
	}
	u, ok := s.usage[key]
	if !ok {
		u = &dayUsage{day: day}
		s.usage[key] = u
	}
	if u.objects+1 > s.cfg.DailyObjects || u.bytes+size > s.cfg.DailyBytes {
		return fmt.Errorf("this account has reached its daily upload limit (%d objects, %d bytes)", s.cfg.DailyObjects, s.cfg.DailyBytes)
	}
	if s.node.objects+1 > s.cfg.NodeDailyObjects || s.node.bytes+size > s.cfg.NodeDailyBytes {
		return errors.New("this node has taken all the uploads it takes in a day; try again tomorrow")
	}
	u.objects++
	u.bytes += size
	s.node.objects++
	s.node.bytes += size
	return nil
}

// --- HTTP -------------------------------------------------------------------

func (s *Store) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /media/{name}", s.handleObject)
	mux.HandleFunc("GET /media/{name}/{id}", s.handleObject)
	mux.HandleFunc("GET /api/media/challenge", s.handleChallenge)
	mux.HandleFunc("POST /api/media/session", s.handleSession)
	mux.HandleFunc("POST /api/media", s.handleUpload)
	mux.HandleFunc("GET /api/media/{hash}", s.handleStat)
}

// Wrap answers the CDN hostname with objects and nothing else: no explorer, no
// API, no uploads. The hostname in front of a cache should only ever serve what
// is safe to cache forever.
func (s *Store) Wrap(next http.Handler) http.Handler {
	if s.publicHost == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if !strings.EqualFold(host, s.publicHost) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "this hostname serves stored objects only", http.StatusMethodNotAllowed)
			return
		}
		m := refRE.FindStringSubmatch(strings.TrimPrefix(r.URL.Path, s.publicPath))
		if m == nil {
			http.NotFound(w, r)
			return
		}
		s.serve(w, r, m[1], m[2])
	})
}

func (s *Store) handleObject(w http.ResponseWriter, r *http.Request) {
	name, _, _ := strings.Cut(r.PathValue("name"), ".")
	id := r.PathValue("id")
	id, _, _ = strings.Cut(id, ".")
	s.serve(w, r, name, id)
}

func (s *Store) serve(w http.ResponseWriter, r *http.Request, hash, id string) {
	if !hashRE.MatchString(hash) {
		http.NotFound(w, r)
		return
	}
	o, err := s.Stat(hash)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if o.Hidden {
		http.Error(w, "this node's operator does not serve this object", http.StatusUnavailableForLegalReasons)
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	// An SVG is a document: served from this hostname it must not be able to do
	// anything but draw itself.
	h.Set("Content-Security-Policy", "default-src 'none'; img-src data:; style-src 'unsafe-inline'; sandbox")

	data, err := os.ReadFile(s.path(hash))
	if err != nil {
		http.Error(w, "the object is recorded but its bytes are missing", http.StatusInternalServerError)
		return
	}
	etag := `"` + hash + `"`
	contentType := o.ContentType
	if id != "" {
		if o.ContentType != "application/json" {
			http.NotFound(w, r)
			return
		}
		data = render(data, id)
		etag = `"` + hash + "-" + id + `"`
		contentType = "application/json"
	}
	h.Set("Content-Type", contentType)
	h.Set("ETag", etag)
	http.ServeContent(w, r, "", time.Unix(o.CreatedAt, 0), strings.NewReader(string(data)))
}

func (s *Store) handleChallenge(w http.ResponseWriter, r *http.Request) {
	address := r.URL.Query().Get("address")
	now := time.Now()
	expires := now.Add(24 * time.Hour)
	domain, uri := requestOrigin(r)
	message, err := Challenge(domain, uri, address, s.cfg.ChainID, now, expires)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": message, "expiresAt": expires.Unix()})
}

func (s *Store) handleSession(w http.ResponseWriter, r *http.Request) {
	var req SessionRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	domain, _ := requestOrigin(r)
	account, expires, err := VerifySession(req, s.cfg.ChainID, domain, time.Now())
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token": s.issue(account, expires), "address": account.Hex(), "expiresAt": expires.Unix(),
	})
}

func (s *Store) handleUpload(w http.ResponseWriter, r *http.Request) {
	account, err := s.Authenticate(r.Header.Get("Authorization"))
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxImageBytes+1))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": err.Error()})
		return
	}
	// Refused before it is charged, and charged only when it takes new disk:
	// bytes this node already holds are the same object and cost nothing.
	if _, err := s.check(data, r.Header.Get("Content-Type")); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	sum := sha256.Sum256(data)
	if o, err := s.Stat(hex.EncodeToString(sum[:])); err == nil {
		writeJSON(w, http.StatusCreated, o)
		return
	}
	if err := s.charge(account, int64(len(data)), time.Now()); err != nil {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": err.Error()})
		return
	}
	o, err := s.Put(data, r.Header.Get("Content-Type"), account.Hex())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.cfg.Log.Info("stored media", "hash", o.Hash, "type", o.ContentType, "bytes", o.Size, "by", account)
	writeJSON(w, http.StatusCreated, o)
}

func (s *Store) handleStat(w http.ResponseWriter, r *http.Request) {
	o, err := s.Stat(strings.TrimSuffix(r.PathValue("hash"), ".json"))
	if errors.Is(err, ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such object"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
