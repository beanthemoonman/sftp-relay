package nas

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"sftp-relay/internal/db"
)

// fakeNAS is an SSH server with an in-memory SFTP subsystem and a canned exec
// handler, standing in for the Synology for unit tests. The real thing is
// covered by the E2E stack.
type fakeNAS struct {
	addr     string
	hostKey  string
	handlers sftp.Handlers

	chmod      *chmodRecorder
	execOutput string // what an exec request echoes back
	execStatus uint32
}

// chmodRecorder stands in for a chmod the in-memory SFTP handler does not
// implement. The real Synology does, so the call is recorded and allowed
// rather than failing the way the in-memory tree would.
type chmodRecorder struct {
	sftp.FileCmder
	mu    sync.Mutex
	modes map[string]os.FileMode
}

func (c *chmodRecorder) Filecmd(r *sftp.Request) error {
	if r.Method == "Setstat" {
		c.mu.Lock()
		c.modes[r.Filepath] = r.Attributes().FileMode().Perm()
		c.mu.Unlock()
		return nil
	}
	return c.FileCmder.Filecmd(r)
}

func (c *chmodRecorder) mode(p string) os.FileMode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.modes[p]
}

func startFakeNAS(t *testing.T) *fakeNAS {
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
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	handlers := sftp.InMemHandler()
	chmod := &chmodRecorder{FileCmder: handlers.FileCmd, modes: map[string]os.FileMode{}}
	handlers.FileCmd = chmod

	n := &fakeNAS{
		addr:       ln.Addr().String(),
		hostKey:    strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))),
		handlers:   handlers,
		chmod:      chmod,
		execOutput: "/opt/bin/lftp\n",
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go n.serve(c, cfg)
		}
	}()
	return n
}

func (n *fakeNAS) serve(c net.Conn, cfg *ssh.ServerConfig) {
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
		go n.session(ch, chReqs)
	}
}

func (n *fakeNAS) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	for req := range reqs {
		switch {
		case req.Type == "subsystem" && strings.Contains(string(req.Payload), "sftp"):
			req.Reply(true, nil) //nolint:errcheck // best effort
			go func() {
				defer ch.Close()
				srv := sftp.NewRequestServer(ch, n.handlers)
				if err := srv.Serve(); err != nil && !errors.Is(err, io.EOF) {
					return
				}
			}()
		case req.Type == "exec":
			req.Reply(true, nil)             //nolint:errcheck // best effort
			io.WriteString(ch, n.execOutput) //nolint:errcheck // best effort
			status := make([]byte, 4)
			binary.BigEndian.PutUint32(status, n.execStatus)
			ch.SendRequest("exit-status", false, status) //nolint:errcheck // best effort
			ch.Close()
		default:
			req.Reply(false, nil) //nolint:errcheck // best effort
		}
	}
}

func (n *fakeNAS) port(t *testing.T) string {
	t.Helper()
	_, p, err := net.SplitHostPort(n.addr)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func writeKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "nas_key")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// setup wires a Client to a fake NAS with /volume1/media allowed and seeded.
func setup(t *testing.T) (*Client, *db.DB, *fakeNAS) {
	t.Helper()
	fake := startFakeNAS(t)
	store, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	if err := store.SetSettings(context.Background(), map[string]string{
		"nas_host":           "127.0.0.1",
		"nas_user":           "relay",
		"nas_port":           fake.port(t),
		"allowed_dest_roots": "/volume1/media",
	}); err != nil {
		t.Fatal(err)
	}

	c := New(store, writeKey(t), 5*time.Second)
	t.Cleanup(func() { c.Close() })

	// Seed the in-memory tree the destination picker will walk.
	if err := c.do(context.Background(), func(sc *sftp.Client) error {
		for _, dir := range []string{"/volume1", "/volume1/media", "/volume1/media/films",
			"/volume1/media/Music", "/volume1/other"} {
			if err := sc.Mkdir(dir); err != nil {
				return err
			}
		}
		f, err := sc.Create("/volume1/media/notes.txt")
		if err != nil {
			return err
		}
		return f.Close()
	}); err != nil {
		t.Fatal(err)
	}
	return c, store, fake
}

