package e2e

import (
	"os"
	"testing"

	"github.com/piperbox/piper/internal/relay/relaytest"
)

// The relay binaries these tests spawn need a Postgres; relaytest provides
// one per process (RUN_E2E already implies Docker is present).
func TestMain(m *testing.M) { os.Exit(relaytest.Main(m)) }
