package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type backupPortrait struct {
	id                 int64
	label, fingerprint string
}

func photoFingerprint(data []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func photoSignature(secret, event string, person, until int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s/%d/%d", event, person, until)
	return hex.EncodeToString(mac.Sum(nil))
}

func (v votingEvent) backupPortrait(w http.ResponseWriter, r *http.Request) {
	event := r.URL.Query().Get("event")
	secret := os.Getenv("MEGALOVE_ADMIN_HASH")
	person := pathID(r, "person")
	until, _ := strconv.ParseInt(r.URL.Query().Get("until"), 10, 64)
	now := time.Now().Unix()
	if secret == "" || event == "" || until < now || until > now+600 ||
		subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("signature")), []byte(photoSignature(secret, event, person, until))) != 1 {
		http.NotFound(w, r)
		return
	}
	var photo []byte
	err := v.db.QueryRowContext(r.Context(), `SELECT p.photo_jpeg FROM participants p JOIN events e ON e.id=p.event_id
		WHERE e.slug=$1 AND p.id=$2 AND p.photo_jpeg IS NOT NULL`, event, person).Scan(&photo)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(photo)
}

func (v votingEvent) syncBackupLoop() {
	settings := currentBackupSettings()
	secret := os.Getenv("MEGALOVE_ADMIN_HASH")
	folder := os.Getenv("MEGALOVE_BACKUP_FOLDER_ID")
	managed := managedBackupSettings(settings)
	if (managed.credentials == "" || !driveFileID.MatchString(folder)) && (settings.credentials == "" || settings.event == "" || secret == "") {
		return
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		var err error
		if driveFileID.MatchString(folder) {
			err = v.provisionNextBackup(ctx, folder, managed)
		}
		if err == nil && secret != "" && settings.event != "" && driveFileID.MatchString(settings.id) && responderURL(settings.link) {
			err = v.syncBackup(ctx, settings, secret)
		}
		cancel()
		if secret != "" && driveFileID.MatchString(folder) {
			v.syncProvisionedBackups(managed, secret)
		}
		if err != nil {
			log.Printf("backup sync: %v", err)
		}
		<-ticker.C
	}
}

func (v votingEvent) syncBackup(ctx context.Context, settings backupSettings, secret string) error {
	return v.syncBackupWithPortraits(ctx, settings, func(candidates []backupPortrait) map[string]any {
		return backupQuestion(candidates, settings.event, secret, "")
	})
}

