package nas

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"sftp-relay/internal/db"
)

const (
	testPassword = "corr3ct-h0rse-battery"
	testHostKey  = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleRemoteHostKeyValue00 remote"
)

// keyPair returns an unencrypted PEM private key and, if passphrase is set, the
// same key encrypted with it.
func keyPair(t *testing.T, passphrase string) (plain, encrypted string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	plain = string(pem.EncodeToMemory(block))
	if passphrase != "" {
		enc, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
		if err != nil {
			t.Fatal(err)
		}
		encrypted = string(pem.EncodeToMemory(enc))
	}
	return plain, encrypted
}

func keyServer(t *testing.T) db.Server {
	t.Helper()
	plain, _ := keyPair(t, "")
	return db.Server{
		Name: "media", Host: "files.example", Port: 22, Username: "alice",
		AuthType: "key", PrivateKey: plain, HostKey: testHostKey,
	}
}

func fileOf(ws workspace, name string) (genFile, bool) {
	for _, f := range ws.files {
		if f.name == name {
			return f, true
		}
	}
	return genFile{}, false
}

func TestRenderFileJobWithKeyAuth(t *testing.T) {
	ws, err := render(TransferSpec{
		JobID: 7, Kind: "file", Remote: "/pub/bigfile.bin", Dest: "/volume1/media",
		Segments: 4, Parallel: 2, Server: keyServer(t),
	}, "/share/stage/job-7", "/tmp/job-7", tools{lftp: "/opt/bin/lftp"})
	if err != nil {
		t.Fatal(err)
	}

	cmds, ok := fileOf(ws, "cmds")
	if !ok {
		t.Fatal("no lftp script was generated")
	}
	script := string(cmds.body)
	for _, want := range []string{
		`pget -n 4 -c "/pub/bigfile.bin" -o "/volume1/media/bigfile.bin"`,
		"set cmd:fail-exit yes",
		"set net:max-retries 3",
		"set xfer:use-temp-file yes",
		"StrictHostKeyChecking=yes",
		"UserKnownHostsFile=/tmp/job-7/known_hosts",
		"-i /tmp/job-7/key",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("lftp script is missing %q:\n%s", want, script)
		}
	}

	key, ok := fileOf(ws, "key")
	if !ok {
		t.Fatal("no key file was generated")
	}
	if key.mode != 0o600 {
		t.Errorf("key file mode = %o, want 600", key.mode)
	}
	if cmds.mode != 0o600 {
		t.Errorf("script mode = %o, want 600", cmds.mode)
	}
	runner, ok := fileOf(ws, "run.sh")
	if !ok {
		t.Fatal("no runner script was generated")
	}
	if runner.mode != 0o700 {
		t.Errorf("runner mode = %o, want 700", runner.mode)
	}
	for _, want := range []string{
		"#!/bin/sh", "trap cleanup EXIT INT TERM HUP", `rm -rf "$d"`,
		`echo "$p" > "$d/pid"`, "'/opt/bin/lftp'",
	} {
		if !strings.Contains(string(runner.body), want) {
			t.Errorf("runner is missing %q:\n%s", want, runner.body)
		}
	}

	hosts, _ := fileOf(ws, "known_hosts")
	if got := string(hosts.body); got != "files.example "+testHostKey+"\n" {
		t.Errorf("known_hosts = %q", got)
	}
}

func TestRenderDirJobMirrors(t *testing.T) {
	ws, err := render(TransferSpec{
		JobID: 1, Kind: "dir", Remote: "/pub/season 1", Dest: "/volume1/media",
		Segments: 4, Parallel: 3, Server: keyServer(t),
	}, "/share/stage/job-1", "/tmp/job-1", tools{lftp: "/opt/bin/lftp"})
	if err != nil {
		t.Fatal(err)
	}
	cmds, _ := fileOf(ws, "cmds")
	want := `mirror --continue --parallel=3 --use-pget-n=4 "/pub/season 1" "/volume1/media/season 1"`
	if !strings.Contains(string(cmds.body), want) {
		t.Errorf("mirror command missing:\n%s", cmds.body)
	}
}

