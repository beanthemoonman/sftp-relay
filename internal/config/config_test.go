package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func writeEnv(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		preset  map[string]string
		want    Config
		wantErr bool
	}{
		{
			name: "defaults when file missing",
			file: filepath.Join(t.TempDir(), "absent"),
			want: Config{Port: "8080", DBPath: "/data/app.db", LogLevel: slog.LevelInfo},
		},
		{
			name: "values from file, comments and quotes handled",
			file: writeEnv(t, "# comment\n\nexport PORT=9999\nDB_PATH=\"/tmp/x.db\"\nAUTH_USER='bob'\nAUTH_PASS=hunter2\nNAS_SSH_KEY_PATH=/keys/id\nLOG_LEVEL=debug\nnot a pair\n"),
			want: Config{Port: "9999", DBPath: "/tmp/x.db", AuthUser: "bob", AuthPass: "hunter2",
				NASSSHKeyPath: "/keys/id", LogLevel: slog.LevelDebug},
		},
		{
			name:   "real environment wins over the file",
			file:   writeEnv(t, "PORT=1111\n"),
			preset: map[string]string{"PORT": "2222"},
			want:   Config{Port: "2222", DBPath: "/data/app.db", LogLevel: slog.LevelInfo},
		},
		{
			name:    "bad log level rejected",
			file:    writeEnv(t, "LOG_LEVEL=shouting\n"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, k := range []string{"PORT", "DB_PATH", "AUTH_USER", "AUTH_PASS", "NAS_SSH_KEY_PATH", "LOG_LEVEL"} {
				t.Setenv(k, "")
				os.Unsetenv(k)
			}
			for k, v := range tt.preset {
				t.Setenv(k, v)
			}
			got, err := Load(tt.file)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}
