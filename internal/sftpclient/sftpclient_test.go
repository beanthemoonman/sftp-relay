package sftpclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"sftp-relay/internal/db"
)

const testPassword = "pw"

// testServer is a real SSH server with an in-memory SFTP subsystem, so browsing
// is exercised over an actual protocol exchange rather than a stub.
type testServer struct {
	addr     string
	hostKey  string
	handlers sftp.Handlers
}

func startServer(t *testing.T) *testServer {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if c.User() == "u" && string(pass) == testPassword {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	s := &testServer{
		addr:     ln.Addr().String(),
		hostKey:  strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))),
		handlers: sftp.InMemHandler(),
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c, cfg)
		}
	}()
	return s
}

func (s *testServer) serve(c net.Conn, cfg *ssh.ServerConfig) {
	defer c.Close()
	conn, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(ssh.UnknownChannelType, "only sessions") //nolint:errcheck // best effort
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			return
		}
		go func() {
			for req := range chReqs {
				ok := req.Type == "subsystem" && strings.Contains(string(req.Payload), "sftp")
				req.Reply(ok, nil) //nolint:errcheck // best effort
			}
		}()
		go func() {
			defer ch.Close()
			srv := sftp.NewRequestServer(ch, s.handlers)
			if err := srv.Serve(); err != nil && !errors.Is(err, io.EOF) {
				return
			}
		}()
	}
}

func (s *testServer) port(t *testing.T) int {
	t.Helper()
	_, p, err := net.SplitHostPort(s.addr)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	for _, r := range p {
		n = n*10 + int(r-'0')
	}
	return n
}

