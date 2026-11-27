package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestBallotBurst(t *testing.T) {
	for _, voters := range []int{800, 1000} {
		t.Run(strconv.Itoa(voters), func(t *testing.T) {
			testBallotBurst(t, voters)
		})
	}
}

func testBallotBurst(t *testing.T, voters int) {
	db := ballotTestDB(t)
	db.SetMaxOpenConns(8)
	v := votingEvent{db: db, slug: "demo"}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	if err := v.transition(ctx, "preparing", "open"); err != nil {
		t.Fatal(err)
	}
	var choices ballotChoices
	err := db.QueryRowContext(ctx, `SELECT min(participant_id) FILTER (WHERE nomination='dancer'),
		min(participant_id) FILTER (WHERE nomination='vocalist'),
		min(participant_id) FILTER (WHERE nomination='actor') FROM candidacies`).Scan(&choices.DancerID, &choices.VocalistID, &choices.ActorID)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(choices)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/voting/status", v.status)
	mux.HandleFunc("POST /api/voting/ballot", v.submitBallot)
	server := httptest.NewUnstartedServer(mux)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	server.Client().Transport.(*http.Transport).MaxConnsPerHost = 1
	cookies := make([]*http.Cookie, voters)
	for i := range cookies {
		w := httptest.NewRecorder()
		v.status(w, httptest.NewRequestWithContext(ctx, "GET", "/api/voting/status", nil))
		if w.Code != 200 || len(w.Result().Cookies()) != 1 {
			t.Fatal("could not initialize voter")
		}
		cookies[i] = w.Result().Cookies()[0]
	}
	post := func(cookie *http.Cookie) error {
		req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/api/voting/ballot", strings.NewReader(string(body)))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		res, err := server.Client().Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		var reply struct{ Accepted bool }
		if err := json.NewDecoder(res.Body).Decode(&reply); res.StatusCode != 200 || err != nil || !reply.Accepted {
			return fmt.Errorf("response HTTP %d, accepted=%t, decode=%v", res.StatusCode, reply.Accepted, err)
		}
		return nil
	}
	start := make(chan struct{})
	warm, err := server.Client().Get(server.URL + "/api/voting/status")
	if err != nil {
		t.Fatal(err)
	}
	warm.Body.Close()
	if warm.ProtoMajor != 2 {
		t.Fatal("expected HTTP/2")
	}
	var ready, done sync.WaitGroup
	ready.Add(voters)
	done.Add(voters)
	for _, cookie := range cookies {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			if err := post(cookie); err != nil {
				t.Error(err)
			}
		}()
	}
	ready.Wait()
	began := time.Now()
	close(start)
	done.Wait()
	t.Logf("%d simultaneous HTTP submissions completed in %s", voters, time.Since(began))
	for _, cookie := range cookies[:80] {
		if err := post(cookie); err != nil {
			t.Fatal(err)
		}
	}
	var total, distinct, matched int
	err = db.QueryRowContext(ctx, `SELECT count(*),count(DISTINCT browser_id),
		count(*) FILTER (WHERE dancer_id=$1 AND vocalist_id=$2 AND actor_id=$3) FROM ballots`,
		choices.DancerID, choices.VocalistID, choices.ActorID).Scan(&total, &distinct, &matched)
	if err != nil || total != voters || distinct != voters || matched != voters {
		t.Fatalf("stored total=%d unique=%d matching=%d, error=%v", total, distinct, matched, err)
	}
	live, err := v.calculateResults(ctx)
	if err != nil || live.Ballots != int64(voters) || len(live.RecentBallots) == 0 {
		t.Fatalf("live results do not show the burst: %v", err)
	}
	if err := v.transition(ctx, "open", "closed"); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(ctx, "GET", "/", nil)
	journal := httptest.NewRecorder()
	v.ballotJournal(journal, request)
	rows, err := csv.NewReader(journal.Body).ReadAll()
	if err != nil || journal.Code != 200 || len(rows) != voters+1 {
		t.Fatalf("journal HTTP %d rows %d: %v", journal.Code, len(rows), err)
	}
	seconds := map[int64]int{}
	for _, row := range rows[1:] {
		at, err := time.Parse(time.RFC3339Nano, row[1])
		if err != nil {
			t.Fatal(err)
		}
		seconds[at.Unix()]++
	}
	timeline := httptest.NewRecorder()
	v.ballotTimeline(timeline, request)
	match := regexp.MustCompile(`data-chart="([^"]+)"`).FindStringSubmatch(timeline.Body.String())
	if timeline.Code != 200 || len(match) != 2 {
		t.Fatalf("timeline HTTP %d or missing chart data", timeline.Code)
	}
	var chart struct {
		Results resultsView
		Counts  []map[string]map[int64]int
	}
	if err := json.Unmarshal([]byte(html.UnescapeString(match[1])), &chart); err != nil {
		t.Fatal(err)
	}
	if !chart.Results.Verified || chart.Results.Ballots != int64(voters) || len(chart.Counts) != 3 {
		t.Fatal("timeline results failed reconciliation")
	}
	for i, id := range []int64{choices.DancerID, choices.VocalistID, choices.ActorID} {
		bins := chart.Counts[i][strconv.FormatInt(id, 10)]
		if len(chart.Counts[i]) != 1 || len(bins) != len(seconds) {
			t.Fatal("timeline and journal differ")
		}
		for second, count := range seconds {
			if bins[second] != count {
				t.Fatal("timeline counts differ from journal")
			}
		}
	}
}

func ballotTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("MEGALOVE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set MEGALOVE_TEST_DATABASE_URL to a local megalove_demo database")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil || config.Database != "megalove_demo" {
		t.Fatal("test database must be megalove_demo")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	base := stdlib.OpenDB(*config)
	t.Cleanup(func() { base.Close() })
	schema := pgx.Identifier{fmt.Sprintf("ballot_test_%d", time.Now().UnixNano())}.Sanitize()
	if _, err := base.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := base.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	config.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { db.Close() })
	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, demoSQL); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestBallotHTTPAndPersistence(t *testing.T) {
	db := ballotTestDB(t)
	v := votingEvent{db: db, slug: "demo"}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	var choices ballotChoices
	err := db.QueryRowContext(ctx, `
		SELECT min(participant_id) FILTER (WHERE nomination = 'dancer'),
		       min(participant_id) FILTER (WHERE nomination = 'vocalist'),
		       min(participant_id) FILTER (WHERE nomination = 'actor')
		FROM candidacies
	`).Scan(&choices.DancerID, &choices.VocalistID, &choices.ActorID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(choices)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/voting/status", v.status)
	mux.Handle("POST /api/voting/ballot", http.NewCrossOriginProtection().Handler(http.HandlerFunc(v.submitBallot)))
	request := func(method, path, body string, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(ctx, method, "https://vote.example"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	require := func(w *httptest.ResponseRecorder, code int, text string) {
		t.Helper()
		if w.Code != code || !strings.Contains(w.Body.String(), text) {
			t.Fatalf("expected %d %q, got %d %s", code, text, w.Code, w.Body.String())
		}
	}
	status := func(cookie *http.Cookie) *httptest.ResponseRecorder {
		return request("GET", "/api/voting/status", "", cookie, "")
	}
	newBrowser := func() *http.Cookie {
		w := status(nil)
		require(w, 200, `"voted":false`)
		cookies := w.Result().Cookies()
		if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly ||
			cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Path != "/api/voting" {
			t.Fatal("voter cookie is missing or unprotected")
		}
		return cookies[0]
	}
	post := func(cookie *http.Cookie, body string) *httptest.ResponseRecorder {
		return request("POST", "/api/voting/ballot", body, cookie, "https://vote.example")
	}
	change := func(from, to string) {
		t.Helper()
		if err := v.transition(ctx, from, to); err != nil {
			t.Fatal(err)
		}
	}
	browser := newBrowser()
	require(post(nil, string(data)), 409, "browser_required")
	require(post(browser, string(data)), 409, "voting_closed")
	change("preparing", "open")
	require(post(browser, `{"dancer_id":1}`), 400, "three_choices_required")
	wrong, _ := json.Marshal(ballotChoices{choices.ActorID, choices.VocalistID, choices.ActorID})
	require(post(browser, string(wrong)), 422, "invalid_candidate")
	require(request("POST", "/api/voting/ballot", string(data), browser, "https://other.example"), 403, "")

	// Two tabs share the cookie and submit simultaneously.
	tabReplies := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() { tabReplies <- post(browser, string(data)) }()
	}
	for range 2 {
		require(<-tabReplies, 200, `"accepted":true`)
	}
	require(status(browser), 200, `"voted":true`)

	// Discard the response after storing a ballot, then retry after closure.
	lostReplyBrowser := newBrowser()
	_ = post(lostReplyBrowser, string(data))
	change("open", "closed")
	require(status(lostReplyBrowser), 200, `"voted":true`)
	require(post(lostReplyBrowser, string(data)), 200, `"accepted":true`)
	require(post(newBrowser(), string(data)), 409, "voting_closed")
	change("closed", "open")
	require(post(browser, string(wrong)), 200, `"accepted":true`)

	// Closure and a new ballot compete for the same event lock.
	racingBrowser := newBrowser()
	start := make(chan struct{})
	closed := make(chan error, 1)
	raceReply := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		<-start
		closed <- v.transition(ctx, "open", "closed")
	}()
	go func() {
		<-start
		raceReply <- post(racingBrowser, string(data))
	}()
	close(start)
	reply := <-raceReply
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	wantCount := 2
	if reply.Code == 200 {
		wantCount++
		require(status(racingBrowser), 200, `"voted":true`)
	} else {
		require(reply, 409, "voting_closed")
		require(status(racingBrowser), 200, `"voted":false`)
	}
	require(post(newBrowser(), string(data)), 409, "voting_closed")
	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM ballots").Scan(&count); err != nil || count != wantCount {
		t.Fatalf("got %d ballots, want %d: %v", count, wantCount, err)
	}
	var stored ballotChoices
	err = db.QueryRowContext(ctx, `
		SELECT dancer_id, vocalist_id, actor_id FROM ballots WHERE browser_id = $1::uuid
	`, browser.Value).Scan(&stored.DancerID, &stored.VocalistID, &stored.ActorID)
	if err != nil || stored != choices {
		t.Fatalf("original choices changed: %+v, %v", stored, err)
	}
}

func TestBallotPersistsAfterDisconnect(t *testing.T) {
	db := ballotTestDB(t)
	v := votingEvent{db: db, slug: "demo"}
	if err := v.transition(context.Background(), "preparing", "open"); err != nil {
		t.Fatal(err)
	}
	const browser = "11111111-1111-4111-8111-111111111111"
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		r := httptest.NewRequestWithContext(ctx, "POST", "/api/voting/ballot", disconnectOnRead{
			strings.NewReader(`{"dancer_id":1,"vocalist_id":2,"actor_id":3}`), cancel,
		})
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-MegaLove-Event", "demo/1")
		r.AddCookie(&http.Cookie{Name: voterCookie, Value: browser})
		w := httptest.NewRecorder()
		v.submitBallot(w, r)
		cancel()
		if w.Code != 200 {
			t.Fatalf("submission %d: %d %s", i, w.Code, w.Body)
		}
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM ballots").Scan(&count); err != nil || count != 1 {
		t.Fatalf("expected exactly one ballot: %d %v", count, err)
	}
}
