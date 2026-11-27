package main

import (
	"bytes"
	"database/sql"
	"embed"
	"errors"
	"log"
	"net/http"
)

//go:embed assets
var viewerAssets embed.FS

func (v votingEvent) catalog(w http.ResponseWriter, r *http.Request) {
	var payload string
	err := v.db.QueryRowContext(r.Context(), `
		SELECT CASE WHEN (`+publicRunState+`) <> 'open' THEN '' ELSE json_build_object(
			'faculties', (SELECT json_agg(json_build_object(
				'code', f.code, 'name', f.short_name, 'fullName', f.full_name
			) ORDER BY f.short_name) FROM faculties f),
			'candidates', (SELECT coalesce(json_agg(json_build_object(
				'id', p.participant_id, 'name', p.display_name, 'faculty', p.faculty_code, 'nomination', c.nomination,
				'character', c.character_name, 'hasPhoto', p.photo_jpeg IS NOT NULL
			) ORDER BY p.display_name, p.participant_id), '[]'::json)
			FROM run_candidacies c JOIN run_participants p USING(run_id, participant_id)
			WHERE c.run_id = vr.id)
		)::text END
		FROM events e `+latestRunJoin+` WHERE `+activeEventFilter, v.slug, v.kind()).Scan(&payload)
	if err != nil {
		log.Printf("voting catalog: %T", err)
		votingError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if payload == "" {
		votingError(w, http.StatusConflict, "voting_closed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(payload))
}

func (v votingEvent) publicPhoto(w http.ResponseWriter, r *http.Request) {
	var data []byte
	err := v.db.QueryRowContext(r.Context(), `
		SELECT p.photo_jpeg FROM events e `+latestRunJoin+` JOIN run_participants p ON p.run_id=vr.id
        WHERE `+activeEventFilter+` AND (`+publicRunState+`) = 'open' AND p.participant_id=$3 AND p.photo_jpeg IS NOT NULL
    `, v.slug, v.kind(), pathID(r, "person")).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		w.Header().Set("Cache-Control", "no-store")
		http.NotFound(w, r)
		return
	}
	if err != nil {
		votingError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if len(data) != ballotPhotoBytes {
		data, err = preparePhoto(bytes.NewReader(data))
		if err != nil {
			http.NotFound(w, r)
			return
		}
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Write(data)
}
