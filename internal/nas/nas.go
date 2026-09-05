// Package nas talks to the Synology: destination browsing, directory creation,
// path validation and (from Phase 5) running lftp there. It is the only place
// that holds an SSH connection to the NAS.
package nas

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
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

// ErrNotConfigured means the NAS host or user has not been set in Settings yet.
var ErrNotConfigured = errors.New("nas: not configured — set the NAS host and user in Settings")

// ErrHostKeyChanged mirrors sftpclient's: a changed key is never auto-accepted.
var ErrHostKeyChanged = errors.New("nas: host key changed")

// lftpCandidates are the usual Synology locations. `command -v` is tried first;
// non-interactive SSH on Synology has a famously thin PATH.
var lftpCandidates = []string{
	"/opt/bin/lftp", "/usr/local/bin/lftp", "/usr/bin/lftp", "/bin/lftp",
	"/volume1/@entware/opt/bin/lftp", "/usr/local/lftp/bin/lftp",
}

// Entry is a destination directory as the UI needs it.
type Entry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
}

// Listing is the payload of GET /api/nas/browse.
type Listing struct {
	Path       string  `json:"path"`
	Parent     string  `json:"parent"`
	Entries    []Entry `json:"entries"`
	FreeBytes  uint64  `json:"free_bytes"`
	TotalBytes uint64  `json:"total_bytes"`
}

// Client is a lazily-connected, self-healing SSH+SFTP session to the NAS.
type Client struct {
	store   *db.DB
	keyPath string
	timeout time.Duration

	mu       sync.Mutex
	ssh      *ssh.Client
	sftp     *sftp.Client
	prefixes map[string]string // share name -> shell path prefix

	closed bool
	stop   chan struct{}
	done   chan struct{}
}

// New starts the keepalive loop. Close stops it.
func New(store *db.DB, keyPath string, timeout time.Duration) *Client {
	c := &Client{
		store:    store,
		keyPath:  keyPath,
		timeout:  timeout,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		prefixes: map[string]string{},
	}
	go c.keepalive()
	return c
}

// keepalive pokes the connection so a NAT table or the Synology's idle timer
// does not silently drop it between jobs.
func (c *Client) keepalive() {
	defer close(c.done)
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
			c.mu.Lock()
			client := c.ssh
			c.mu.Unlock()
			if client == nil {
				continue
			}
			if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				slog.Debug("nas: keepalive failed, dropping connection", "err", err)
				c.disconnect()
			}
		}
	}
}

// Close drops the connection and stops the keepalive loop.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	close(c.stop)
	<-c.done
	c.disconnect()
	return nil
}

func (c *Client) disconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sftp != nil {
		if err := c.sftp.Close(); err != nil {
			slog.Error("failed to close sftp client", "err", err)
		}
		c.sftp = nil
	}
	if c.ssh != nil {
		if err := c.ssh.Close(); err != nil {
			slog.Error("failed to close ssh client", "err", err)
		}
		c.ssh = nil
	}
}

// do runs fn against a live SFTP client, reconnecting once if the session died.
func (c *Client) do(ctx context.Context, fn func(*sftp.Client) error) error {
	for attempt := range 2 {
		sc, err := c.connect(ctx)
		if err != nil {
			return err
		}
		err = fn(sc)
		if err != nil && attempt == 0 && isBroken(err) {
			c.disconnect()
			continue
		}
		return err
	}
	return errors.New("nas: unreachable")
}

func isBroken(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, sftp.ErrSSHFxConnectionLost) ||
		strings.Contains(err.Error(), "EOF")
}

// connect returns the live SFTP client, dialling if there isn't one.
func (c *Client) connect(ctx context.Context) (*sftp.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("nas: client is closed")
	}
	if c.sftp != nil {
		return c.sftp, nil
	}

	cfg, err := c.settings(ctx)
	if err != nil {
		return nil, err
	}
	key, err := os.ReadFile(c.keyPath)
	if err != nil {
		return nil, fmt.Errorf("nas: reading ssh key %s: %w", c.keyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("nas: parsing ssh key %s: %w", c.keyPath, err)
	}

	var observed string
	addr := net.JoinHostPort(cfg.host, strconv.Itoa(cfg.port))
	clientCfg := &ssh.ClientConfig{
		User:    cfg.user,
		Auth:    []ssh.AuthMethod{ssh.PublicKeys(signer)},
		Timeout: c.timeout,
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			observed = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
			if cfg.hostKey == "" {
				return nil // trust on first use, then pinned
			}
			if observed != cfg.hostKey {
				return fmt.Errorf("%w for %s: refusing to connect; clear nas_host_key "+
					"in Settings if this change was expected", ErrHostKeyChanged, cfg.host)
			}
			return nil
		},
	}

	d := net.Dialer{Timeout: c.timeout}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("nas: dial %s: %w", addr, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := raw.SetDeadline(deadline); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("nas: set deadline for %s: %w", addr, err)
		}
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(raw, addr, clientCfg)
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("nas: ssh handshake with %s: %w", addr, err)
	}
	if err := raw.SetDeadline(time.Time{}); err != nil {
		_ = sshConn.Close()
		return nil, fmt.Errorf("nas: clear deadline for %s: %w", addr, err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)

	sc, err := sftp.NewClient(client)
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("nas: open sftp subsystem on %s: %w", addr, err)
	}

	if cfg.hostKey == "" && observed != "" {
		if err := c.store.SetSettings(ctx, map[string]string{"nas_host_key": observed}); err != nil {
			_ = sc.Close()
			_ = client.Close()
			return nil, fmt.Errorf("nas: record host key: %w", err)
		}
		slog.Info("nas: recorded host key on first connect", "host", cfg.host)
	}

	c.ssh, c.sftp = client, sc
	slog.Info("nas: connected", "host", cfg.host, "user", cfg.user)
	return sc, nil
}

