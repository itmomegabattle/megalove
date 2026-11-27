package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestProvisionBackupWithGoogle(t *testing.T) {
	credentials := os.Getenv("MEGALOVE_TEST_GOOGLE_CREDENTIALS")
	folder := os.Getenv("MEGALOVE_TEST_GOOGLE_FOLDER")
	if credentials == "" || !driveFileID.MatchString(folder) {
		t.Skip("set test Google credentials and destination folder")
	}
	db := ballotTestDB(t)
	t.Setenv("MEGALOVE_BACKUP_CREDENTIALS", credentials)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	v := votingEvent{db: db, slug: "google-test"}
	settings := backupSettings{credentials: credentials}
	var event int64
	var cleanupID string
	name := fmt.Sprintf("MegaLove integration test %d", time.Now().Unix())
	if err := db.QueryRowContext(ctx, "INSERT INTO events(slug,name,backup_requested) VALUES ('google-test',$1,true) RETURNING id",
		name).Scan(&event); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		var id string
		id = cleanupID
		if id == "" {
			if err := db.QueryRow("SELECT coalesce(backup_form_id,'') FROM events WHERE id=$1", event).Scan(&id); err != nil {
				t.Error(err)
				return
			}
		}
		if id == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		client, err := settings.client(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, backupDriveFiles+id, nil)
		if err != nil {
			t.Error(err)
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("temporary form cleanup HTTP %d", resp.StatusCode)
		}
	})
	if err := v.provisionNextBackup(ctx, folder, settings); err != nil {
		t.Fatal(err)
	}
	first, err := eventBackupSettings(ctx, db, event)
	if err != nil || !driveFileID.MatchString(first.id) || !responderURL(first.link) {
		t.Fatal("provisioned form was not attached to the test event", err)
	}
	cleanupID = first.id
	qrRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	qrRequest.SetPathValue("event", strconv.FormatInt(event, 10))
	beforeQR := httptest.NewRecorder()
	v.backupQR(beforeQR, qrRequest)
	expectedQR, err := whiteBackupQR(first.link)
	if err != nil || beforeQR.Code != http.StatusOK || beforeQR.Header().Get("Content-Type") != "image/png" ||
		!bytes.Equal(beforeQR.Body.Bytes(), expectedQR) {
		t.Fatal("QR download did not use the viewer URL", err)
	}
	if err := v.provisionBackup(ctx, event, folder, settings); err != nil {
		t.Fatal(err)
	}
	retry, err := eventBackupSettings(ctx, db, event)
	if err != nil || retry.id != first.id || retry.link != first.link {
		t.Fatal("repeated provision changed the form or responder URL", err)
	}
	client, err := settings.client(ctx)
	if err != nil {
		t.Fatal(err)
	}
	form, err := requestGoogleForm(ctx, client, http.MethodGet, first.endpoint(), "")
	state := form.PublishSettings.State
	if err != nil || state == nil || !state.Published || !state.Accepting || form.ResponderURI != first.link {
		t.Fatal("unexpected responder URL or publication state", err)
	}
	fixtureURL := os.Getenv("MEGALOVE_TEST_GOOGLE_PHOTO")
	if os.Getenv("MEGALOVE_BACKUP_PORTRAIT_ORIGIN") == "" && fixtureURL == "" {
		if err := v.removeEvent(ctx, event, name); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM events WHERE id=$1", event).Scan(&count); err != nil || count != 0 {
			t.Fatalf("event not removed: %d %v", count, err)
		}
		return
	}
	const secret = "isolated-photo-test-key"
	t.Setenv("MEGALOVE_ADMIN_HASH", secret)
	listener, err := net.Listen("tcp", "127.0.0.1:58584")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/backup/portraits/{person}", v.backupPortrait)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go server.Serve(listener)
	defer server.Close()
	var photo bytes.Buffer
	if err := jpeg.Encode(&photo, image.NewRGBA(image.Rect(0, 0, 40, 40)), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `WITH p AS (
		INSERT INTO participants(event_id,faculty_code,display_name,photo_jpeg)
		SELECT $1,'ktu', 'Test candidate ' || n,$2 FROM generate_series(1,3) n RETURNING id
	) INSERT INTO candidacies(event_id,participant_id,nomination)
	SELECT $1,id,(array['dancer','vocalist','actor'])[row_number() OVER (ORDER BY id)] FROM p`, event, photo.Bytes()); err != nil {
		t.Fatal(err)
	}
	if fixtureURL == "" {
		var person int64
		if err := db.QueryRowContext(ctx, "SELECT min(id) FROM participants WHERE event_id=$1", event).Scan(&person); err != nil {
			t.Fatal(err)
		}
		until := time.Now().Add(5 * time.Minute).Unix()
		probe := fmt.Sprintf("%s/api/backup/portraits/%d?event=%s&until=%d&signature=%s",
			strings.TrimRight(os.Getenv("MEGALOVE_BACKUP_PORTRAIT_ORIGIN"), "/"), person, first.event, until, photoSignature(secret, first.event, person, until))
		probeClient := &http.Client{Timeout: 3 * time.Second}
		var reachable bool
		for range 8 {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, probe, nil)
			if err != nil {
				t.Fatal("invalid test portrait URL")
			}
			resp, err := probeClient.Do(req)
			if err == nil {
				body, err := io.ReadAll(io.LimitReader(resp.Body, 100000))
				resp.Body.Close()
				if err == nil && resp.StatusCode == 200 && bytes.Equal(body, photo.Bytes()) {
					reachable = true
					break
				}
				t.Logf("Public portrait probe HTTP %d", resp.StatusCode)
			}
			time.Sleep(time.Second)
		}
		if !reachable {
			t.Fatal("test portrait is not reachable through the public HTTPS origin")
		}
	}
	if err := v.syncBackupWithPortraits(ctx, first, func(candidates []backupPortrait) map[string]any {
		return backupQuestion(candidates, first.event, secret, fixtureURL)
	}); err != nil {
		t.Fatal(err)
	}
	form, err = requestGoogleForm(ctx, client, http.MethodGet, first.endpoint(), "")
	if err != nil || len(form.Items) != 3 || form.ResponderURI != first.link {
		t.Fatal("unexpected synchronized form", err)
	}
	for _, item := range form.Items {
		q := item.QuestionItem.Question
		if !q.Required || q.ChoiceQuestion.Type != "RADIO" || len(q.ChoiceQuestion.Options) != 1 {
			t.Fatalf("wrong question shape for %s", item.Title)
		}
		opt := q.ChoiceQuestion.Options[0]
		if opt.Image == nil || opt.Image.ContentURI == "" {
			t.Fatalf("missing portrait for %s", item.Title)
		}
	}
	if _, err := db.ExecContext(ctx, `WITH inserted AS (
        INSERT INTO participants(event_id,faculty_code,display_name,photo_jpeg,registration_submission)
        VALUES($1,'ktu','New test registration',$2,$3) RETURNING id)
        INSERT INTO candidacies(event_id,participant_id,nomination) SELECT $1,id,'vocalist' FROM inserted`,
		event, photo.Bytes(), newRegistrationKey()); err != nil {
		t.Fatal(err)
	}
	if err := v.syncBackupWithPortraits(ctx, first, func(candidates []backupPortrait) map[string]any {
		return backupQuestion(candidates, first.event, secret, fixtureURL)
	}); err != nil {
		t.Fatal(err)
	}
	form, err = requestGoogleForm(ctx, client, http.MethodGet, first.endpoint(), "")
	if err != nil || form.ResponderURI != first.link || form.PublishSettings.State == nil || !form.PublishSettings.State.Accepting ||
		len(form.Items) != 3 || len(form.Items[1].QuestionItem.Question.ChoiceQuestion.Options) != 2 {
		t.Fatal("open form did not synchronize the new registration at its original URL", err)
	}

	if _, err := db.ExecContext(ctx, "UPDATE participants SET display_name='Renamed test candidate' WHERE event_id=$1 AND display_name='Test candidate 1'", event); err != nil {
		t.Fatal(err)
	}
	if err := v.syncBackupWithPortraits(ctx, first, func(candidates []backupPortrait) map[string]any {
		return backupQuestion(candidates, first.event, secret, fixtureURL)
	}); err != nil {
		t.Fatal(err)
	}
	form, err = requestGoogleForm(ctx, client, http.MethodGet, first.endpoint(), "")
	if err != nil || len(form.Items) != 3 || form.ResponderURI != first.link {
		t.Fatal("unexpected form after editing the roster", err)
	}
	options := form.Items[0].QuestionItem.Question.ChoiceQuestion.Options
	if len(options) != 1 || !strings.HasPrefix(options[0].Value, "Renamed test candidate ·") || options[0].Image == nil || options[0].Image.ContentURI == "" {
		t.Fatal("Google did not keep the updated candidate and photo")
	}
	afterQR := httptest.NewRecorder()
	v.backupQR(afterQR, qrRequest)
	if afterQR.Code != http.StatusOK || !bytes.Equal(beforeQR.Body.Bytes(), afterQR.Body.Bytes()) {
		t.Fatal("QR download changed after updating a candidate")
	}
	if _, err := db.ExecContext(ctx, "UPDATE events SET backup_reviewed_count=4,state='open' WHERE id=$1", event); err != nil {
		t.Fatal(err)
	}
	first, err = eventBackupSettings(ctx, db, event)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	v.activateBackupWith(ctx, w, event, first, client)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("activation HTTP %d: %s", w.Code, w.Body.String())
	}
	var closed bool
	if err := db.QueryRowContext(ctx, "SELECT state='closed' AND backup_activated_at IS NOT NULL FROM events WHERE id=$1", event).Scan(&closed); err != nil || !closed {
		t.Fatal("main voting did not close", err)
	}
	form, err = requestGoogleForm(ctx, client, http.MethodGet, first.endpoint(), "")
	if err != nil || form.PublishSettings.State == nil || !form.PublishSettings.State.Accepting || form.ResponderURI != first.link {
		t.Fatal("Google did not accept responses at the same link", err)
	}
	values := url.Values{}
	for _, item := range form.Items {
		q := item.QuestionItem.Question
		id, err := strconv.ParseUint(q.QuestionID, 16, 32)
		if err != nil {
			t.Fatal(err)
		}
		values.Set("entry."+strconv.FormatUint(id, 10), q.ChoiceQuestion.Options[0].Value)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(first.link, "/viewform")+"/formResponse", strings.NewReader(values.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("respondent HTTP %d", resp.StatusCode)
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, first.endpoint()+"/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var recorded struct {
		Responses []struct {
			Answers map[string]struct {
				TextAnswers struct{ Answers []struct{ Value string } }
			}
		}
	}
	if resp.StatusCode != 200 {
		t.Fatalf("responses API HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&recorded); err != nil {
		t.Fatal(err)
	}
	if len(recorded.Responses) != 1 || len(recorded.Responses[0].Answers) != 3 {
		t.Fatal("Google did not record exactly one complete ballot")
	}
	for _, item := range form.Items {
		q := item.QuestionItem.Question
		answers := recorded.Responses[0].Answers[q.QuestionID].TextAnswers.Answers
		if len(answers) != 1 || answers[0].Value != q.ChoiceQuestion.Options[0].Value {
			t.Fatalf("unexpected stored answer for %s", item.Title)
		}
	}
}

func TestSignedBackupPortrait(t *testing.T) {
	db := ballotTestDB(t)
	const secret = "test-portrait-signing-key"
	t.Setenv("MEGALOVE_ADMIN_HASH", secret)
	var photo bytes.Buffer
	if err := jpeg.Encode(&photo, image.NewRGBA(image.Rect(0, 0, 40, 40)), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE participants SET photo_jpeg=$1 WHERE id=1", photo.Bytes()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO events(slug,name) VALUES ('another','Another test event')"); err != nil {
		t.Fatal(err)
	}
	v := votingEvent{db: db}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/backup/portraits/{person}", v.backupPortrait)
	now := time.Now().Unix()
	for _, tc := range []struct {
		name, event, signedEvent string
		person, until            int64
		want                     int
	}{
		{"valid", "demo", "demo", 1, now + 60, 200},
		{"expired", "demo", "demo", 1, now - 60, 404},
		{"too far", "demo", "demo", 1, now + 720, 404},
		{"changed event", "another", "demo", 1, now + 60, 404},
		{"other event photo", "another", "another", 1, now + 60, 404},
		{"changed participant", "demo", "demo", 2, now + 60, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := fmt.Sprintf("/api/backup/portraits/%d?event=%s&until=%d&signature=%s", tc.person, tc.event, tc.until, photoSignature(secret, tc.signedEvent, 1, tc.until))
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			if w.Code != tc.want || (w.Code == 200 && (w.Header().Get("Content-Type") != "image/jpeg" ||
				w.Header().Get("Cache-Control") != "no-store" || !bytes.Equal(w.Body.Bytes(), photo.Bytes()))) {
				t.Fatalf("unexpected portrait response: HTTP %d", w.Code)
			}
		})
	}
}

