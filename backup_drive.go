package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
)

const backupDriveFiles = "https://www.googleapis.com/drive/v3/files/"

func trashBackupForm(ctx context.Context, client *http.Client, id string) error {
	if !driveFileID.MatchString(id) {
		return fmt.Errorf("invalid backup form ID")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, backupDriveFiles+id+"?fields=trashed", strings.NewReader(`{"trashed":true}`))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Google Drive trash HTTP %d", resp.StatusCode)
	}
	var file struct{ Trashed bool }
	if err := json.NewDecoder(resp.Body).Decode(&file); err != nil {
		return err
	}
	if !file.Trashed {
		return fmt.Errorf("Google Drive did not confirm trashing the form")
	}
	return nil
}

// moveBackupForm preserves the form ID and checks the resulting parent.
func moveBackupForm(ctx context.Context, client *http.Client, formID, folderID string) error {
	if !driveFileID.MatchString(formID) || !driveFileID.MatchString(folderID) {
		return fmt.Errorf("invalid Drive form or folder ID")
	}
	endpoint := backupDriveFiles + formID
	var file struct{ Parents []string }
	driveRequest := func(method string, query url.Values, body string) error {
		req, err := http.NewRequestWithContext(ctx, method, endpoint+"?"+query.Encode(), strings.NewReader(body))
		if err != nil {
			return err
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("Google Drive %s HTTP %d", method, resp.StatusCode)
		}
		file.Parents = nil
		return json.NewDecoder(resp.Body).Decode(&file)
	}
	query := url.Values{"fields": {"parents"}, "supportsAllDrives": {"true"}}
	if err := driveRequest(http.MethodGet, query, ""); err != nil {
		return err
	}
	if slices.Contains(file.Parents, folderID) {
		return nil
	}
	query.Set("addParents", folderID)
	if len(file.Parents) > 0 {
		query.Set("removeParents", strings.Join(file.Parents, ","))
	}
	if err := driveRequest(http.MethodPatch, query, "{}"); err != nil {
		return err
	}
	if !slices.Contains(file.Parents, folderID) {
		return fmt.Errorf("Google Drive did not confirm the destination folder")
	}
	return nil
}

// createBackupDraft creates an unpublished form. Persist its ID before moving
// or populating it so a retry can reuse the same form.
func createBackupDraft(ctx context.Context, client *http.Client, name string) (string, error) {
	body, err := json.Marshal(map[string]any{"info": map[string]string{"title": name + " - Plan B"}})
	if err != nil {
		return "", err
	}
	form, err := requestGoogleForm(ctx, client, http.MethodPost, "https://forms.googleapis.com/v1/forms?unpublished=true", string(body))
	if err != nil {
		return "", err
	}
	if !driveFileID.MatchString(form.FormID) {
		return "", fmt.Errorf("Google Forms did not return a valid form ID")
	}
	return form.FormID, nil
}

type backupEventRow interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func eventBackupSettings(ctx context.Context, db backupEventRow, id int64) (backupSettings, error) {
	s := currentBackupSettings()
	var slug, formID, link string
	var reviewed int
	err := db.QueryRowContext(ctx, "SELECT slug, coalesce(backup_form_id,''), coalesce(backup_responder_url,''),coalesce(backup_reviewed_count,0) FROM events WHERE id=$1", id).Scan(&slug, &formID, &link, &reviewed)
	if err == nil && formID == "" && slug != s.event {
		s.id, s.link = "", ""
	}
	if err == nil && formID != "" {
		s = managedBackupSettings(s)
		s.managed = true
		s.expected, s.rehearsed = reviewed, reviewed > 0
		s.event, s.id, s.link = slug, formID, link
	}
	return s, err
}

func managedBackupSettings(s backupSettings) backupSettings {
	if path := os.Getenv("MEGALOVE_BACKUP_MANAGED_CREDENTIALS"); path != "" {
		s.credentials = path
	}
	return s
}

func (v votingEvent) provisionBackup(ctx context.Context, eventID int64, folder string, settings backupSettings) error {
	tx, err := v.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var name, id string
	err = tx.QueryRowContext(ctx, "SELECT name,coalesce(backup_form_id,'') FROM events WHERE id=$1 FOR UPDATE", eventID).Scan(&name, &id)
	if err != nil {
		return err
	}
	client, err := settings.client(ctx)
	if err != nil {
		return err
	}
	if id == "" {
		id, err = createBackupDraft(ctx, client, name)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE events SET backup_form_id=$1 WHERE id=$2", id, eventID); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if err = moveBackupForm(ctx, client, id, folder); err != nil {
		return err
	}
	settings.id = id
	form, err := requestGoogleForm(ctx, client, http.MethodGet, settings.endpoint(), "")
	if err != nil {
		return err
	}
	if !responderURL(form.ResponderURI) {
		return fmt.Errorf("Google Forms did not return a responder URL")
	}
	if form.PublishSettings.State == nil || !form.PublishSettings.State.Published || !form.PublishSettings.State.Accepting {
		if err := enableBackupResponses(ctx, client, settings); err != nil {
			return err
		}
	}
	_, err = v.db.ExecContext(ctx, "UPDATE events SET backup_responder_url=$1 WHERE id=$2 AND backup_form_id=$3", form.ResponderURI, eventID, id)
	return err
}

func (v votingEvent) provisionNextBackup(ctx context.Context, folder string, settings backupSettings) error {
	var id int64
	err := v.db.QueryRowContext(ctx, `SELECT id FROM events
 WHERE backup_requested AND backup_responder_url IS NULL AND slug <> $1 ORDER BY id LIMIT 1`, settings.event).Scan(&id)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	return v.provisionBackup(ctx, id, folder, settings)
}