// The whole point of the workspace: nothing secret may reach the process list.
func TestGeneratedCommandCarriesNoCredentials(t *testing.T) {
	plain, _ := keyPair(t, "")
	tests := []struct {
		name   string
		server db.Server
		tools  tools
	}{
		{
			name: "key auth",
			server: db.Server{Host: "h", Port: 22, Username: "alice", AuthType: "key",
				PrivateKey: plain, HostKey: testHostKey},
			tools: tools{lftp: "/opt/bin/lftp"},
		},
		{
			name: "password auth",
			server: db.Server{Host: "h", Port: 22, Username: "alice", AuthType: "password",
				Password: testPassword, HostKey: testHostKey},
			tools: tools{lftp: "/opt/bin/lftp", sshpass: "/opt/bin/sshpass"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws, err := render(TransferSpec{
				JobID: 3, Kind: "file", Remote: "/a/b", Dest: "/volume1/media",
				Segments: 2, Parallel: 1, Server: tc.server,
			}, "/share/stage/job-3", "/tmp/job-3", tc.tools)
			if err != nil {
				t.Fatal(err)
			}
			runner, _ := fileOf(ws, "run.sh")
			for _, secret := range []string{testPassword, "PRIVATE KEY", plain} {
				if strings.Contains(ws.cmd, secret) {
					t.Errorf("command line leaks a credential: %s", ws.cmd)
				}
				if strings.Contains(string(runner.body), secret) {
					t.Errorf("runner script leaks a credential:\n%s", runner.body)
				}
			}
			if !strings.HasPrefix(ws.cmd, "/bin/sh '/share/stage/job-3/run.sh'") {
				t.Errorf("command = %q, want a plain reference to the runner", ws.cmd)
			}
		})
	}
}

func TestRenderPasswordAuth(t *testing.T) {
	spec := TransferSpec{
		JobID: 4, Kind: "file", Remote: "/a/b", Dest: "/volume1/media", Segments: 1, Parallel: 1,
		Server: db.Server{Host: "h", Port: 2222, Username: "alice", AuthType: "password",
			Password: testPassword, HostKey: testHostKey},
	}

	if _, err := render(spec, "/share/stage/job-4", "/tmp/job-4", tools{lftp: "/opt/bin/lftp"}); !errors.Is(err, ErrNoSSHPass) {
		t.Fatalf("without sshpass: err = %v, want ErrNoSSHPass", err)
	}

	ws, err := render(spec, "/share/stage/job-4", "/tmp/job-4", tools{lftp: "/opt/bin/lftp", sshpass: "/opt/bin/sshpass"})
	if err != nil {
		t.Fatal(err)
	}
	pass, ok := fileOf(ws, "pass")
	if !ok {
		t.Fatal("no password file was generated")
	}
	if pass.mode != 0o600 {
		t.Errorf("password file mode = %o, want 600", pass.mode)
	}
	if string(pass.body) != testPassword+"\n" {
		t.Errorf("password file = %q", pass.body)
	}
	cmds, _ := fileOf(ws, "cmds")
	if !strings.Contains(string(cmds.body), "/opt/bin/sshpass -f /tmp/job-4/pass ssh") {
		t.Errorf("connect-program does not use sshpass:\n%s", cmds.body)
	}
	hosts, _ := fileOf(ws, "known_hosts")
	if got := string(hosts.body); !strings.HasPrefix(got, "[h]:2222 ") {
		t.Errorf("known_hosts for a non-default port = %q", got)
	}
}