func newStore(t *testing.T) *db.DB {
	t.Helper()
	store, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// setup returns a pool plus the stored server row pointing at the test server.
func setup(t *testing.T, hostKey string) (*Pool, *db.DB, db.Server) {
	t.Helper()
	s := startServer(t)
	store := newStore(t)
	switch hostKey {
	case "wrong":
		hostKey = "ssh-ed25519 AAAAsomethingelse"
	case "pinned":
		hostKey = s.hostKey
	}
	row := db.Server{
		Name: "test", Host: "127.0.0.1", Port: s.port(t), Username: "u",
		AuthType: "password", Password: testPassword, HostKey: hostKey,
		DefaultRemotePath: "/",
	}
	id, err := store.CreateServer(context.Background(), row)
	if err != nil {
		t.Fatal(err)
	}
	row.ID = id
	p := NewPool(store, time.Minute, 5*time.Second)
	t.Cleanup(func() { p.Close() })
	return p, store, row
}

func seed(t *testing.T, p *Pool, s db.Server) {
	t.Helper()
	ctx := context.Background()
	err := p.do(ctx, s, func(c *sftp.Client) error {
		for _, dir := range []string{"/dir", "/dir/sub", "/dir/zsub"} {
			if err := c.Mkdir(dir); err != nil {
				return err
			}
		}
		files := map[string]string{
			"/dir/a.txt":     "0123456789",
			"/dir/empty.txt": "",
			"/dir/sub/b.txt": "12345",
		}
		for name, body := range files {
			f, err := c.Create(name)
			if err != nil {
				return err
			}
			if body != "" {
				if _, err := f.Write([]byte(body)); err != nil {
					return err
				}
			}
			if err := f.Close(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestListSortsDirectoriesFirst(t *testing.T) {
	p, _, s := setup(t, "")
	seed(t, p, s)

	entries, err := p.List(context.Background(), s, "/dir")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name)
	}
	want := []string{"sub", "zsub", "a.txt", "empty.txt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("entries = %v, want %v", got, want)
	}
	for _, e := range entries {
		switch e.Name {
		case "sub", "zsub":
			if !e.IsDir {
				t.Errorf("%s should be a directory", e.Name)
			}
		case "a.txt":
			if e.Size != 10 {
				t.Errorf("a.txt size = %d, want 10", e.Size)
			}
		case "empty.txt":
			if e.Size != 0 {
				t.Errorf("empty.txt size = %d, want 0", e.Size)
			}
		}
		if e.Mode == "" {
			t.Errorf("%s has no mode string", e.Name)
		}
	}
}

func TestListMissingDirectoryErrors(t *testing.T) {
	p, _, s := setup(t, "")
	if _, err := p.List(context.Background(), s, "/nope"); err == nil {
		t.Fatal("listing a missing directory should fail")
	}
}

func TestSize(t *testing.T) {
	p, _, s := setup(t, "")
	seed(t, p, s)
	ctx := context.Background()

	total, isDir, err := p.Size(ctx, s, "/dir")
	if err != nil {
		t.Fatal(err)
	}
	if !isDir || total != 15 {
		t.Errorf("Size(/dir) = %d, isDir=%v, want 15, true", total, isDir)
	}

	total, isDir, err = p.Size(ctx, s, "/dir/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if isDir || total != 10 {
		t.Errorf("Size(/dir/a.txt) = %d, isDir=%v, want 10, false", total, isDir)
	}

	if _, _, err := p.Size(ctx, s, "/missing"); err == nil {
		t.Error("sizing a missing path should fail")
	}
}

func TestHostKeyRecordedOnFirstConnect(t *testing.T) {
	p, store, s := setup(t, "")
	if _, err := p.List(context.Background(), s, "/"); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetServer(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored.HostKey, "ssh-ed25519 ") {
		t.Errorf("host key = %q, want the server's key recorded", stored.HostKey)
	}
}

func TestHostKeyPinnedAndChangeRejected(t *testing.T) {
	p, _, s := setup(t, "pinned")
	if _, err := p.List(context.Background(), s, "/"); err != nil {
		t.Fatalf("a matching pinned key should connect: %v", err)
	}

	p2, _, s2 := setup(t, "wrong")
	_, err := p2.List(context.Background(), s2, "/")
	if !errors.Is(err, ErrHostKeyChanged) {
		t.Fatalf("error = %v, want ErrHostKeyChanged", err)
	}
}

func TestTestReportsTiming(t *testing.T) {
	p, _, s := setup(t, "")
	seed(t, p, s)
	res, err := p.Test(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Entries != 1 || !res.HostKeyNew || res.HostKey == "" {
		t.Errorf("result = %+v", res)
	}
	if !strings.Contains(res.Description, "listed 1 entries") {
		t.Errorf("description = %q", res.Description)
	}
}

func TestConnectionsAreReusedAndEvicted(t *testing.T) {
	p, _, s := setup(t, "")
	ctx := context.Background()
	if _, err := p.List(ctx, s, "/"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.List(ctx, s, "/"); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	n := len(p.conns)
	// Age the connection past the idle window without sleeping.
	for _, c := range p.conns {
		c.lastUsed = time.Now().Add(-time.Hour)
	}
	p.mu.Unlock()
	if n != 1 {
		t.Fatalf("pooled connections = %d, want 1", n)
	}

	p.evictIdle()
	p.mu.Lock()
	n = len(p.conns)
	p.mu.Unlock()
	if n != 0 {
		t.Errorf("connections after eviction = %d, want 0", n)
	}

	// A new call transparently redials.
	if _, err := p.List(ctx, s, "/"); err != nil {
		t.Fatalf("redial after eviction: %v", err)
	}
}

func TestClosedPoolRejectsWork(t *testing.T) {
	p, _, s := setup(t, "")
	if _, err := p.List(context.Background(), s, "/"); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := p.List(context.Background(), s, "/"); err == nil {
		t.Error("a closed pool should refuse work")
	}
}

func TestUnreachableAndBadAuth(t *testing.T) {
	store := newStore(t)
	p := NewPool(store, time.Minute, 200*time.Millisecond)
	t.Cleanup(func() { p.Close() })
	ctx := context.Background()

	tests := []struct {
		name string
		srv  db.Server
	}{
		{"unknown auth type", db.Server{ID: 1, Name: "x", Host: "127.0.0.1", Port: 1, AuthType: "magic"}},
		{"unparseable private key", db.Server{ID: 2, Name: "x", Host: "127.0.0.1", Port: 1, AuthType: "key", PrivateKey: "not-a-key"}},
		{"connection refused", db.Server{ID: 3, Name: "x", Host: "127.0.0.1", Port: 1, AuthType: "password", Password: "p"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := p.List(ctx, tt.srv, "/")
			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), "not-a-key") {
				t.Error("the error leaked key material")
			}
		})
	}
}

func TestWrongPasswordIsRejected(t *testing.T) {
	p, store, s := setup(t, "")
	s.Password = "wrong"
	if err := store.UpdateServer(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if _, err := p.List(context.Background(), s, "/"); err == nil {
		t.Fatal("a wrong password should not connect")
	}
}

func TestCleanPath(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "/"},
		{"/", "/"},
		{"a/b", "/a/b"},
		{"/a//b/", "/a/b"},
		{"/a/./b", "/a/b"},
		{"/a/b/..", "/a"},
		{"/a/../../..", "/"},
	}
	for _, tt := range tests {
		if got := CleanPath(tt.in); got != tt.want {
			t.Errorf("CleanPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
