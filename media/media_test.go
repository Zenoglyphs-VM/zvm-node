package media

import (
	"bytes"
	"database/sql"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	_ "modernc.org/sqlite"
)

const testChain = 7340470

func testStore(t *testing.T, publicURL string) *Store {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "index.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := Open(Config{Dir: filepath.Join(dir, "media"), PublicURL: publicURL, ChainID: testChain}, db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{0, 213, 87, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSniffReadsTheBytesNotTheClaim(t *testing.T) {
	pngBytes := tinyPNG(t)
	for _, c := range []struct {
		name, declared, want string
		data                 []byte
	}{
		{"png claimed as json", "application/json", "", pngBytes},
		{"png", "image/png", "image/png", pngBytes},
		{"png with no claim", "", "image/png", pngBytes},
		{"json", "application/json", "application/json", []byte(`{"name":"One"}`)},
		{"json with no claim", "", "application/json", []byte(`{"name":"One"}`)},
		{"svg", "image/svg+xml", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect width="2" height="2"/></svg>`)},
		{"html claimed as svg", "image/svg+xml", "", []byte(`<!doctype html><script>alert(1)</script>`)},
		{"broken json", "application/json", "", []byte(`{"name":`)},
		{"text", "text/plain", "", []byte("hello")},
	} {
		got, err := Sniff(c.data, c.declared)
		if c.want == "" {
			if err == nil {
				t.Errorf("%s: accepted as %s", c.name, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %s", c.name, got, err, c.want)
		}
	}
}

func TestPutIsContentAddressedAndResolves(t *testing.T) {
	s := testStore(t, "https://nft.example.org")
	data := tinyPNG(t)
	o, err := s.Put(data, "image/png", "0xabc")
	if err != nil {
		t.Fatal(err)
	}
	if o.URL != "https://nft.example.org/"+o.Hash+".png" {
		t.Errorf("canonical URL %q", o.URL)
	}
	again, err := s.Put(data, "image/png", "0xdef")
	if err != nil || again.Hash != o.Hash || again.Uploader != "0xabc" {
		t.Errorf("the same bytes stored twice became %+v", again)
	}
	meta := []byte(`{"name":"Glyph {id}","image":"https://nft.example.org/` + o.Hash + `.png"}`)
	m, err := s.Put(meta, "application/json", "0xabc")
	if err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{
		"https://nft.example.org/" + o.Hash + ".png",
		"https://nft.example.org/" + o.Hash,
		"/media/" + o.Hash + ".png",
		"http://127.0.0.1:47000/media/" + o.Hash + ".png",
	} {
		got, ok := s.Resolve(uri)
		if !ok || !bytes.Equal(got, data) {
			t.Errorf("did not resolve %s", uri)
		}
	}
	for _, uri := range []string{
		"https://elsewhere.example/" + o.Hash + ".png",
		"ipfs://" + o.Hash,
		"https://nft.example.org/not-a-hash.png",
	} {
		if _, ok := s.Resolve(uri); ok {
			t.Errorf("resolved %s, which this store does not hold", uri)
		}
	}
	rendered, ok := s.Resolve("https://nft.example.org/" + m.Hash + "/7")
	if !ok || !strings.Contains(string(rendered), `"Glyph 7"`) {
		t.Errorf("template for token 7 rendered %s", rendered)
	}
}

// wallet is one signer that keeps its key, the way a wallet does: the address
// in the challenge has to be the address that signs it.
type wallet struct {
	address string
	account common.Address
	sign    func(message string) SessionRequest
}

func evmWallet(t *testing.T) wallet {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	account := crypto.PubkeyToAddress(key.PublicKey)
	return wallet{address: account.Hex(), account: account, sign: func(message string) SessionRequest {
		sig, err := crypto.Sign(accounts.TextHash([]byte(message)), key)
		if err != nil {
			t.Fatal(err)
		}
		sig[64] += 27 // the v a wallet returns
		return SessionRequest{Address: account.Hex(), Message: message, Signature: hex.EncodeToString(sig)}
	}}
}

// Sessions are Sign-In with Ethereum messages bound to this node's host and
// chain, signed by the account that will own the uploads (D31).
func TestSessionsAreSignInWithEthereum(t *testing.T) {
	now := time.Now()
	expires := now.Add(2 * time.Hour)
	const host = "localhost:47000"
	w := evmWallet(t)
	message, err := Challenge(host, "http://"+host, w.address, testChain, now, expires)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseSIWE(message)
	if err != nil {
		t.Fatalf("the challenge is not a SIWE message: %v\n%s", err, message)
	}
	if parsed.Address != w.account || parsed.ChainID != testChain || parsed.Domain != host || parsed.Statement != SessionStatement {
		t.Fatalf("parsed %+v", parsed)
	}
	req := w.sign(message)

	got, gotExpiry, err := VerifySession(req, testChain, host, now)
	if err != nil || got != w.account || gotExpiry.Unix() != expires.Unix() {
		t.Fatalf("%v %v %v", got, gotExpiry, err)
	}
	if _, _, err := VerifySession(req, testChain+1, host, now); err == nil {
		t.Error("a session for another chain was accepted")
	}
	if _, _, err := VerifySession(req, testChain, "evil.example", now); err == nil {
		t.Error("a message for this host was accepted by another")
	}
	if _, _, err := VerifySession(req, testChain, host, expires.Add(time.Second)); err == nil {
		t.Error("an expired session was accepted")
	}
	tampered := req
	tampered.Message = strings.Replace(message, "Chain ID: ", "Chain ID:  ", 1)
	if _, _, err := VerifySession(tampered, testChain, host, now); err == nil {
		t.Error("a rewritten message verified")
	}
	elsewhere := w.sign(message + " ")
	elsewhere.Message = message
	if _, _, err := VerifySession(elsewhere, testChain, host, now); err == nil {
		t.Error("a signature over other bytes verified")
	}
	impostor := evmWallet(t).sign(message)
	impostor.Address = w.address
	if _, _, err := VerifySession(impostor, testChain, host, now); err == nil {
		t.Error("another key's signature verified for this address")
	}
	// A Zenon address can no longer open a session.
	if _, err := Challenge(host, "http://"+host, "z1qqjnwjjpnue8xmmpanz6csze6tcmtzzdtfsww7", testChain, now, expires); err == nil {
		t.Error("a z1 address was offered a challenge")
	}
	// Lowercase addresses are not EIP-55, and EIP-4361 requires it.
	lower := strings.Replace(message, w.account.Hex(), strings.ToLower(w.account.Hex()), 1)
	if _, err := ParseSIWE(lower); err == nil {
		t.Error("an unchecksummed address parsed")
	}
}
func TestTokensRoundTripAndExpire(t *testing.T) {
	s := testStore(t, "")
	account := common.HexToAddress("0x1111111111111111111111111111111111111111")
	token := s.issue(account, time.Now().Add(time.Hour))
	got, err := s.Authenticate("Bearer " + token)
	if err != nil || got != account {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := s.Authenticate("Bearer " + token + "x"); err == nil {
		t.Error("a token with a broken signature was accepted")
	}
	expired := s.issue(account, time.Now().Add(-time.Second))
	if _, err := s.Authenticate("Bearer " + expired); err == nil {
		t.Error("an expired token was accepted")
	}
	other := testStore(t, "")
	if _, err := other.Authenticate("Bearer " + token); err == nil {
		t.Error("another node's token was accepted")
	}
}

func TestUploadNeedsASession(t *testing.T) {
	s := testStore(t, "")
	mux := http.NewServeMux()
	s.Register(mux)

	post := func(auth string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/media", bytes.NewReader(body))
		r.Header.Set("Content-Type", "image/png")
		if auth != "" {
			r.Header.Set("Authorization", "Bearer "+auth)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	if got := post("", tinyPNG(t)).Code; got != http.StatusUnauthorized {
		t.Errorf("an unsigned upload got %d", got)
	}
	account := common.HexToAddress("0x2222222222222222222222222222222222222222")
	token := s.issue(account, time.Now().Add(time.Hour))
	w := post(token, tinyPNG(t))
	if w.Code != http.StatusCreated {
		t.Fatalf("upload got %d: %s", w.Code, w.Body)
	}
	if got := post(token, []byte("just text")).Code; got != http.StatusBadRequest {
		t.Errorf("a text upload got %d", got)
	}
}

func TestObjectsAreServedImmutably(t *testing.T) {
	s := testStore(t, "https://nft.example.org")
	o, err := s.Put(tinyPNG(t), "image/png", "0xabc")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.Register(mux)
	handler := s.Wrap(mux)

	r := httptest.NewRequest(http.MethodGet, "/media/"+o.Hash+".png", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("node path got %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control %q", cc)
	}
	if w.Header().Get("ETag") != `"`+o.Hash+`"` {
		t.Errorf("ETag %q", w.Header().Get("ETag"))
	}

	// The CDN hostname serves objects at the root, and nothing else at all.
	cdn := httptest.NewRequest(http.MethodGet, "/"+o.Hash+".png", nil)
	cdn.Host = "nft.example.org"
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, cdn)
	if w.Code != http.StatusOK {
		t.Errorf("CDN hostname got %d", w.Code)
	}
	for _, path := range []string{"/api/info", "/", "/api/media/challenge?address=0x1"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Host = "nft.example.org"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("CDN hostname answered %s with %d", path, w.Code)
		}
	}

	if err := s.SetHidden(o.Hash, true); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/media/"+o.Hash+".png", nil))
	if w.Code != http.StatusUnavailableForLegalReasons {
		t.Errorf("a withheld object got %d", w.Code)
	}
}

// uploader posts bytes as a fresh account each time: an account is a key and
// one signature, both free, so this is the shape a disk-filling script takes.
func uploader(t *testing.T, s *Store) func(body []byte) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	s.Register(mux)
	return func(body []byte) *httptest.ResponseRecorder {
		key, err := crypto.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/api/media", bytes.NewReader(body))
		r.Header.Set("Content-Type", "image/png")
		r.Header.Set("Authorization", "Bearer "+s.issue(crypto.PubkeyToAddress(key.PublicKey), time.Now().Add(time.Hour)))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
}

// pngOf is a distinct valid PNG for every n, so each upload is new bytes.
func pngOf(t *testing.T, n int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{uint8(n), uint8(n >> 8), 87, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestTheNodeHasADailyCapWhateverTheAccounts(t *testing.T) {
	s := testStore(t, "")
	s.cfg.NodeDailyObjects = 3
	post := uploader(t, s)
	for i := 0; i < 3; i++ {
		if w := post(pngOf(t, i)); w.Code != http.StatusCreated {
			t.Fatalf("upload %d from a fresh account: %d %s", i, w.Code, w.Body)
		}
	}
	if w := post(pngOf(t, 3)); w.Code != http.StatusTooManyRequests {
		t.Errorf("a fourth fresh account past the node's day got %d, want 429", w.Code)
	}
	// Bytes the node already holds take no disk, so they cost nothing.
	if w := post(pngOf(t, 0)); w.Code != http.StatusCreated {
		t.Errorf("re-uploading stored bytes past the cap got %d", w.Code)
	}
	// A new day is a new budget, and the old day's uploaders are forgotten.
	if err := s.charge(common.HexToAddress("0x01"), 1, time.Now().Add(24*time.Hour)); err != nil {
		t.Errorf("the next day: %v", err)
	}
	if len(s.usage) != 1 {
		t.Errorf("the new day still holds %d accounts", len(s.usage))
	}
}

func TestARefusedUploadSpendsNoQuota(t *testing.T) {
	s := testStore(t, "")
	s.cfg.NodeDailyObjects = 1
	post := uploader(t, s)
	if w := post([]byte("not an image")); w.Code != http.StatusBadRequest {
		t.Fatalf("a text upload got %d", w.Code)
	}
	if w := post(pngOf(t, 1)); w.Code != http.StatusCreated {
		t.Errorf("the refused upload spent the node's only slot: %d %s", w.Code, w.Body)
	}
}