type nasConfig struct {
	host, user, hostKey string
	port                int
}

func (c *Client) settings(ctx context.Context) (nasConfig, error) {
	s, err := c.store.Settings(ctx)
	if err != nil {
		return nasConfig{}, fmt.Errorf("nas: loading settings: %w", err)
	}
	cfg := nasConfig{
		host:    strings.TrimSpace(s["nas_host"]),
		user:    strings.TrimSpace(s["nas_user"]),
		hostKey: strings.TrimSpace(s["nas_host_key"]),
		port:    22,
	}
	if p, err := strconv.Atoi(strings.TrimSpace(s["nas_port"])); err == nil && p > 0 {
		cfg.port = p
	}
	if cfg.host == "" || cfg.user == "" {
		return nasConfig{}, ErrNotConfigured
	}
	return cfg, nil
}

// SplitRoots parses the allowed_dest_roots setting: comma or newline separated.
func SplitRoots(v string) []string {
	var out []string
	for _, r := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '\n' }) {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}

// Roots returns the configured allowed destination roots.
func (c *Client) Roots(ctx context.Context) ([]string, error) {
	s, err := c.store.Settings(ctx)
	if err != nil {
		return nil, fmt.Errorf("nas: loading settings: %w", err)
	}
	return SplitRoots(s["allowed_dest_roots"]), nil
}

