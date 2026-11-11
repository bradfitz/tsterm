//go:build unix

package tcp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/bradfitz/tsterm/pkg/auth"
	"github.com/bradfitz/tsterm/pkg/pty"
	"github.com/bradfitz/tsterm/pkg/session"
	"github.com/bradfitz/tsterm/pkg/session/sessiontest"
	"github.com/bradfitz/tsterm/pkg/transport/ws"
)

const testFQDN = "braid.example.ts.net"

// testCert returns a self-signed certificate for testFQDN.
func testCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: testFQDN},
		DNSNames:     []string{testFQDN},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// startTLSServer serves s on a new port, with HTTPS for testFQDN, and
// returns the port.
func startTLSServer(t *testing.T) (*Server, string) {
	t.Helper()
	a, err := auth.NewSelfUser(context.Background(), &allowAllLC{user: 1})
	if err != nil {
		t.Fatal(err)
	}
	mgr := sessiontest.NewManager(t)
	srv := &Server{
		Manager:   mgr,
		Auth:      a,
		WSHandler: (&ws.Server{Manager: mgr}).Handler(),
		TLS:       &tls.Config{Certificates: []tls.Certificate{testCert(t)}, NextProtos: []string{"http/1.1"}},
		HTTPSHost: testFQDN,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go Serve(ctx, ln, srv)
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return srv, port
}

// httpsClient returns a client that connects to 127.0.0.1:port for
// testFQDN and trusts the test certificate.
func httpsClient(port string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", "127.0.0.1:"+port)
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: testFQDN},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func TestHTTPSAndHTTPOnOnePort(t *testing.T) {
	srv, port := startTLSServer(t)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// A cleartext request for a page, by any name, is sent to HTTPS on
	// the FQDN and the same port.
	for _, path := range []string{"/", "/s/pty-X?ro", "/static/viewer.js"} {
		resp, err := noRedirect.Get("http://127.0.0.1:" + port + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		want := "https://" + testFQDN + ":" + port + path
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != want {
			t.Errorf("GET %s: %d, Location %q; want 302 to %q", path, resp.StatusCode, resp.Header.Get("Location"), want)
		}
	}

	// The API stays on cleartext.
	resp, err := noRedirect.Get("http://127.0.0.1:" + port + "/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("cleartext API: %d, want 200", resp.StatusCode)
	}

	// HTTPS on the same port serves the pages.
	c := httpsClient(port)
	resp, err = c.Get("https://" + testFQDN + ":" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.TLS == nil || !strings.Contains(string(body), "tsterm sessions") {
		t.Errorf("HTTPS index: %d, TLS %v", resp.StatusCode, resp.TLS != nil)
	}

	// And the WebSocket viewer works over TLS.
	sess, err := srv.Manager.Create(session.Config{PTY: pty.Config{Command: "/bin/cat"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "wss://"+testFQDN+":"+port+"/v1/sessions/"+string(sess.ID())+"/ws", &websocket.DialOptions{HTTPClient: c})
	if err != nil {
		t.Fatalf("wss: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(1 << 20)
	if _, _, err := conn.Read(ctx); err != nil {
		t.Errorf("wss read: %v", err)
	}
}

func TestNoRedirectWithoutHTTPS(t *testing.T) {
	_, hts := newTestServer(t, &allowAllLC{user: 1}) // no TLS, no HTTPSHost
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Get(hts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("GET / without HTTPS: %d, want 200", resp.StatusCode)
	}
}

func TestSniffListenerSlowClient(t *testing.T) {
	// A connection that never sends anything must not hold up others.
	_, port := startTLSServer(t)
	idle, err := net.Dial("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	resp, err := httpsClient(port).Get("https://" + testFQDN + ":" + port + "/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status %d", resp.StatusCode)
	}
}
