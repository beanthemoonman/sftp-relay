package nas

import (
	"bufio"
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"sftp-relay/internal/db"
)

// ErrNoHostKey means the remote server has never been connected to, so there is
// no pinned key to hand ssh on the NAS. Browsing or testing the server fixes it.
var ErrNoHostKey = errors.New("nas: the remote server has no recorded host key — " +
	"test or browse the server once so its key is pinned, then retry")

// ErrNoSSHPass is the remediation for password-auth servers on a NAS without
// sshpass. lftp shells out to ssh for sftp://, and ssh will not read a password
// from anywhere we can reach non-interactively.
var ErrNoSSHPass = errors.New("nas: this server uses password authentication, which " +
	"needs sshpass on the NAS (`opkg install sshpass`). Switch the server to key " +
	"authentication to avoid the dependency")

// TransferSpec is everything a single lftp invocation needs.
type TransferSpec struct {
	JobID    int64
	Kind     string // file | dir
	Remote   string // path on the remote server
	Dest     string // already-validated destination directory on the NAS
	Segments int    // pget segments per file
	Parallel int    // parallel files for a mirror
	Server   db.Server
}

// genFile is one file written into the job's workspace on the NAS.
type genFile struct {
	name string
	mode fs.FileMode
	body []byte
}

// tools are the absolute paths resolved by the startup probes.
type tools struct{ lftp, sshpass string }

// workspace is the rendered, credential-bearing content of a job's temp dir
// plus the credential-free command that runs it.
type workspace struct {
	files []genFile
	cmd   string
}

// render builds the whole workspace as pure data, so the escaping, the script
// shape and the "no credentials on the command line" rule are all unit-testable
// without a NAS in the loop.
func render(spec TransferSpec, stage, dir string, t tools) (workspace, error) {
	if t.lftp == "" {
		return workspace{}, errors.New("nas: lftp path is not known yet")
	}
	if strings.TrimSpace(spec.Server.HostKey) == "" {
		return workspace{}, ErrNoHostKey
	}
	port := spec.Server.Port
	if port == 0 {
		port = 22
	}
	segments, parallel := spec.Segments, spec.Parallel
	if segments < 1 {
		segments = 1
	}
	if parallel < 1 {
		parallel = 1
	}

	files := []genFile{{
		name: "known_hosts",
		mode: 0o600,
		body: []byte(knownHostsLine(spec.Server.Host, port, spec.Server.HostKey)),
	}}

	sshOpts := []string{"-a", "-x",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + path.Join(dir, "known_hosts"),
		"-o", "BatchMode=yes",
		"-p", strconv.Itoa(port),
	}
	connect := "ssh"

	switch spec.Server.AuthType {
	case "key":
		key, err := decryptKey(spec.Server.PrivateKey, spec.Server.Passphrase)
		if err != nil {
			return workspace{}, err
		}
		files = append(files, genFile{name: "key", mode: 0o600, body: key})
		sshOpts = append(sshOpts, "-i", path.Join(dir, "key"), "-o", "IdentitiesOnly=yes")
	case "password":
		if t.sshpass == "" {
			return workspace{}, ErrNoSSHPass
		}
		files = append(files, genFile{
			name: "pass", mode: 0o600, body: []byte(spec.Server.Password + "\n"),
		})
		connect = t.sshpass + " -f " + path.Join(dir, "pass") + " ssh"
	default:
		return workspace{}, fmt.Errorf("nas: server %q has unknown auth_type %q",
			spec.Server.Name, spec.Server.AuthType)
	}

	local := path.Join(spec.Dest, path.Base(spec.Remote))
	var transfer string
	switch spec.Kind {
	case "file":
		transfer = fmt.Sprintf("pget -n %d -c %s -o %s",
			segments, lftpQuote(spec.Remote), lftpQuote(local))
	case "dir":
		transfer = fmt.Sprintf("mirror --continue --parallel=%d --use-pget-n=%d %s %s",
			parallel, segments, lftpQuote(spec.Remote), lftpQuote(local))
	default:
		return workspace{}, fmt.Errorf("nas: unknown job kind %q", spec.Kind)
	}

	var cmds bytes.Buffer
	cmds.WriteString("set cmd:fail-exit yes\n")
	cmds.WriteString("set net:max-retries 3\n")
	cmds.WriteString("set net:timeout 30\n")
	cmds.WriteString("set xfer:use-temp-file yes\n")
	cmds.WriteString("set xfer:clobber yes\n")
	cmds.WriteString("set sftp:auto-confirm no\n")
	_, err := fmt.Fprintf(&cmds, "set sftp:connect-program %s\n",
		lftpQuote(connect+" "+strings.Join(sshOpts, " ")))
	if err != nil {
		return workspace{}, err
	}
	_, err = fmt.Fprintf(&cmds, "open -u %s sftp://%s\n",
		lftpQuote(spec.Server.Username+","+spec.Server.Password), lftpQuote(spec.Server.Host))
	if err != nil {
		return workspace{}, err
	}
	_, err = fmt.Fprintf(&cmds, "%s\n", transfer)
	if err != nil {
		return workspace{}, err
	}
	cmds.WriteString("bye\n")
	files = append(files, genFile{name: "cmds", mode: 0o600, body: cmds.Bytes()})

	// POSIX sh — Synology's default shell is ash. lftp runs in the background so
	// its own pid lands in the pidfile and cancel can TERM it directly; the EXIT
	// trap then removes both directories on every ordinary exit path.
	//
	// Two directories, because the staging one is reachable over SFTP but sits in
	// a Synology share, whose ACLs quietly override chmod and leave every file
	// world-readable — and ssh ignores a private key it can read like that. The
	// credentials are copied into a real 0700 dir outside the share first.
	run := "#!/bin/sh\n" +
		"s=" + shQuote(stage) + "\n" +
		"d=" + shQuote(dir) + "\n" +
		"cleanup() { rm -rf \"$d\" \"$s\"; }\n" +
		"trap cleanup EXIT INT TERM HUP\n" +
		"exec 2>&1\n" +
		"rm -rf \"$d\" && mkdir -p \"$d\" && chmod 700 \"$d\" || exit 1\n" +
		"cp \"$s\"/cmds \"$s\"/known_hosts \"$d\"/ || exit 1\n" +
		"for f in key pass; do [ -f \"$s/$f\" ] && cp \"$s/$f\" \"$d/$f\"; done\n" +
		"chmod 600 \"$d\"/* || exit 1\n" +
		shQuote(t.lftp) + " -f \"$d/cmds\" &\n" +
		"p=$!\n" +
		"echo \"$p\" > \"$d/pid\"\n" +
		"wait \"$p\"\n" +
		"exit $?\n"
	files = append(files, genFile{name: "run.sh", mode: 0o700, body: []byte(run)})

	return workspace{
		files: files,
		cmd:   "/bin/sh " + shQuote(path.Join(stage, "run.sh")),
	}, nil
}

