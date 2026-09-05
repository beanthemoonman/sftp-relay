package sftpclient

import (
	"testing"

	"go.uber.org/goleak"
)

// Every SSH/SFTP connection here owns goroutines; a leaked one is a leaked
// socket on the NAS or a remote server.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
