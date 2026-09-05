// Package sftpclient browses remote SFTP servers on behalf of the relay.
// Bytes are never transferred here — only listings, stats and connection tests.
package sftpclient

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"sftp-relay/internal/db"
)

// ErrHostKeyChanged is returned when a server presents a key other than the
// one recorded on first connect. It is never auto-accepted.
var ErrHostKeyChanged = errors.New("sftpclient: host key changed")

// Entry is one directory entry as the UI needs it.
type Entry struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	ModTime time.Time `json:"mod_time"`
	IsDir   bool      `json:"is_dir"`
}

// TestResult is what POST /api/servers/{id}/test reports back.
type TestResult struct {
	OK          bool   `json:"ok"`
	Entries     int    `json:"entries"`
	ElapsedMS   int64  `json:"elapsed_ms"`
	HostKey     string `json:"host_key"`
	HostKeyNew  bool   `json:"host_key_new"`
	Description string `json:"description"`
}

type conn struct {
	ssh      *ssh.Client
	sftp     *sftp.Client
	lastUsed time.Time
}

func (c *conn) close() {
	c.sftp.Close()
	c.ssh.Close()
}

// Pool keeps at most one connection per server alive, evicting idle ones so a
// long-lived process does not sit on sockets.
//
// ponytail: one connection per server, held under a mutex, so browsing a single
// server is serialised. Fine for a one-user tool; give each server a small
// channel-based connection set if parallel listings ever matter.
type Pool struct {
	store   *db.DB
	idle    time.Duration
	timeout time.Duration

	mu     sync.Mutex
	conns  map[int64]*conn
	closed bool

	stop chan struct{}
	done chan struct{}
}

// NewPool starts the idle-eviction janitor. Close stops it.
func NewPool(store *db.DB, idle, timeout time.Duration) *Pool {
	p := &Pool{
		store:   store,
		idle:    idle,
		timeout: timeout,
		conns:   map[int64]*conn{},
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go p.janitor()
	return p
}

func (p *Pool) janitor() {
	defer close(p.done)
	t := time.NewTicker(p.idle / 2)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.evictIdle()
		}
	}
}

func (p *Pool) evictIdle() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, c := range p.conns {
		if time.Since(c.lastUsed) > p.idle {
			c.close()
			delete(p.conns, id)
			slog.Debug("sftpclient: evicted idle connection", "server_id", id)
		}
	}
}

// Close drops every pooled connection and stops the janitor.
func (p *Pool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	for id, c := range p.conns {
		c.close()
		delete(p.conns, id)
	}
	p.mu.Unlock()

	close(p.stop)
	<-p.done
	return nil
}

// do runs fn against a connection to s, dialling or redialling as needed.
func (p *Pool) do(ctx context.Context, s db.Server, fn func(*sftp.Client) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("sftpclient: pool is closed")
	}

	c, ok := p.conns[s.ID]
	if ok {
		if err := fn(c.sftp); err == nil {
			c.lastUsed = time.Now()
			return nil
		} else if !isBroken(err) {
			c.lastUsed = time.Now()
			return err
		}
		c.close()
		delete(p.conns, s.ID)
	}

	c, err := p.dial(ctx, s)
	if err != nil {
		return err
	}
	p.conns[s.ID] = c
	if err := fn(c.sftp); err != nil {
		return err
	}
	c.lastUsed = time.Now()
	return nil
}

func isBroken(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, sftp.ErrSSHFxConnectionLost) ||
		strings.Contains(err.Error(), "EOF")
}

func (p *Pool) dial(ctx context.Context, s db.Server) (*conn, error) {
	auth, err := authMethods(s)
	if err != nil {
		return nil, err
	}

	var observed string
	cfg := &ssh.ClientConfig{
		User:    s.Username,
		Auth:    auth,
		Timeout: p.timeout,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			observed = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
			if s.HostKey == "" {
				return nil // trust on first use; the key is recorded below
			}
			if observed != strings.TrimSpace(s.HostKey) {
				return fmt.Errorf("%w for %s: refusing to connect; clear the stored key "+
					"in the server settings if this change was expected", ErrHostKeyChanged, s.Host)
			}
			return nil
		},
	}

	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	d := net.Dialer{Timeout: p.timeout}
	rawConn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("sftpclient: dial %s: %w", addr, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := rawConn.SetDeadline(deadline); err != nil {
			rawConn.Close()
			return nil, fmt.Errorf("sftpclient: set deadline for %s: %w", addr, err)
		}
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(rawConn, addr, cfg)
	if err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("sftpclient: ssh handshake with %s: %w", addr, err)
	}
	// The handshake deadline must not outlive the handshake itself.
	if err := rawConn.SetDeadline(time.Time{}); err != nil {
		sshConn.Close()
		return nil, fmt.Errorf("sftpclient: clear deadline for %s: %w", addr, err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)

	sc, err := sftp.NewClient(client)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("sftpclient: open sftp subsystem on %s: %w", addr, err)
	}

	if s.HostKey == "" && observed != "" {
		if err := p.store.SetServerHostKey(ctx, s.ID, observed); err != nil {
			sc.Close()
			client.Close()
			return nil, fmt.Errorf("sftpclient: record host key for %s: %w", s.Host, err)
		}
		slog.Info("sftpclient: recorded host key on first connect", "server_id", s.ID, "host", s.Host)
	}
	return &conn{ssh: client, sftp: sc, lastUsed: time.Now()}, nil
}