func (v votingEvent) syncBackupWithPortraits(ctx context.Context, settings backupSettings, question func([]backupPortrait) map[string]any) error {
	tx, err := v.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRowContext(ctx, "SELECT id FROM events WHERE slug=$1", settings.event).Scan(&id); err != nil {
		return err
	}
	locked, err := tryBackupLock(ctx, tx, id)
	if err != nil || !locked {
		return err
	}
	var activated bool
	var synced, syncedRoster string
	if err := tx.QueryRowContext(ctx, "SELECT backup_activated_at IS NOT NULL,coalesce(backup_synced_revision,''),coalesce(backup_synced_roster,'') FROM events WHERE id=$1", id).Scan(&activated, &synced, &syncedRoster); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	if activated {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT p.id,p.display_name,f.short_name,c.nomination,p.photo_jpeg
		FROM participants p JOIN events e ON e.id=p.event_id JOIN faculties f ON f.code=p.faculty_code
		JOIN candidacies c ON c.event_id=p.event_id AND c.participant_id=p.id
		WHERE e.slug=$1 ORDER BY f.short_name,p.display_name,p.id`, settings.event)
	if err != nil {
		return err
	}
	defer rows.Close()
	roster := map[string][]backupPortrait{}
	for rows.Next() {
		var p backupPortrait
		var name, faculty, code string
		var photo []byte
		if err := rows.Scan(&p.id, &name, &faculty, &code, &photo); err != nil {
			return err
		}
		if len(photo) == 0 {
			return fmt.Errorf("candidate %d has no portrait", p.id)
		}
		p.label = fmt.Sprintf("%s · %s · №%d", name, faculty, p.id)
		p.fingerprint = photoFingerprint(photo)
		roster[code] = append(roster[code], p)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	complete := true
	for code := range backupTitles {
		complete = complete && len(roster[code]) > 0
	}
	if !complete && !settings.managed {
		return nil
	}
	labels := make(map[string][]string)
	fingerprints := make(map[string]string)
	for code, candidates := range roster {
		for _, candidate := range candidates {
			labels[code] = append(labels[code], candidate.label)
			fingerprints[candidate.label] = portraitAlt(candidate)
		}
	}
	digest := backupRosterDigest(labels, fingerprints)
	client, err := settings.client(ctx)
	if err != nil {
		return err
	}
	client.Timeout = 35 * time.Second
	form, err := requestGoogleForm(ctx, client, http.MethodGet, settings.endpoint(), "")
	if err != nil {
		return err
	}
	if form.ResponderURI != settings.link || form.RevisionID == "" || len(form.Items) > len(backupTitles) {
		return fmt.Errorf("Google form identity or question structure did not match")
	}
	if settings.managed && (form.PublishSettings.State == nil || !form.PublishSettings.State.Published || !form.PublishSettings.State.Accepting) {
		if err := enableBackupResponses(ctx, client, settings); err != nil {
			return err
		}
		form, err = requestGoogleForm(ctx, client, http.MethodGet, settings.endpoint(), "")
		if err != nil {
			return err
		}
	}
	requests := make([]map[string]any, 0, len(backupTitles))
	seen := map[string]bool{}
	var removed []int
	for index, item := range form.Items {
		code := ""
		for key, title := range backupTitles {
			if title == item.Title {
				code = key
				break
			}
		}
		if code == "" || seen[code] {
			return fmt.Errorf("unexpected Google form title")
		}
		seen[code] = true
		candidates := roster[code]
		if len(candidates) == 0 {
			removed = append(removed, index)
			continue
		}
		current := item.QuestionItem.Question
		existing := map[string]bool{}
		for _, option := range current.ChoiceQuestion.Options {
			if option.Image != nil && option.Image.ContentURI != "" && !option.IsOther {
				existing[option.Value] = true
			}
		}
		same := synced == form.RevisionID && syncedRoster == digest && current.Required && current.ChoiceQuestion.Type == "RADIO" &&
			len(existing) == len(candidates) && len(current.ChoiceQuestion.Options) == len(candidates)
		for _, candidate := range candidates {
			same = same && existing[candidate.label]
		}
		if same {
			continue
		}
		requests = append(requests, map[string]any{"updateItem": map[string]any{
			"item":       map[string]any{"questionItem": question(candidates)},
			"location":   map[string]int{"index": index},
			"updateMask": "questionItem.question.required,questionItem.question.choiceQuestion.type,questionItem.question.choiceQuestion.options",
		}})
	}
	for i := len(removed) - 1; i >= 0; i-- {
		requests = append(requests, map[string]any{"deleteItem": map[string]any{"location": map[string]int{"index": removed[i]}}})
	}
	nextIndex := len(form.Items) - len(removed)
	for _, code := range []string{"dancer", "vocalist", "actor"} {
		if seen[code] || len(roster[code]) == 0 {
			continue
		}
		requests = append(requests, map[string]any{"createItem": map[string]any{
			"location": map[string]int{"index": nextIndex},
			"item":     map[string]any{"title": backupTitles[code], "questionItem": question(roster[code])},
		}})
		nextIndex++
	}
	if len(requests) == 0 {
		if synced == form.RevisionID && syncedRoster == digest {
			return nil
		}
		return saveBackupRevision(ctx, tx, id, form.RevisionID, digest)
	}
	payload, err := json.Marshal(map[string]any{"requests": requests, "writeControl": map[string]string{"requiredRevisionId": form.RevisionID}})
	if err != nil {
		return err
	}
	updated, err := requestGoogleForm(ctx, client, http.MethodPost, settings.endpoint()+":batchUpdate", string(payload))
	if err != nil {
		return fmt.Errorf("update %d nomination questions: %w", len(requests), err)
	}
	revision := updated.WriteControl.RequiredRevisionID
	if revision == "" {
		return fmt.Errorf("Google did not return a revision after synchronization")
	}
	return saveBackupRevision(ctx, tx, id, revision, digest)
}

func saveBackupRevision(ctx context.Context, tx *sql.Tx, id int64, revision, digest string) error {
	if _, err := tx.ExecContext(ctx, "UPDATE events SET backup_synced_revision=$1,backup_synced_roster=$2 WHERE id=$3", revision, digest, id); err != nil {
		return err
	}
	return tx.Commit()
}

func backupRosterDigest(roster map[string][]string, fingerprints map[string]string) string {
	var entries []string
	for code, labels := range roster {
		for _, label := range labels {
			entries = append(entries, code+"\x00"+fingerprints[label])
		}
	}
	sort.Strings(entries)
	data, _ := json.Marshal(entries)
	return photoFingerprint(data)
}

func portraitAlt(p backupPortrait) string { return p.label + " [" + p.fingerprint + "]" }

func backupQuestion(candidates []backupPortrait, event, secret, fixtureURL string) map[string]any {
	base := strings.TrimRight(os.Getenv("MEGALOVE_BACKUP_PORTRAIT_ORIGIN"), "/")
	if base == "" {
		base = "https://vote.megabattle.site"
	}
	options := make([]map[string]any, 0, len(candidates))
	until := time.Now().Add(9 * time.Minute).Unix()
	for _, candidate := range candidates {
		source := fmt.Sprintf("%s/api/backup/portraits/%d?until=%d&event=%s&signature=%s", base, candidate.id, until, url.QueryEscape(event), photoSignature(secret, event, candidate.id, until))
		if fixtureURL != "" {
			source = fixtureURL
		}
		options = append(options, map[string]any{"value": candidate.label, "image": map[string]any{"sourceUri": source, "altText": portraitAlt(candidate)}})
	}
	return map[string]any{"question": map[string]any{"required": true, "choiceQuestion": map[string]any{"type": "RADIO", "options": options}}}
}

func (v votingEvent) syncProvisionedBackups(base backupSettings, secret string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	rows, err := v.db.QueryContext(ctx, `SELECT slug,backup_form_id,backup_responder_url FROM events
 WHERE backup_responder_url IS NOT NULL AND backup_activated_at IS NULL ORDER BY id DESC`)
	if err != nil {
		cancel()
		log.Printf("backup roster list: %v", err)
		return
	}
	var forms []backupSettings
	for rows.Next() {
		settings := base
		settings.managed = true
		if err = rows.Scan(&settings.event, &settings.id, &settings.link); err != nil {
			break
		}
		forms = append(forms, settings)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	cancel()
	if err != nil {
		log.Printf("backup roster list: %v", err)
		return
	}
	for _, settings := range forms {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		err := v.syncBackup(ctx, settings, secret)
		cancel()
		if err != nil {
			log.Printf("backup roster %s: %v", settings.event, err)
		}
	}
}
