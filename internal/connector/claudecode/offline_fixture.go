package claudecode

import (
	"errors"
	"net/http"

	"github.com/blestafist/pestiroute/internal/core"
)

// NewOfflineFakeTransport marks an explicitly supplied deterministic fixture
// transport for candidate execution. It only forwards the connector's fixed
// reserved .invalid Messages request; production composition does not use it.
func NewOfflineFakeTransport(fixture core.HTTPDoer) core.HTTPDoer {
	return offlineFixtureTransport{fixture: fixture}
}

type offlineFixtureTransport struct {
	fixture core.HTTPDoer
}

func (offlineFixtureTransport) claudeCodeOfflineFakeOnly() {}

func (t offlineFixtureTransport) Do(req *http.Request) (*http.Response, error) {
	if t.fixture == nil || req == nil || req.URL == nil || req.Method != http.MethodPost || req.URL.String() != messagesURL {
		return nil, errors.New("offline Claude Code fixture rejected request")
	}
	return t.fixture.Do(req)
}
