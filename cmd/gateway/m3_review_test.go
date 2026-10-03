package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

// These checks exercise composition seams that individual repository tests miss.
func TestReviewedRequestIsTerminalBeforeRestart(t *testing.T) {
	fixture := newDurableAccountingFixture(t, nil)
	response, gatewayErr := fixture.dispatcher.Execute(fixture.ctx, fixture.request)
	if gatewayErr != nil {
		t.Fatal(gatewayErr)
	}
	defer response.Stream.Close()
	stream := fixture.connector.(*accountingRaceConnector).stream
	go func() {
		stream.events <- accountingRaceEvent{frame: core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeSucceeded}}}
		stream.events <- accountingRaceEvent{err: io.EOF}
	}()
	for {
		_, err := response.Stream.Next(fixture.ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := sqlite.NewLedger(fixture.db).QueryRequests(context.Background(), sqlite.RequestFilter{})
	if err != nil || len(rows) != 1 || rows[0].Request.State != "succeeded" || rows[0].Request.FinishedAt == nil {
		t.Fatalf("request remains nonterminal after completion: %+v, %v", rows, err)
	}
}

type reviewedFailingAccounting struct{ sqliteAccountingStore }

func (reviewedFailingAccounting) Admit(context.Context, core.AccountingAdmission) error {
	return core.AccountingStorageFailure{Err: errors.New("controlled disk failure")}
}

func TestReviewedAccountingFailureWithdrawsReadiness(t *testing.T) {
	_, dbPath, keyPath := protectedFixture(t)
	c, err := prepareProtectedConfig(context.Background(), fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0"))
	if err != nil {
		t.Fatal(err)
	}
	issued, err := sqlite.NewVirtualKeys(c.runtimeDB).Create(context.Background(), sqlite.CreateVirtualKeyParams{PolicyID: "policy-id-a", PolicyRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	c.accounting = reviewedFailingAccounting{sqliteAccountingStore{ledger: sqlite.NewLedger(c.runtimeDB)}}
	var ready, draining atomic.Bool
	ready.Store(true)
	handler, closeComponents, err := composeHandler(c, &ready, &draining, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeComponents(context.Background())
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"model-a","input":"x"}`))
	req.Header.Set("Authorization", "Bearer "+issued.Secret)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("failure status %d: %s", w.Code, w.Body.String())
	}
	probe := httptest.NewRecorder()
	handler.ServeHTTP(probe, httptest.NewRequest("GET", "/readyz", nil))
	if probe.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz stays ready after storage failure: %d", probe.Code)
	}
}

func TestReviewedProtectedStartupRecoversAuthSessions(t *testing.T) {
	_, dbPath, keyPath := protectedFixture(t)
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	sessions := sqlite.NewAuthSessions(db)
	now := time.Now()
	if err := sessions.CreateRefreshMarker(context.Background(), "stale-refresh", "account-a", "upstream", 1, now); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	c, err := prepareProtectedConfig(context.Background(), fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.runtimeDB.Close()
	defer c.processLock.Close()
	var lifecycle, reason string
	if err := c.runtimeDB.QueryRow(`SELECT lifecycle,COALESCE(quarantine_reason,'') FROM auth_sessions WHERE id='stale-refresh'`).Scan(&lifecycle, &reason); err != nil {
		t.Fatal(err)
	}
	if lifecycle != "uncertain" || reason != "restart_in_progress" {
		t.Fatalf("restart left refresh replayable/in-progress: %s/%s", lifecycle, reason)
	}
}