func authMethods(s db.Server) ([]ssh.AuthMethod, error) {
	switch s.AuthType {
	case "password":
		return []ssh.AuthMethod{ssh.Password(s.Password)}, nil
	case "key":
		var (
			signer ssh.Signer
			err    error
		)
		if s.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(s.PrivateKey), []byte(s.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(s.PrivateKey))
		}
		if err != nil {
			// The error text from x/crypto never contains key material.
			return nil, fmt.Errorf("sftpclient: parse private key for %q: %w", s.Name, err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	default:
		return nil, fmt.Errorf("sftpclient: server %q has unknown auth_type %q", s.Name, s.AuthType)
	}
}

// CleanPath normalises a remote path. Remote servers have no allow-list — the
// user may browse anywhere they can log in to — but the path must be absolute
// and free of traversal segments so a listing cannot be tricked into wandering.
func CleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

// List returns the entries of dir, directories first then name, both ascending.
func (p *Pool) List(ctx context.Context, s db.Server, dir string) ([]Entry, error) {
	dir = CleanPath(dir)
	var out []Entry
	err := p.do(ctx, s, func(c *sftp.Client) error {
		infos, err := c.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("sftpclient: list %s on %q: %w", dir, s.Name, err)
		}
		out = make([]Entry, 0, len(infos))
		for _, fi := range infos {
			out = append(out, entryOf(fi))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortEntries(out)
	return out, nil
}

func entryOf(fi fs.FileInfo) Entry {
	return Entry{
		Name:    fi.Name(),
		Size:    fi.Size(),
		Mode:    fi.Mode().String(),
		ModTime: fi.ModTime().UTC(),
		IsDir:   fi.IsDir(),
	}
}

func sortEntries(e []Entry) {
	sort.Slice(e, func(i, j int) bool {
		if e[i].IsDir != e[j].IsDir {
			return e[i].IsDir
		}
		return strings.ToLower(e[i].Name) < strings.ToLower(e[j].Name)
	})
}

// Size reports the total byte count under remote and whether it is a directory.
// This is what makes an accurate progress bar possible later.
//
// ponytail: the directory walk is sequential. It is metadata only, and the
// bottleneck is the server's round-trip; parallelise it if a deep tree drags.
func (p *Pool) Size(ctx context.Context, s db.Server, remote string) (total int64, isDir bool, err error) {
	remote = CleanPath(remote)
	err = p.do(ctx, s, func(c *sftp.Client) error {
		fi, err := c.Stat(remote)
		if err != nil {
			return fmt.Errorf("sftpclient: stat %s on %q: %w", remote, s.Name, err)
		}
		if !fi.IsDir() {
			total, isDir = fi.Size(), false
			return nil
		}
		isDir = true
		w := c.Walk(remote)
		for w.Step() {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("sftpclient: walk %s on %q: %w", remote, s.Name, err)
			}
			if err := w.Err(); err != nil {
				return fmt.Errorf("sftpclient: walk %s on %q: %w", remote, s.Name, err)
			}
			if st := w.Stat(); st != nil && !st.IsDir() {
				total += st.Size()
			}
		}
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	return total, isDir, nil
}

// Test connects, lists the server's default path and reports how long it took.
func (p *Pool) Test(ctx context.Context, s db.Server) (TestResult, error) {
	// A fresh connection is the point of the test, so drop any pooled one first.
	p.mu.Lock()
	if c, ok := p.conns[s.ID]; ok {
		c.close()
		delete(p.conns, s.ID)
	}
	p.mu.Unlock()

	root := CleanPath(s.DefaultRemotePath)
	start := time.Now()
	entries, err := p.List(ctx, s, root)
	if err != nil {
		return TestResult{}, err
	}
	res := TestResult{
		OK:          true,
		Entries:     len(entries),
		ElapsedMS:   time.Since(start).Milliseconds(),
		HostKeyNew:  s.HostKey == "",
		Description: fmt.Sprintf("listed %d entries under %s", len(entries), root),
	}
	if updated, err := p.store.GetServer(ctx, s.ID); err == nil {
		res.HostKey = updated.HostKey
	}
	return res, nil
}