// knownHostsLine renders the pinned key in the format ssh expects. A non-default
// port is bracketed, exactly as ssh writes it itself.
func knownHostsLine(host string, port int, key string) string {
	pattern := host
	if port != 22 {
		pattern = fmt.Sprintf("[%s]:%d", host, port)
	}
	return pattern + " " + strings.TrimSpace(key) + "\n"
}

// decryptKey returns an unencrypted PEM key. The passphrase stays on the Pi:
// ssh on the NAS has no way to be prompted, so the key is decrypted here and
// written into the 0600 workspace instead.
func decryptKey(privateKey, passphrase string) ([]byte, error) {
	if strings.TrimSpace(privateKey) == "" {
		return nil, errors.New("nas: server has key authentication but no private key stored")
	}
	if passphrase == "" {
		if _, err := ssh.ParsePrivateKey([]byte(privateKey)); err != nil {
			return nil, fmt.Errorf("nas: parsing private key: %w", err)
		}
		return normalizePEM([]byte(privateKey)), nil
	}
	raw, err := ssh.ParseRawPrivateKeyWithPassphrase([]byte(privateKey), []byte(passphrase))
	if err != nil {
		// x/crypto never puts key material in its error text.
		return nil, fmt.Errorf("nas: decrypting private key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(raw, "")
	if err != nil {
		return nil, fmt.Errorf("nas: re-encoding private key: %w", err)
	}
	return normalizePEM(pem.EncodeToMemory(block)), nil
}

// normalizePEM makes a stored key acceptable to OpenSSH on the NAS. Go's parser
// happily accepts CRLF line endings and a missing final newline — a key pasted
// into the browser form usually has both — while ssh rejects the file outright
// with "invalid format" and falls back to no authentication at all.
func normalizePEM(b []byte) []byte {
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	b = bytes.ReplaceAll(b, []byte("\r"), []byte("\n"))
	return append(bytes.TrimRight(b, "\n"), '\n')
}

// lftpQuote wraps a value for lftp's tokeniser, which understands backslash
// escapes inside double quotes.
func lftpQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		if r == '\\' || r == '"' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// shQuote wraps a value in single quotes for POSIX sh.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// WorkspaceDir is the staging directory a job's files are uploaded to over SFTP.
// It has to sit inside a share, because that is all the SFTP chroot can reach.
func WorkspaceDir(tmp string, jobID int64) string {
	if strings.TrimSpace(tmp) == "" {
		tmp = "/tmp"
	}
	return path.Join(tmp, fmt.Sprintf("job-%d", jobID))
}

// RuntimeDir is where the job actually runs from: a plain shell path outside any
// share, so 0700 means 0700 and ssh will accept the key it holds.
func RuntimeDir(jobID int64) string {
	return fmt.Sprintf("/tmp/sftp-relay-job-%d", jobID)
}

func (c *Client) tools(ctx context.Context) (tools, string, error) {
	s, err := c.store.Settings(ctx)
	if err != nil {
		return tools{}, "", fmt.Errorf("nas: loading settings: %w", err)
	}
	t := tools{
		lftp:    strings.TrimSpace(s["lftp_path"]),
		sshpass: strings.TrimSpace(s["sshpass_path"]),
	}
	if t.lftp == "" {
		if t.lftp, err = c.ProbeLftp(ctx); err != nil {
			return tools{}, "", err
		}
	}
	return t, strings.TrimSpace(s["nas_tmp"]), nil
}

// Transfer writes the job workspace onto the NAS, runs lftp there and streams
// every output line to onLine. It returns lftp's exit code.
func (c *Client) Transfer(ctx context.Context, spec TransferSpec, onLine func(string)) (int, error) {
	t, tmp, err := c.tools(ctx)
	if err != nil {
		return -1, err
	}
	dir := WorkspaceDir(tmp, spec.JobID)
	// The script and the paths inside it are read by a shell on the NAS, which
	// does not see the SFTP chroot we wrote them through.
	spec.Dest = c.ShellPath(ctx, spec.Dest)
	ws, err := render(spec, c.ShellPath(ctx, dir), RuntimeDir(spec.JobID), t)
	if err != nil {
		return -1, err
	}
	if err := c.writeWorkspace(ctx, dir, ws.files); err != nil {
		return -1, err
	}
	return c.stream(ctx, ws.cmd, onLine)
}

// writeWorkspace writes the rendered files over SFTP. dir is the SFTP-visible
// path, which is not necessarily the one the script itself refers to.
func (c *Client) writeWorkspace(ctx context.Context, dir string, files []genFile) error {
	return c.do(ctx, func(sc *sftp.Client) error {
		if err := sc.MkdirAll(dir); err != nil {
			return fmt.Errorf("nas: creating workspace %s: %w", dir, err)
		}
		if err := sc.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("nas: securing workspace %s: %w", dir, err)
		}
		for _, f := range files {
			p := path.Join(dir, f.name)
			fh, err := sc.Create(p)
			if err != nil {
				return fmt.Errorf("nas: creating %s: %w", p, err)
			}
			// Mode before content: the file must never be readable while it holds
			// a credential, not even for the width of a write.
			if err := sc.Chmod(p, f.mode); err != nil {
				err := fh.Close()
				if err != nil {
					slog.Error("failed to close file handle", "err", err)
					return err
				} //nolint:errcheck // the error below is the one that matters
				return fmt.Errorf("nas: securing %s: %w", p, err)
			}
			if _, err := fh.Write(f.body); err != nil {
				err := fh.Close()
				if err != nil {
					slog.Error("failed to close file handle", "err", err)
					return err
				} //nolint:errcheck // ditto
				return fmt.Errorf("nas: writing %s: %w", p, err)
			}
			if err := fh.Close(); err != nil {
				return fmt.Errorf("nas: closing %s: %w", p, err)
			}
		}
		return nil
	})
}

// stream runs cmd on the NAS and feeds each output line to onLine. lftp writes
// progress with carriage returns, so \r ends a line here just as \n does.
func (c *Client) stream(ctx context.Context, cmd string, onLine func(string)) (int, error) {
	if _, err := c.connect(ctx); err != nil {
		return -1, err
	}
	c.mu.Lock()
	client := c.ssh
	c.mu.Unlock()
	if client == nil {
		return -1, errors.New("nas: no connection")
	}
	sess, err := client.NewSession()
	if err != nil {
		return -1, fmt.Errorf("nas: new session: %w", err)
	}
	defer func(sess *ssh.Session) {
		err := sess.Close()
		if err != nil {
			slog.Error("failed to close session", "err", err)
		}
	}(sess) //nolint:errcheck // the session is finished either way

	out, err := sess.StdoutPipe()
	if err != nil {
		return -1, fmt.Errorf("nas: stdout pipe: %w", err)
	}
	// run.sh folds its own stderr into stdout, but anything that fails before it
	// starts — a missing script, a shell that cannot open it — only ever speaks on
	// stderr. Discarding that hid a "No such file or directory" behind a bare 127.
	errPipe, err := sess.StderrPipe()
	if err != nil {
		return -1, fmt.Errorf("nas: stderr pipe: %w", err)
	}
	if err := sess.Start(cmd); err != nil {
		return -1, fmt.Errorf("nas: starting remote command: %w", err)
	}

	scan := func(r io.Reader, done chan<- struct{}) {
		defer close(done)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 8*1024), 64*1024)
		sc.Split(scanLines)
		for sc.Scan() {
			if line := strings.TrimRight(sc.Text(), " \t"); line != "" {
				onLine(line)
			}
		}
	}
	scanned, scannedErr := make(chan struct{}), make(chan struct{})
	go scan(out, scanned)
	go scan(errPipe, scannedErr)

	waited := make(chan error, 1)
	go func() { waited <- sess.Wait() }()

	select {
	case <-ctx.Done():
		err := sess.Signal(ssh.SIGTERM)
		if err != nil {
			return 0, err
		} //nolint:errcheck // best effort; the session closes next
		return -1, fmt.Errorf("nas: transfer: %w", ctx.Err())
	case err := <-waited:
		<-scanned
		<-scannedErr
		var exit *ssh.ExitError
		if errors.As(err, &exit) {
			return exit.ExitStatus(), nil
		}
		if err != nil {
			return -1, fmt.Errorf("nas: transfer: %w", err)
		}
		return 0, nil
	}
}

