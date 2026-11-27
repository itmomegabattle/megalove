package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type disconnectOnRead struct {
	io.Reader
	cancel context.CancelFunc
}

func (r disconnectOnRead) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.cancel()
	return n, err
}

func TestParticipantRegistration(t *testing.T) {
	db := ballotTestDB(t)
	v := votingEvent{db: db, slug: "demo"}
	a := participantAdmin{db}
	toggle := func(action string) {
		t.Helper()
		values := url.Values{"event_id": {"1"}, "action": {action}}
		r := httptest.NewRequest("POST", "/admin/registration", strings.NewReader(values.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		v.toggleRegistration(w, r)
		if w.Code != 303 {
			t.Fatalf("toggle %s: %d %s", action, w.Code, w.Body)
		}
	}
	qr := func() []byte {
		t.Helper()
		r := httptest.NewRequest("GET", "/", nil)
		r.SetPathValue("event", "1")
		w := httptest.NewRecorder()
		v.registrationQR(w, r)
		if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
			t.Fatalf("registration QR: %d", w.Code)
		}
		return w.Body.Bytes()
	}
	originalQR := qr()
	toggle("open")
	var token string
	if err := db.QueryRow("SELECT registration_token FROM events WHERE id=1").Scan(&token); err != nil {
		t.Fatal(err)
	}
	disconnect := false
	request := func(method string, body *bytes.Buffer, media string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/register/"+token, body)
		if disconnect && method == "POST" {
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			r = r.WithContext(ctx)
			r.Body = io.NopCloser(disconnectOnRead{r.Body, cancel})
		}
		r.SetPathValue("token", token)
		r.Header.Set("Content-Type", media)
		w := httptest.NewRecorder()
		a.register(w, r)
		return w
	}
	page := request("GET", new(bytes.Buffer), "")
	match := regexp.MustCompile(`name="submission" value="([a-f0-9]+)"`).FindStringSubmatch(page.Body.String())
	if page.Code != 200 || len(match) != 2 {
		t.Fatalf("form: %d %s", page.Code, page.Body)
	}
	submit := func(name, submission string) *httptest.ResponseRecorder {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		for k, value := range map[string]string{"name": name, "faculty": "ktu", "actor": "on", "dancer": "on", "character": "Рассказчик", "submission": submission} {
			if err := form.WriteField(k, value); err != nil {
				t.Fatal(err)
			}
		}
		file, err := form.CreateFormFile("photo", "portrait.png")
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(file, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
			t.Fatal(err)
		}
		if err := form.Close(); err != nil {
			t.Fatal(err)
		}
		return request("POST", &body, form.FormDataContentType())
	}
	single := func() *httptest.ResponseRecorder { return submit("Пробный Участник", match[1]) }
	for i := range 2 {
		disconnect = i == 0
		w := single()
		if w.Code != 303 || w.Header().Get("Location") != "/register/complete" {
			t.Fatalf("submit: %d %s", w.Code, w.Body)
		}
	}
	var count, roles int
	err := db.QueryRow(`SELECT count(DISTINCT p.id), count(c.nomination)
 FROM participants p JOIN candidacies c ON c.event_id=p.event_id AND c.participant_id=p.id
 WHERE p.registration_submission=$1 AND p.photo_jpeg IS NOT NULL`, match[1]).Scan(&count, &roles)
	if err != nil || count != 1 || roles != 2 {
		t.Fatalf("stored: %d/%d %v", count, roles, err)
	}
	var id int64
	if err := db.QueryRow("SELECT id FROM participants WHERE registration_submission=$1", match[1]).Scan(&id); err != nil {
		t.Fatal(err)
	}
	directory := func(path string) string {
		t.Helper()
		w := httptest.NewRecorder()
		v.people(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("directory %s: %d %s", path, w.Code, w.Body)
		}
		return w.Body.String()
	}
	root := directory("/people")
	if strings.Count(root, `class="choice people-link"`) != 5 || strings.Contains(root, "Пробный Участник") {
		t.Fatal("faculties should be displayed before candidate cards")
	}
	rolesPage := directory("/people?faculty=ktu")
	if strings.Count(rolesPage, `class="choice people-link"`) != 3 {
		t.Fatal("missing role choices")
	}
	choicePage := directory("/people?faculty=ktu&role=dancer")
	if !strings.Contains(choicePage, "Без группы") {
		t.Fatal("unassigned rosters became inaccessible")
	}
	assertPeople := func(group string, want bool) {
		t.Helper()
		body := directory("/people?faculty=ktu&role=dancer&group=" + group)
		if strings.Contains(body, "Пробный Участник") != want || strings.Contains(body, match[1]) || strings.Contains(body, "Анна Волкова") {
			t.Fatalf("unexpected directory for %s", group)
		}
		photo := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/people/photos/", nil)
		req.SetPathValue("person", fmt.Sprint(id))
		v.peoplePhoto(photo, req)
		if photo.Code != 200 || photo.Body.Len() != ballotPhotoBytes {
			t.Fatalf("public portrait: %d", photo.Code)
		}
	}
	assertPeople("unassigned", true)
	bulkPage := httptest.NewRecorder()
	viewRequest := httptest.NewRequest("GET", "/", nil)
	viewRequest.SetPathValue("event", "1")
	a.directoryGroups(bulkPage, viewRequest)
	if bulkPage.Code != 200 || !strings.Contains(bulkPage.Body.String(), fmt.Sprintf("group-%d", id)) {
		t.Fatal("bulk list is missing applicant")
	}
	assign := httptest.NewRequest("POST", "/", strings.NewReader(fmt.Sprintf("group-%d=boy", id)))
	assign.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	assign.SetPathValue("event", "1")
	saved := httptest.NewRecorder()
	a.assignDirectoryGroups(saved, assign)
	if saved.Code != 303 {
		t.Fatalf("bulk assignment: %d %s", saved.Code, saved.Body)
	}
	assertPeople("boy", true)
	assertPeople("girl", false)
	assertPeople("unassigned", false)

	db.SetMaxOpenConns(8)
	const uploads = 50
	var pending sync.WaitGroup
	start := make(chan struct{})
	for i := range uploads {
		pending.Go(func() {
			<-start
			w := submit(fmt.Sprintf("Участник %d", i), newRegistrationKey())
			if w.Code != 303 || w.Header().Get("Location") != "/register/complete" {
				t.Errorf("upload %d: HTTP %d", i, w.Code)
			}
		})
	}
	began := time.Now()
	close(start)
	pending.Wait()
	t.Logf("%d concurrent registration uploads took %s", uploads, time.Since(began))
	if err := db.QueryRow("SELECT count(*) FROM participants WHERE registration_submission IS NOT NULL AND octet_length(photo_jpeg)=98304").Scan(&count); err != nil || count != uploads+1 {
		t.Fatalf("stored normalized portraits: %d, error: %v", count, err)
	}
	toggle("close")
	assertPeople("boy", true)
	if w := single(); w.Code != 404 {
		t.Fatalf("after closing: %d", w.Code)
	}
	if !bytes.Equal(originalQR, qr()) {
		t.Fatal("QR changed when registration closed")
	}
	toggle("open")
	var reopened string
	if err := db.QueryRow("SELECT registration_token FROM events WHERE id=1").Scan(&reopened); err != nil || reopened != token {
		t.Fatalf("registration URL changed after reopening: %v", err)
	}
	if w := request("GET", new(bytes.Buffer), ""); w.Code != 200 {
		t.Fatalf("old registration URL after reopening: %d", w.Code)
	}
	if !bytes.Equal(originalQR, qr()) {
		t.Fatal("QR changed after reopening")
	}

	if err := v.transition(context.Background(), "preparing", "open"); err != nil {
		t.Fatal(err)
	}
	if w := single(); w.Code != 404 {
		t.Fatalf("after voting opens: %d", w.Code)
	}
}
