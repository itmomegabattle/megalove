package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type benchmark struct {
	base     string
	client   *http.Client
	inFlight atomic.Int64
	peak     atomic.Int64
}

type sample struct {
	Duration time.Duration
	Status   int
	Bytes    int64
	Error    string
}

type candidate struct {
	ID                  int64
	Nomination, Faculty string
	HasPhoto            bool
}

type choices struct {
	Dancer   int64 `json:"dancer_id"`
	Vocalist int64 `json:"vocalist_id"`
	Actor    int64 `json:"actor_id"`
}

type visitor struct {
	Cookie *http.Cookie
	Choice choices
}

type voteStatus struct {
	Event, State string
	Voted        bool
}

func newBenchmark(base string) (*benchmark, error) {
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme != "http" || !net.ParseIP(parsed.Hostname()).IsLoopback() {
		return nil, fmt.Errorf("load target must be a loopback HTTP URL for the isolated fixture")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxConnsPerHost = 256
	transport.MaxIdleConns = 256
	transport.MaxIdleConnsPerHost = 256
	return &benchmark{base: strings.TrimRight(base, "/"), client: &http.Client{
		Transport: transport, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (b *benchmark) request(method, path, contentType string, body []byte, cookie *http.Cookie, target any) (sample, []*http.Cookie) {
	start := time.Now()
	r, err := http.NewRequest(method, b.base+path, bytes.NewReader(body))
	if err != nil {
		return sample{Error: err.Error()}, nil
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if method == http.MethodPost && path == "/api/voting/ballot" {
		var counted atomic.Bool
		r = r.WithContext(httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{
			GotConn: func(httptrace.GotConnInfo) {
				if counted.CompareAndSwap(false, true) {
					active := b.inFlight.Add(1)
					for peak := b.peak.Load(); active > peak && !b.peak.CompareAndSwap(peak, active); peak = b.peak.Load() {
					}
				}
			},
		}))
		defer func() {
			if counted.Load() {
				b.inFlight.Add(-1)
			}
		}()
	}
	res, err := b.client.Do(r)
	if err != nil {
		return sample{Duration: time.Since(start), Error: err.Error()}, nil
	}
	defer res.Body.Close()
	var n int64
	if target != nil && res.StatusCode == 200 {
		err = json.NewDecoder(res.Body).Decode(target)
	} else {
		n, err = io.Copy(io.Discard, res.Body)
	}
	s := sample{Duration: time.Since(start), Status: res.StatusCode, Bytes: n}
	if err != nil {
		s.Error = err.Error()
	}
	return s, res.Cookies()
}

func (b *benchmark) status(cookie *http.Cookie) (voteStatus, *http.Cookie, sample) {
	var status voteStatus
	s, cookies := b.request("GET", "/api/voting/status", "", nil, cookie, &status)
	for _, c := range cookies {
		if c.Name == "megalove_voter" {
			cookie = c
			break
		}
	}
	return status, cookie, s
}

func (b *benchmark) ballot(v visitor) sample {
	data, _ := json.Marshal(v.Choice)
	var response struct{ Accepted bool }
	s, _ := b.request("POST", "/api/voting/ballot", "application/json", data, v.Cookie, &response)
	if s.Status == 200 && !response.Accepted {
		s.Error = "no acceptance in response"
	}
	return s
}

func distribute(total int, window time.Duration, task func(int)) {
	start := time.Now().Add(30 * time.Millisecond)
	var wg sync.WaitGroup
	for i := range total {
		wg.Go(func() {
			time.Sleep(time.Until(start.Add(time.Duration(i) * window / time.Duration(total))))
			task(i)
		})
	}
	wg.Wait()
}

func summarize(label string, samples []sample) int {
	counts := map[string]int{}
	var times []float64
	var sent int64
	var example string
	for _, s := range samples {
		key := fmt.Sprint(s.Status)
		if s.Error != "" {
			key = "error"
			if example == "" {
				example = s.Error
			}
		}
		counts[key]++
		if key == "200" {
			times = append(times, float64(s.Duration.Microseconds())/1000)
			sent += s.Bytes
		}
	}
	sort.Float64s(times)
	p95 := 0.0
	if len(times) > 0 {
		p95 = times[int(math.Ceil(float64(len(times))*.95))-1]
	}
	report := map[string]any{"stage": label, "requests": len(samples), "status": counts, "p95_ms": p95, "bytes": sent}
	if example != "" {
		report["example_error"] = example
	}
	out, _ := json.Marshal(report)
	fmt.Println(string(out))
	return counts["200"]
}

func (b *benchmark) fixtureChoices() (map[string][]candidate, error) {
	state, _, s := b.status(nil)
	if s.Error != "" || s.Status != 200 || state.Event != "loadtest" || state.State != "open" {
		return nil, fmt.Errorf("refusing load outside open loadtest event: %+v / %+v", state, s)
	}
	var catalog struct{ Candidates []candidate }
	s, _ = b.request("GET", "/api/voting/catalog", "", nil, nil, &catalog)
	if s.Status != 200 || s.Error != "" || len(catalog.Candidates) != fixtureCount {
		return nil, fmt.Errorf("unexpected fixture catalog: %d candidates, %+v", len(catalog.Candidates), s)
	}
	groups := map[string][]candidate{}
	for _, c := range catalog.Candidates {
		if !c.HasPhoto {
			return nil, fmt.Errorf("candidate %d is missing a photo", c.ID)
		}
		groups[c.Nomination] = append(groups[c.Nomination], c)
	}
	if len(groups["dancer"]) != 125 || len(groups["vocalist"]) != 20 || len(groups["actor"]) != 90 {
		return nil, fmt.Errorf("unexpected nomination sizes")
	}
	return groups, nil
}

func runProfile(base string, users int, arrival, burst time.Duration) error {
	b, err := newBenchmark(base)
	if err != nil {
		return err
	}
	groups, err := b.fixtureChoices()
	if err != nil {
		return err
	}
	visitors := make([]visitor, users)
	type visitorMetrics struct{ assets, catalog, status, photos []sample }
	metrics := make([]visitorMetrics, users)
	assets := []string{"/", "/assets/viewer.css", "/assets/viewer.js", "/assets/brand-background.css", "/assets/brand-background.js", "/assets/logo.png", "/assets/druk.woff2", "/assets/raleway.woff2"}
	photoRequests := 3
	visitStart := time.Now()
	distribute(users, arrival, func(i int) {
		m := &metrics[i]
		for _, path := range assets {
			s, _ := b.request("GET", path, "", nil, nil, nil)
			m.assets = append(m.assets, s)
		}
		_, cookie, s := b.status(nil)
		m.status = append(m.status, s)
		s, _ = b.request("GET", "/api/voting/catalog", "", nil, cookie, nil)
		m.catalog = append(m.catalog, s)
		d := groups["dancer"][i%len(groups["dancer"])].ID
		v := groups["vocalist"][(i*3)%len(groups["vocalist"])].ID
		a := groups["actor"][(i*7)%len(groups["actor"])].ID
		visitors[i] = visitor{cookie, choices{d, v, a}}
		for _, id := range []int64{d, v, a} {
			s, _ := b.request("GET", fmt.Sprintf("/api/voting/photos/%d", id), "", nil, cookie, nil)
			if s.Status == 200 && s.Bytes != photoBytes {
				s.Error = "incorrect photo length"
			}
			m.photos = append(m.photos, s)
		}
	})
	fmt.Printf("Visited %d browser sessions in %.2fs\n", users, time.Since(visitStart).Seconds())
	var all visitorMetrics
	for _, m := range metrics {
		all.assets = append(all.assets, m.assets...)
		all.status = append(all.status, m.status...)
		all.catalog = append(all.catalog, m.catalog...)
		all.photos = append(all.photos, m.photos...)
	}
	assetsOK := summarize("assets", all.assets)
	statusOK := summarize("browser_status", all.status)
	catalogOK := summarize("catalog", all.catalog)
	photoOK := summarize("photos", all.photos)
	if assetsOK != users*len(assets) || statusOK != users || catalogOK != users || photoOK != users*photoRequests {
		return fmt.Errorf("visitor phase had failed requests")
	}
	replies := make([]sample, users)
	start := time.Now()
	distribute(users, burst, func(i int) { replies[i] = b.ballot(visitors[i]) })
	fmt.Printf("Submitted %d ballots in %.2fs (scheduled window %v)\n", users, time.Since(start).Seconds(), burst)
	if summarize("ballots", replies) != users {
		return fmt.Errorf("some ballots were not acknowledged")
	}
	retries := make([]sample, max(1, users/10))
	distribute(len(retries), time.Second, func(i int) {
		status, _, probe := b.status(visitors[i].Cookie)
		if probe.Status != 200 || !status.Voted {
			retries[i] = sample{Error: "accepted ballot not visible after discarded acknowledgement"}
			return
		}
		retries[i] = b.ballot(visitors[i])
	})
	if summarize("retries", retries) != len(retries) {
		return fmt.Errorf("some safe retries failed")
	}
	within := 0
	for _, s := range replies {
		if s.Duration <= 2*time.Second {
			within++
		}
	}
	if within*100 < 95*users {
		return fmt.Errorf("less than 95%% of ballots completed within 2s")
	}
	return nil
}

func runBallotBurst(base string, users int, window time.Duration) error {
	b, err := newBenchmark(base)
	if err != nil {
		return err
	}
	b.client.Transport.(*http.Transport).MaxConnsPerHost = users
	groups, err := b.fixtureChoices()
	if err != nil {
		return err
	}
	visitors := make([]visitor, users)
	statuses := make([]sample, users)
	distribute(users, time.Second, func(i int) {
		_, cookie, s := b.status(nil)
		if cookie == nil {
			s.Error = "missing voter cookie"
		}
		statuses[i] = s
		dancer := i % len(groups["dancer"])
		if i < users/2 {
			dancer = 0
		}
		visitors[i] = visitor{cookie, choices{groups["dancer"][dancer].ID, groups["vocalist"][i%len(groups["vocalist"])].ID, groups["actor"][i%len(groups["actor"])].ID}}
	})
	if summarize("prewarm", statuses) != users {
		return fmt.Errorf("could not initialize all viewers")
	}
	replies := make([]sample, users)
	started := time.Now()
	distribute(users, window, func(i int) { replies[i] = b.ballot(visitors[i]) })
	fmt.Printf("Burst duration=%v peak_connected_ballots=%d concentrated_dancer=%d\n", time.Since(started), b.peak.Load(), visitors[0].Choice.Dancer)
	if summarize("burst_ballots", replies) != users {
		return fmt.Errorf("some ballots were not acknowledged")
	}
	retries := make([]sample, max(1, users/10))
	distribute(len(retries), 0, func(i int) { retries[i] = b.ballot(visitors[i]) })
	if summarize("burst_retries", retries) != len(retries) {
		return fmt.Errorf("some retries failed")
	}
	return nil
}

func runCloseRace(base string, users int) error {
	b, err := newBenchmark(base)
	if err != nil {
		return err
	}
	groups, err := b.fixtureChoices()
	if err != nil {
		return err
	}
	password := os.Getenv("MEGALOVE_LOAD_PASSWORD")
	if password == "" {
		return fmt.Errorf("set isolated test administrator password")
	}
	login, cookies := b.request("POST", "/admin/login", "application/x-www-form-urlencoded", []byte(url.Values{"password": {password}}.Encode()), nil, nil)
	if login.Status != 303 || len(cookies) != 1 {
		return fmt.Errorf("isolated admin login failed: %+v", login)
	}
	visitors := make([]visitor, users)
	for i := range users {
		_, c, s := b.status(nil)
		if s.Status != 200 || c == nil {
			return fmt.Errorf("race browser initialization failed")
		}
		visitors[i] = visitor{c, choices{groups["dancer"][0].ID, groups["vocalist"][0].ID, groups["actor"][0].ID}}
	}
	var wg sync.WaitGroup
	replies := make([]sample, users)
	var closeResult sample
	wg.Go(func() {
		time.Sleep(530 * time.Millisecond)
		closeResult, _ = b.request("POST", "/admin/voting", "application/x-www-form-urlencoded",
			[]byte("from=open&to=closed"), cookies[0], nil)
	})
	distribute(users, time.Second, func(i int) { replies[i] = b.ballot(visitors[i]) })
	wg.Wait()
	if closeResult.Status != 303 {
		return fmt.Errorf("isolated close failed: %+v", closeResult)
	}
	accepted := summarize("close_race", replies)
	blocked := 0
	for i, s := range replies {
		if s.Status == 409 && s.Error == "" {
			blocked++
		}
		status, _, probe := b.status(visitors[i].Cookie)
		if probe.Status != 200 || status.Voted != (s.Status == 200 && s.Error == "") {
			return fmt.Errorf("race result did not match database-backed status")
		}
	}
	if accepted == 0 || blocked == 0 || accepted+blocked != users {
		return fmt.Errorf("invalid close race results: %d accepted, %d blocked", accepted, blocked)
	}
	_, fresh, _ := b.status(nil)
	if s := b.ballot(visitor{fresh, visitors[0].Choice}); s.Status != 409 {
		return fmt.Errorf("new ballot accepted after close")
	}
	for i, s := range replies {
		if s.Status == 200 {
			if retry := b.ballot(visitors[i]); retry.Status != 200 || retry.Error != "" {
				return fmt.Errorf("accepted ballot retry after close failed")
			}
			break
		}
	}
	fmt.Printf("Race accepted=%d blocked=%d\n", accepted, blocked)
	return nil
}