// Validate is CheckPath against the live NAS, following real symlinks.
func (c *Client) Validate(ctx context.Context, p string) (string, error) {
	roots, err := c.Roots(ctx)
	if err != nil {
		return "", err
	}
	// Reject anything outside the roots before opening a connection: a bad
	// path is a bad path whether or not the NAS is reachable.
	clean, err := CheckPath(p, roots, nil)
	if err != nil {
		return "", err
	}
	var out string
	err = c.do(ctx, func(sc *sftp.Client) error {
		var cerr error
		out, cerr = CheckPath(clean, roots, sc.RealPath)
		return cerr
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

// Browse lists destination directories. An empty path lists the allowed roots
// themselves, which is where the destination picker starts.
func (c *Client) Browse(ctx context.Context, p string) (Listing, error) {
	roots, err := c.Roots(ctx)
	if err != nil {
		return Listing{}, err
	}
	if len(roots) == 0 {
		return Listing{}, ErrNoRoots
	}
	if strings.TrimSpace(p) == "" {
		out := Listing{Path: "", Entries: make([]Entry, 0, len(roots))}
		for _, r := range roots {
			clean, err := normalise(r)
			if err != nil {
				continue
			}
			out.Entries = append(out.Entries, Entry{Name: clean, Path: clean, IsDir: true})
		}
		return out, nil
	}

	dir, err := c.Validate(ctx, p)
	if err != nil {
		return Listing{}, err
	}
	// At a root the picker goes back to the root list rather than up the tree.
	out := Listing{Path: dir, Parent: path.Dir(dir)}
	for _, r := range roots {
		if clean, err := normalise(r); err == nil && clean == dir {
			out.Parent = ""
		}
	}
	err = c.do(ctx, func(sc *sftp.Client) error {
		infos, err := sc.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("nas: list %s: %w", dir, err)
		}
		out.Entries = make([]Entry, 0, len(infos))
		for _, fi := range infos {
			if !fi.IsDir() {
				continue // destinations are directories only
			}
			out.Entries = append(out.Entries, Entry{
				Name: fi.Name(), Path: path.Join(dir, fi.Name()), IsDir: true,
			})
		}
		if vfs, err := sc.StatVFS(dir); err == nil && vfs != nil {
			out.FreeBytes = vfs.Bavail * vfs.Bsize
			out.TotalBytes = vfs.Blocks * vfs.Bsize
		}
		return nil
	})
	if err != nil {
		return Listing{}, err
	}
	sort.Slice(out.Entries, func(i, j int) bool {
		return strings.ToLower(out.Entries[i].Name) < strings.ToLower(out.Entries[j].Name)
	})
	return out, nil
}

// Mkdir creates a directory (and any missing parents) inside an allowed root.
func (c *Client) Mkdir(ctx context.Context, p string) (string, error) {
	dir, err := c.Validate(ctx, p)
	if err != nil {
		return "", err
	}
	err = c.do(ctx, func(sc *sftp.Client) error {
		if err := sc.MkdirAll(dir); err != nil {
			return fmt.Errorf("nas: mkdir %s: %w", dir, err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return dir, nil
}

// Run executes a command on the NAS and returns its combined output.
func (c *Client) Run(ctx context.Context, cmd string) (string, error) {
	if _, err := c.connect(ctx); err != nil {
		return "", err
	}
	c.mu.Lock()
	client := c.ssh
	c.mu.Unlock()
	if client == nil {
		return "", errors.New("nas: no connection")
	}
	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("nas: new session: %w", err)
	}
	defer sess.Close()

	type result struct {
		out []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		out, err := sess.CombinedOutput(cmd)
		ch <- result{out, err}
	}()
	select {
	case <-ctx.Done():
		sess.Signal(ssh.SIGTERM) //nolint:errcheck,gosec // best effort; the session is closed next
		return "", fmt.Errorf("nas: running command: %w", ctx.Err())
	case r := <-ch:
		if r.err != nil {
			return string(r.out), fmt.Errorf("nas: running command: %w", r.err)
		}
		return string(r.out), nil
	}
}

// ProbeLftp finds lftp on the NAS and caches its absolute path in settings.
// Synology's non-interactive PATH rarely includes Entware, hence the candidates.
func (c *Client) ProbeLftp(ctx context.Context) (string, error) {
	// Executability is not enough: an Entware lftp whose libraries are missing
	// exists, is +x, and still exits 127. Only `lftp -v` actually running proves it.
	var cmd string
	for _, p := range append([]string{"lftp"}, lftpCandidates...) {
		cmd += fmt.Sprintf("p=$(command -v %s 2>/dev/null) && \"$p\" -v >/dev/null 2>&1 "+
			"&& { echo \"$p\"; exit 0; }; "+"\n", p)
	}
	cmd += "true"
	out, err := c.Run(ctx, cmd)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "/") {
			if err := c.store.SetSettings(ctx, map[string]string{"lftp_path": line}); err != nil {
				return "", fmt.Errorf("nas: caching lftp path: %w", err)
			}
			slog.Info("nas: found lftp", "path", line)
			return line, nil
		}
	}
	// The probe ran and found nothing, so a previously cached path is stale —
	// leaving it would keep feeding jobs a binary that exits 127.
	if err := c.store.SetSettings(ctx, map[string]string{"lftp_path": ""}); err != nil {
		return "", fmt.Errorf("nas: clearing stale lftp path: %w", err)
	}
	return "", fmt.Errorf("nas: no working lftp on the NAS — `lftp -v` did not run. "+
		"Install it with Entware (`opkg install lftp`) or a community package; if it "+
		"is installed, it is likely missing its shared libraries. Then restart the "+
		"relay. Looked for `lftp` on PATH and at: %s", strings.Join(lftpCandidates, ", "))
}

// volumePrefixes are the roots a Synology share can really live under. The empty
// one comes first so a NAS without an SFTP chroot resolves to itself.
var volumePrefixes = []string{
	"", "/volume1", "/volume2", "/volume3", "/volume4", "/volumeUSB1", "/volumeUSB2",
}

// ShellPath translates an SFTP-visible path into the one a shell on the NAS sees.
// Synology chroots the SFTP subsystem to the share root, so /MoonStorage2/x over
// SFTP is /volume2/MoonStorage2/x to lftp — and every shell command we generate
// (the job script, cancel, the workspace sweep) needs the latter. The answer is
// cached per share, because two shares can sit on different volumes.
//
// An unresolvable path is returned unchanged: the shell then produces its own
// error, which is more useful than one invented here.
func (c *Client) ShellPath(ctx context.Context, p string) string {
	share, _, _ := strings.Cut(strings.TrimPrefix(path.Clean(p), "/"), "/")
	if share == "" {
		return p
	}
	c.mu.Lock()
	prefix, ok := c.prefixes[share]
	c.mu.Unlock()
	if !ok {
		var cmd string
		for _, v := range volumePrefixes {
			root := v + "/" + share
			cmd += fmt.Sprintf("[ -e %s ] && { printf '%%s\n' %s; exit 0; };\n",
				shQuote(root), shQuote(v))
		}
		cmd += "true"
		out, err := c.Run(ctx, cmd)
		if err != nil {
			slog.Warn("nas: could not resolve the shell path for a share",
				"share", share, "err", err)
			return p
		}
		prefix = strings.TrimSpace(out)
		c.mu.Lock()
		c.prefixes[share] = prefix
		c.mu.Unlock()
		if prefix != "" {
			slog.Info("nas: share is chrooted for SFTP", "share", share, "shell_prefix", prefix)
		}
	}
	return prefix + p
}