// scanLines splits on \n or \r, so lftp's in-place progress updates each arrive
// as their own line rather than accumulating into one enormous token.
func scanLines(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// Cancel sends TERM to the lftp recorded in the job's pidfile. Closing the SSH
// session does not reliably kill it, which is the whole reason for the pidfile.
func (c *Client) Cancel(ctx context.Context, jobID int64) error {
	pidFile := path.Join(RuntimeDir(jobID), "pid")
	cmd := "p=$(cat " + shQuote(pidFile) + " 2>/dev/null); " +
		"[ -n \"$p\" ] && kill -TERM \"$p\" 2>/dev/null; true"
	if _, err := c.Run(ctx, cmd); err != nil {
		return fmt.Errorf("nas: cancelling job %d: %w", jobID, err)
	}
	return nil
}

// Sweep removes leftover job workspaces. A SIGKILL outruns the EXIT trap, so
// this runs at startup, when nothing of ours is legitimately running.
func (c *Client) Sweep(ctx context.Context) error {
	_, tmp, err := c.tools(ctx)
	if err != nil {
		return err
	}
	if strings.TrimSpace(tmp) == "" {
		tmp = "/tmp"
	}
	tmp = c.ShellPath(ctx, tmp)
	if _, err := c.Run(ctx, "rm -rf "+shQuote(tmp)+"/job-* /tmp/sftp-relay-job-*; true"); err != nil {
		return fmt.Errorf("nas: sweeping stale workspaces: %w", err)
	}
	slog.Info("nas: swept stale job workspaces", "tmp", tmp)
	return nil
}

// Size reports the bytes present at p on the NAS, walking a directory if need
// be. It is the fallback when lftp's output yields no parseable progress.
func (c *Client) Size(ctx context.Context, p string) (int64, error) {
	var total int64
	err := c.do(ctx, func(sc *sftp.Client) error {
		total = 0
		fi, err := sc.Stat(p)
		if errors.Is(err, os.ErrNotExist) {
			// xfer:use-temp-file means an in-flight file is still called
			// ".in.<name>"; without this the progress bar sits at zero until
			// lftp renames it at the very end.
			fi, err = sc.Stat(path.Join(path.Dir(p), ".in."+path.Base(p)))
		}
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil // nothing transferred yet is not an error
			}
			return fmt.Errorf("nas: stat %s: %w", p, err)
		}
		if !fi.IsDir() {
			total = fi.Size()
			return nil
		}
		w := sc.Walk(p)
		for w.Step() {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("nas: walk %s: %w", p, err)
			}
			if w.Err() != nil {
				continue // a file vanishing mid-walk is normal during a transfer
			}
			if st := w.Stat(); st != nil && !st.IsDir() {
				total += st.Size()
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// ProbeSSHPass caches sshpass's absolute path, needed only for password-auth
// remote servers. Its absence is not an error until such a job actually runs.
func (c *Client) ProbeSSHPass(ctx context.Context) (string, error) {
	out, err := c.Run(ctx, "for s in sshpass /opt/bin/sshpass; do "+
		"p=$(command -v \"$s\" 2>/dev/null) && \"$p\" -V >/dev/null 2>&1 "+
		"&& { echo \"$p\"; exit 0; }; done; true")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "/") {
			if err := c.store.SetSettings(ctx, map[string]string{"sshpass_path": line}); err != nil {
				return "", fmt.Errorf("nas: caching sshpass path: %w", err)
			}
			return line, nil
		}
	}
	if err := c.store.SetSettings(ctx, map[string]string{"sshpass_path": ""}); err != nil {
		return "", fmt.Errorf("nas: clearing stale sshpass path: %w", err)
	}
	return "", nil
}