func TestBrowseListsRootsWhenPathIsEmpty(t *testing.T) {
	c, store, _ := setup(t)
	ctx := context.Background()
	if err := store.SetSettings(ctx, map[string]string{
		"allowed_dest_roots": "/volume1/media, /volume2/backups",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := c.Browse(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 2 || got.Entries[0].Path != "/volume1/media" {
		t.Fatalf("root listing = %+v", got.Entries)
	}
}

func TestBrowseWithNoRootsFails(t *testing.T) {
	c, store, _ := setup(t)
	ctx := context.Background()
	if err := store.SetSettings(ctx, map[string]string{"allowed_dest_roots": ""}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Browse(ctx, ""); !errors.Is(err, ErrNoRoots) {
		t.Errorf("error = %v, want ErrNoRoots", err)
	}
	if _, err := c.Browse(ctx, "/volume1/media"); !errors.Is(err, ErrNoRoots) {
		t.Errorf("error = %v, want ErrNoRoots", err)
	}
}

func TestBrowseListsDirectoriesOnly(t *testing.T) {
	c, _, _ := setup(t)
	got, err := c.Browse(context.Background(), "/volume1/media")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "/volume1/media" {
		t.Errorf("path = %q", got.Path)
	}
	if got.Parent != "" {
		t.Errorf("parent at a root = %q, want empty", got.Parent)
	}
	var names []string
	for _, e := range got.Entries {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "films,Music" {
		t.Errorf("entries = %v, want films,Music (directories only, case-insensitive order)", names)
	}
}

func TestBrowseBelowARootReportsItsParent(t *testing.T) {
	c, _, _ := setup(t)
	got, err := c.Browse(context.Background(), "/volume1/media/films")
	if err != nil {
		t.Fatal(err)
	}
	if got.Parent != "/volume1/media" {
		t.Errorf("parent = %q, want /volume1/media", got.Parent)
	}
}

func TestBrowseRejectsPathsOutsideTheRoots(t *testing.T) {
	c, _, _ := setup(t)
	for _, p := range []string{"/volume1/other", "/volume1/media/../other", "/etc", "relative"} {
		t.Run(p, func(t *testing.T) {
			if _, err := c.Browse(context.Background(), p); err == nil {
				t.Errorf("browsing %q should be rejected", p)
			}
		})
	}
}

func TestMkdirInsideAndOutsideTheRoots(t *testing.T) {
	c, _, _ := setup(t)
	ctx := context.Background()

	created, err := c.Mkdir(ctx, "/volume1/media/new/nested")
	if err != nil {
		t.Fatal(err)
	}
	if created != "/volume1/media/new/nested" {
		t.Errorf("created = %q", created)
	}
	got, err := c.Browse(ctx, "/volume1/media/new")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 1 || got.Entries[0].Name != "nested" {
		t.Errorf("after mkdir: %+v", got.Entries)
	}

	if _, err := c.Mkdir(ctx, "/volume1/media/../../etc/evil"); !errors.Is(err, ErrOutsideRoots) {
		t.Errorf("mkdir outside the roots: %v, want ErrOutsideRoots", err)
	}
}

func TestValidateRejectsBeforeConnecting(t *testing.T) {
	store, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.SetSettings(context.Background(),
		map[string]string{"allowed_dest_roots": "/volume1/media"}); err != nil {
		t.Fatal(err)
	}
	// No nas_host is configured, so any connection attempt would fail; a bad
	// path must still be rejected as a bad path.
	c := New(store, "/no/such/key", time.Second)
	t.Cleanup(func() { c.Close() })
	if _, err := c.Validate(context.Background(), "/etc/passwd"); !errors.Is(err, ErrOutsideRoots) {
		t.Errorf("error = %v, want ErrOutsideRoots", err)
	}
	if _, err := c.Validate(context.Background(), "/volume1/media"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("error = %v, want ErrNotConfigured", err)
	}
}

func TestHostKeyRecordedThenPinned(t *testing.T) {
	c, store, fake := setup(t)
	ctx := context.Background()
	s, err := store.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s["nas_host_key"] != fake.hostKey {
		t.Fatalf("nas_host_key = %q, want the fake NAS key", s["nas_host_key"])
	}

	// A different key on the same host must be refused, not silently accepted.
	if err := store.SetSettings(ctx, map[string]string{"nas_host_key": "ssh-ed25519 AAAAsomethingelse"}); err != nil {
		t.Fatal(err)
	}
	c.disconnect()
	if _, err := c.Browse(ctx, "/volume1/media"); !errors.Is(err, ErrHostKeyChanged) {
		t.Errorf("error = %v, want ErrHostKeyChanged", err)
	}
}

func TestReconnectAfterTheConnectionDrops(t *testing.T) {
	c, _, _ := setup(t)
	ctx := context.Background()
	if _, err := c.Browse(ctx, "/volume1/media"); err != nil {
		t.Fatal(err)
	}
	c.disconnect()
	if _, err := c.Browse(ctx, "/volume1/media"); err != nil {
		t.Fatalf("browse after a dropped connection: %v", err)
	}
}

func TestProbeLftpCachesThePath(t *testing.T) {
	c, store, _ := setup(t)
	ctx := context.Background()
	got, err := c.ProbeLftp(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/opt/bin/lftp" {
		t.Errorf("lftp path = %q", got)
	}
	s, err := store.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s["lftp_path"] != "/opt/bin/lftp" {
		t.Errorf("cached lftp_path = %q", s["lftp_path"])
	}
}

func TestProbeLftpMissingGivesRemediation(t *testing.T) {
	c, _, fake := setup(t)
	fake.execOutput = "\n"
	_, err := c.ProbeLftp(context.Background())
	if err == nil {
		t.Fatal("want an error when lftp is absent")
	}
	if !strings.Contains(err.Error(), "opkg install lftp") {
		t.Errorf("error = %q, want installation guidance", err)
	}
}

func TestRunReturnsOutput(t *testing.T) {
	c, _, fake := setup(t)
	fake.execOutput = "hello\n"
	out, err := c.Run(context.Background(), "echo hello")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "hello" {
		t.Errorf("out = %q", out)
	}
}

func TestClosedClientRejectsWork(t *testing.T) {
	c, _, _ := setup(t)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := c.Browse(context.Background(), "/volume1/media"); err == nil {
		t.Error("a closed client should refuse work")
	}
}

func TestSettingsRequireHostAndUser(t *testing.T) {
	store, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	c := New(store, "/no/such/key", time.Second)
	t.Cleanup(func() { c.Close() })

	ctx := context.Background()
	if _, err := c.settings(ctx); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("empty settings: %v, want ErrNotConfigured", err)
	}
	if err := store.SetSettings(ctx, map[string]string{"nas_host": "h", "nas_user": "u"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := c.settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.port != 22 {
		t.Errorf("default port = %d, want 22", cfg.port)
	}
	if err := store.SetSettings(ctx, map[string]string{"nas_port": "2222"}); err != nil {
		t.Fatal(err)
	}
	if cfg, _ = c.settings(ctx); cfg.port != 2222 {
		t.Errorf("port = %d, want 2222", cfg.port)
	}
}

func TestMissingKeyFileIsReported(t *testing.T) {
	c, _, _ := setup(t)
	ctx := context.Background()
	c.disconnect()
	c.keyPath = filepath.Join(t.TempDir(), "gone")
	if _, err := c.Browse(ctx, "/volume1/media"); err == nil ||
		!strings.Contains(err.Error(), "reading ssh key") {
		t.Errorf("error = %v, want a readable missing-key error", err)
	}
	// A key file that exists but is not a key gets a distinct message.
	bad := filepath.Join(t.TempDir(), "bad")
	if err := os.WriteFile(bad, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.keyPath = bad
	if _, err := c.Browse(ctx, "/volume1/media"); err == nil ||
		!strings.Contains(err.Error(), "parsing ssh key") {
		t.Errorf("error = %v, want a parse error", err)
	}
}