func TestRenderRejectsBadInput(t *testing.T) {
	good := keyServer(t)
	noKey := good
	noKey.HostKey = ""
	badAuth := good
	badAuth.AuthType = "kerberos"
	emptyKey := good
	emptyKey.PrivateKey = " "
	brokenKey := good
	brokenKey.PrivateKey = "not a key at all"

	tests := []struct {
		name   string
		spec   TransferSpec
		tools  tools
		wantIs error
		want   string
	}{
		{
			name:  "no lftp yet",
			spec:  TransferSpec{Kind: "file", Server: good},
			tools: tools{},
			want:  "lftp path is not known",
		},
		{
			name:   "no pinned host key",
			spec:   TransferSpec{Kind: "file", Server: noKey},
			tools:  tools{lftp: "/opt/bin/lftp"},
			wantIs: ErrNoHostKey,
		},
		{
			name:  "unknown auth type",
			spec:  TransferSpec{Kind: "file", Server: badAuth},
			tools: tools{lftp: "/opt/bin/lftp"},
			want:  "unknown auth_type",
		},
		{
			name:  "unknown kind",
			spec:  TransferSpec{Kind: "symlink", Server: good},
			tools: tools{lftp: "/opt/bin/lftp"},
			want:  "unknown job kind",
		},
		{
			name:  "key auth with no key stored",
			spec:  TransferSpec{Kind: "file", Server: emptyKey},
			tools: tools{lftp: "/opt/bin/lftp"},
			want:  "no private key stored",
		},
		{
			name:  "unparseable key",
			spec:  TransferSpec{Kind: "file", Server: brokenKey},
			tools: tools{lftp: "/opt/bin/lftp"},
			want:  "parsing private key",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := render(tc.spec, "/share/stage/job-1", "/tmp/job-1", tc.tools)
			if err == nil {
				t.Fatal("want an error, got none")
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Fatalf("err = %v, want %v", err, tc.wantIs)
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestRenderDecryptsAPassphraseProtectedKey(t *testing.T) {
	plain, encrypted := keyPair(t, "s3cret")
	s := db.Server{Host: "h", Port: 22, Username: "alice", AuthType: "key",
		PrivateKey: encrypted, Passphrase: "s3cret", HostKey: testHostKey}

	ws, err := render(TransferSpec{JobID: 5, Kind: "file", Remote: "/a", Dest: "/d", Server: s},
		"/share/stage/job-5", "/tmp/job-5", tools{lftp: "/opt/bin/lftp"})
	if err != nil {
		t.Fatal(err)
	}
	key, _ := fileOf(ws, "key")
	if _, err := ssh.ParsePrivateKey(key.body); err != nil {
		t.Fatalf("written key is not usable without a passphrase: %v", err)
	}
	if strings.Contains(string(key.body), "ENCRYPTED") {
		t.Error("the key on the NAS is still encrypted; ssh cannot be prompted there")
	}
	if plain == "" {
		t.Fatal("test fixture produced no plain key")
	}

	s.Passphrase = "wrong"
	if _, err := render(TransferSpec{JobID: 5, Kind: "file", Remote: "/a", Dest: "/d", Server: s},
		"/share/stage/job-5", "/tmp/job-5", tools{lftp: "/opt/bin/lftp"}); err == nil ||
		!strings.Contains(err.Error(), "decrypting private key") {
		t.Errorf("wrong passphrase: err = %v", err)
	}
}

func TestRenderClampsSegmentsAndParallel(t *testing.T) {
	ws, err := render(TransferSpec{
		JobID: 6, Kind: "dir", Remote: "/a", Dest: "/d", Segments: 0, Parallel: -3,
		Server: keyServer(t),
	}, "/share/stage/job-6", "/tmp/job-6", tools{lftp: "/opt/bin/lftp"})
	if err != nil {
		t.Fatal(err)
	}
	cmds, _ := fileOf(ws, "cmds")
	if !strings.Contains(string(cmds.body), "--parallel=1 --use-pget-n=1") {
		t.Errorf("zero and negative values were not clamped:\n%s", cmds.body)
	}
}

func TestQuoting(t *testing.T) {
	lftpTests := []struct{ in, want string }{
		{`plain`, `"plain"`},
		{`with space`, `"with space"`},
		{`quote"inside`, `"quote\"inside"`},
		{`back\slash`, `"back\\slash"`},
		{``, `""`},
		{`ünïcode`, `"ünïcode"`},
	}
	for _, tc := range lftpTests {
		if got := lftpQuote(tc.in); got != tc.want {
			t.Errorf("lftpQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	shTests := []struct{ in, want string }{
		{`/tmp/job-1`, `'/tmp/job-1'`},
		{`it's here`, `'it'\''s here'`},
		{``, `''`},
	}
	for _, tc := range shTests {
		if got := shQuote(tc.in); got != tc.want {
			t.Errorf("shQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestWorkspaceDir(t *testing.T) {
	tests := []struct {
		tmp  string
		id   int64
		want string
	}{
		{"/volume1/tmp", 12, "/volume1/tmp/job-12"},
		{"", 1, "/tmp/job-1"},
		{"  ", 1, "/tmp/job-1"},
		{"/tmp/", 3, "/tmp/job-3"},
	}
	for _, tc := range tests {
		if got := WorkspaceDir(tc.tmp, tc.id); got != tc.want {
			t.Errorf("WorkspaceDir(%q, %d) = %q, want %q", tc.tmp, tc.id, got, tc.want)
		}
	}
}

// --- against the fake NAS -------------------------------------------------

func TestTransferWritesTheWorkspaceAndStreamsOutput(t *testing.T) {
	c, store, fake := setup(t)
	ctx := context.Background()
	if err := store.SetSettings(ctx, map[string]string{
		"lftp_path": "/opt/bin/lftp", "nas_tmp": "/volume1/media",
	}); err != nil {
		t.Fatal(err)
	}
	fake.execOutput = "`f' at 512 (50%) 1.0K/s eta:1s\r512 bytes transferred in 1 seconds\n"

	var lines []string
	code, err := c.Transfer(ctx, TransferSpec{
		JobID: 9, Kind: "file", Remote: "/pub/f", Dest: "/volume1/media",
		Segments: 2, Parallel: 1, Server: keyServer(t),
	}, func(line string) { lines = append(lines, line) })
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if len(lines) != 2 {
		t.Fatalf("streamed lines = %q, want 2", lines)
	}
	if !strings.Contains(lines[0], "(50%)") {
		t.Errorf("first line = %q", lines[0])
	}

	// The workspace really landed on the NAS, and every credential-bearing file
	// was chmodded before anything was written into it.
	if err := c.do(ctx, func(sc *sftp.Client) error {
		for _, name := range []string{"cmds", "key", "known_hosts", "run.sh"} {
			if _, err := sc.Stat("/volume1/media/job-9/" + name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{
		"":             0o700,
		"/cmds":        0o600,
		"/key":         0o600,
		"/known_hosts": 0o600,
		"/run.sh":      0o700,
	} {
		if got := fake.chmod.mode("/volume1/media/job-9" + name); got != want {
			t.Errorf("chmod of job-9%s = %o, want %o", name, got, want)
		}
	}
}

func TestTransferReportsANonZeroExitCode(t *testing.T) {
	c, store, fake := setup(t)
	ctx := context.Background()
	if err := store.SetSettings(ctx, map[string]string{
		"lftp_path": "/opt/bin/lftp", "nas_tmp": "/volume1/media",
	}); err != nil {
		t.Fatal(err)
	}
	fake.execOutput = "cd: Access failed: No such file\n"
	fake.execStatus = 1

	code, err := c.Transfer(ctx, TransferSpec{
		JobID: 10, Kind: "file", Remote: "/pub/f", Dest: "/volume1/media", Server: keyServer(t),
	}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestTransferSurfacesRenderErrors(t *testing.T) {
	c, store, _ := setup(t)
	ctx := context.Background()
	if err := store.SetSettings(ctx, map[string]string{"lftp_path": "/opt/bin/lftp"}); err != nil {
		t.Fatal(err)
	}
	s := keyServer(t)
	s.HostKey = ""
	if _, err := c.Transfer(ctx, TransferSpec{
		JobID: 11, Kind: "file", Remote: "/a", Dest: "/volume1/media", Server: s,
	}, func(string) {}); !errors.Is(err, ErrNoHostKey) {
		t.Errorf("err = %v, want ErrNoHostKey", err)
	}
}

func TestTransferHonoursACancelledContext(t *testing.T) {
	c, store, _ := setup(t)
	if err := store.SetSettings(context.Background(), map[string]string{
		"lftp_path": "/opt/bin/lftp", "nas_tmp": "/volume1/media",
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Transfer(ctx, TransferSpec{
		JobID: 12, Kind: "file", Remote: "/a", Dest: "/volume1/media", Server: keyServer(t),
	}, func(string) {}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestCancelAndSweepIssueRemoteCommands(t *testing.T) {
	c, store, _ := setup(t)
	ctx := context.Background()
	if err := store.SetSettings(ctx, map[string]string{
		"lftp_path": "/opt/bin/lftp", "nas_tmp": "/volume1/tmp",
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Cancel(ctx, 42); err != nil {
		t.Errorf("Cancel: %v", err)
	}
	if err := c.Sweep(ctx); err != nil {
		t.Errorf("Sweep: %v", err)
	}
}

func TestSizeMeasuresTheDestination(t *testing.T) {
	c, _, _ := setup(t)
	ctx := context.Background()

	// A path that does not exist yet means nothing has transferred, not an error.
	if n, err := c.Size(ctx, "/volume1/media/absent"); err != nil || n != 0 {
		t.Errorf("missing path: n = %d, err = %v", n, err)
	}

	if err := c.do(ctx, func(sc *sftp.Client) error {
		f, err := sc.Create("/volume1/media/films/part.bin")
		if err != nil {
			return err
		}
		if _, err := f.Write(make([]byte, 2048)); err != nil {
			return err
		}
		return f.Close()
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := c.Size(ctx, "/volume1/media/films/part.bin"); err != nil || n != 2048 {
		t.Errorf("file size = %d, err = %v, want 2048", n, err)
	}
	if n, err := c.Size(ctx, "/volume1/media/films"); err != nil || n != 2048 {
		t.Errorf("directory size = %d, err = %v, want 2048", n, err)
	}
}

func TestProbeSSHPass(t *testing.T) {
	c, store, fake := setup(t)
	ctx := context.Background()

	fake.execOutput = "/opt/bin/sshpass\n"
	got, err := c.ProbeSSHPass(ctx)
	if err != nil || got != "/opt/bin/sshpass" {
		t.Fatalf("ProbeSSHPass = %q, %v", got, err)
	}
	s, err := store.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s["sshpass_path"] != "/opt/bin/sshpass" {
		t.Errorf("sshpass_path = %q", s["sshpass_path"])
	}

	fake.execOutput = "\n"
	if got, err := c.ProbeSSHPass(ctx); err != nil || got != "" {
		t.Errorf("missing sshpass: got %q, %v", got, err)
	}
}

func TestToolsProbesLftpWhenItIsNotCachedYet(t *testing.T) {
	c, _, fake := setup(t)
	fake.execOutput = "/usr/local/bin/lftp\n"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, tmp, err := c.tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.lftp != "/usr/local/bin/lftp" {
		t.Errorf("lftp = %q", got.lftp)
	}
	if tmp != "/tmp" {
		t.Errorf("nas_tmp = %q, want the seeded default", tmp)
	}
}

func TestNormalizePEM(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"missing final newline", "-----BEGIN-----\nabc\n-----END-----",
			"-----BEGIN-----\nabc\n-----END-----\n"},
		{"crlf", "-----BEGIN-----\r\nabc\r\n-----END-----\r\n",
			"-----BEGIN-----\nabc\n-----END-----\n"},
		{"bare cr", "-----BEGIN-----\rabc\r-----END-----\r",
			"-----BEGIN-----\nabc\n-----END-----\n"},
		{"already clean", "-----BEGIN-----\nabc\n-----END-----\n",
			"-----BEGIN-----\nabc\n-----END-----\n"},
		{"trailing blank lines", "-----BEGIN-----\nabc\n-----END-----\n\n\n",
			"-----BEGIN-----\nabc\n-----END-----\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(normalizePEM([]byte(tc.in))); got != tc.want {
				t.Errorf("normalizePEM = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSizeCountsAnInFlightTempFile(t *testing.T) {
	c, _, _ := setup(t)
	ctx := context.Background()
	if err := c.do(ctx, func(sc *sftp.Client) error {
		f, err := sc.Create("/volume1/media/films/.in.part.bin")
		if err != nil {
			return err
		}
		if _, err := f.Write(make([]byte, 512)); err != nil {
			return err
		}
		return f.Close()
	}); err != nil {
		t.Fatal(err)
	}
	// lftp has not renamed it yet, so the final name does not exist.
	if n, err := c.Size(ctx, "/volume1/media/films/part.bin"); err != nil || n != 512 {
		t.Errorf("in-flight size = %d, err = %v, want 512", n, err)
	}
}