func TestWhiteBackupQR(t *testing.T) {
	data, err := whiteBackupQR("https://docs.google.com/forms/d/e/testfixture/viewform")
	if err != nil {
		t.Fatal(err)
	}
	qr, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if qr.Bounds().Dx() != 768 || qr.Bounds().Dy() != 768 {
		t.Fatalf("unexpected QR dimensions: %s", qr.Bounds())
	}
	foundWhite, foundTransparent := false, false
	for y := range 768 {
		for x := range 768 {
			r, g, b, alpha := qr.At(x, y).RGBA()
			switch {
			case alpha == 0:
				foundTransparent = true
			case r == 65535 && g == 65535 && b == 65535 && alpha == 65535:
				foundWhite = true
			default:
				t.Fatalf("unexpected QR pixel at %d,%d", x, y)
			}
		}
	}
	if !foundWhite || !foundTransparent {
		t.Fatal("QR must contain both white modules and a transparent background")
	}
}

func TestBackupReviewInvalidatedByRosterChanges(t *testing.T) {
	db := ballotTestDB(t)
	ctx := context.Background()
	for _, change := range []string{
		"UPDATE participants SET display_name='Updated candidate' WHERE id=1",
		"UPDATE candidacies SET character_name='Updated character' WHERE nomination='actor'",
	} {
		if _, err := db.Exec(`UPDATE events SET backup_form_id='test-form-fixture',
			backup_responder_url='https://docs.google.com/forms/d/e/testfixture/viewform',backup_reviewed_count=5 WHERE id=1`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(change); err != nil {
			t.Fatal(err)
		}
		s, err := eventBackupSettings(ctx, db, 1)
		if err != nil || s.rehearsed || s.expected != 0 || s.id != "test-form-fixture" {
			t.Fatalf("review should reset but retain the form ID: reviewed=%v count=%d id=%q err=%v", s.rehearsed, s.expected, s.id, err)
		}
	}
}

func TestBackupSwitchOpensGoogleBeforeClosingSite(t *testing.T) {
	db := ballotTestDB(t)
	ctx := context.Background()
	v := votingEvent{db: db, slug: "demo"}
	var photo bytes.Buffer
	if err := jpeg.Encode(&photo, image.NewRGBA(image.Rect(0, 0, 40, 40)), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE participants SET photo_jpeg=$1", photo.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := v.transition(ctx, "preparing", "open"); err != nil {
		t.Fatal(err)
	}
	const link = "https://docs.google.com/forms/d/e/testfixture/viewform"
	settings := backupSettings{event: "demo", id: "test-form-fixture", link: link, expected: 5, rehearsed: true}
	const published = `"publishSettings":{"publishState":{"isPublished":true,"isAcceptingResponses":false}}`
	form := `{"responderUri":"` + link + `",` + published + `,"items":[
	{"title":"Самый яркий танцор","questionItem":{"question":{"required":true,"choiceQuestion":{"type":"RADIO","options":[{"value":"Анна Волкова · КТУ · №1"}]}}}},
	{"title":"Самый яркий вокалист","questionItem":{"question":{"required":true,"choiceQuestion":{"type":"DROP_DOWN","options":[{"value":"Полина Егорова · ТИНТ · №2"},{"value":"Илья Котов · ФТМФ · №4"}]}}}},
	{"title":"Самый яркий актёр","questionItem":{"question":{"required":true,"choiceQuestion":{"type":"RADIO","options":[{"value":"Дарья Лебедева · НОЖ · №3"},{"value":"Мария Белова · ФТМИ · №5"}]}}}}
	]}`
	for _, label := range []string{"Анна Волкова · КТУ · №1", "Полина Егорова · ТИНТ · №2", "Илья Котов · ФТМФ · №4", "Дарья Лебедева · НОЖ · №3", "Мария Белова · ФТМИ · №5"} {
		form = strings.Replace(form, `"value":"`+label+`"`, `"value":"`+label+`","image":{"contentUri":"https://example.invalid/photo","altText":"`+label+` [`+photoFingerprint(photo.Bytes())+`]" }`, 1)
	}
	postAllowed, failGoogle, invalidRoster := false, true, true
	calls := 0
	assertState := func(want string) {
		t.Helper()
		var state string
		if err := db.QueryRow("SELECT state FROM events WHERE id=1").Scan(&state); err != nil || state != want {
			t.Fatalf("main state = %s, want %s (%v)", state, want, err)
		}
	}
	client := &http.Client{Transport: photoTransport(func(r *http.Request) (*http.Response, error) {
		body, code := form, http.StatusOK
		if invalidRoster {
			body = strings.Replace(body, "Мария Белова", "Другой кандидат", 1)
		}
		if r.Method == http.MethodPost {
			calls++
			if !postAllowed || !strings.HasSuffix(r.URL.Path, ":setPublishSettings") {
				t.Fatal("unexpected Google mutation")
			}
			assertState("open")
			if failGoogle {
				code = http.StatusServiceUnavailable
			} else {
				body = fmt.Sprintf("{%s}", strings.Replace(published, "false", "true", 1))
			}
		}
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	syncTx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer syncTx.Rollback()
	if locked, err := tryBackupLock(ctx, syncTx, 1); err != nil || !locked {
		t.Fatalf("could not simulate active synchronization: %v", err)
	}
	blocked := httptest.NewRecorder()
	v.activateBackupWith(ctx, blocked, 1, settings, client)
	if blocked.Code != http.StatusConflict || calls != 0 {
		t.Fatalf("activation during synchronization: HTTP %d, Google writes=%d", blocked.Code, calls)
	}
	assertState("open")
	if err := syncTx.Rollback(); err != nil {
		t.Fatal(err)
	}
	for i, status := range []int{http.StatusConflict, http.StatusBadGateway, http.StatusSeeOther} {
		postAllowed, failGoogle, invalidRoster = i != 0, i != 2, i == 0
		w := httptest.NewRecorder()
		v.activateBackupWith(ctx, w, 1, settings, client)
		if w.Code != status {
			t.Fatalf("attempt %d: status %d, want %d: %s", i, w.Code, status, w.Body.String())
		}
		if i < 2 {
			assertState("open")
		}
	}
	assertState("closed")
	if calls != 2 {
		t.Fatalf("Google activation called %d times", calls)
	}
	if err := v.transition(ctx, "closed", "open"); err != errVotingTransition {
		t.Fatalf("reopened after failover: %v", err)
	}
}
