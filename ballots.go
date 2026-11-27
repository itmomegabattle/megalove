package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const voterCookie = "megalove_voter"
const voterCookieLifetime = 30 * 24 * time.Hour

var browserIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var errVotingClosed = errors.New("voting is not open")
var errBallotEventChanged = errors.New("selected event changed")

type ballotChoices struct {
	DancerID   int64 `json:"dancer_id"`
	VocalistID int64 `json:"vocalist_id"`
	ActorID    int64 `json:"actor_id"`
}

func browserID(r *http.Request) string {
	cookie, err := r.Cookie(voterCookie)
	if err != nil || !browserIDPattern.MatchString(cookie.Value) {
		return ""
	}
	return cookie.Value
}

func (v votingEvent) issueVoterCookie(w http.ResponseWriter) {
	var raw [16]byte
	rand.Read(raw[:])
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	id := fmt.Sprintf("%x-%x-%x-%x-%x", raw[:4], raw[4:6], raw[6:8], raw[8:10], raw[10:])
	http.SetCookie(w, &http.Cookie{
		Name: voterCookie, Value: id, Path: "/api/voting",
		HttpOnly: true, Secure: !v.localHTTP, SameSite: http.SameSiteStrictMode,
		MaxAge: int(voterCookieLifetime.Seconds()),
	})
}

func votingJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func votingError(w http.ResponseWriter, status int, code string) {
	votingJSON(w, status, map[string]string{"error": code})
}

type ballotAuditWriter struct {
	http.ResponseWriter
	status int
}

func (w *ballotAuditWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (v votingEvent) status(w http.ResponseWriter, r *http.Request) {
	var response struct {
		Event string `json:"event"`
		State string `json:"state"`
		Voted bool   `json:"voted"`
	}
	id := browserID(r)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	err := v.db.QueryRowContext(ctx, `
		SELECT concat(e.slug, '/', coalesce(vr.id,0)), `+publicRunState+`, EXISTS (
			SELECT 1 FROM ballots b WHERE b.run_id = vr.id AND b.browser_id = nullif($3, '')::uuid
		)
		FROM events e `+latestRunJoin+` WHERE `+activeEventFilter, v.slug, v.kind(), id).Scan(&response.Event, &response.State, &response.Voted)
	if err != nil {
		log.Printf("voting status: %T", err)
		votingError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if id == "" {
		v.issueVoterCookie(w)
	}
	votingJSON(w, http.StatusOK, response)
}

func (v votingEvent) submitBallot(w http.ResponseWriter, r *http.Request) {
	audit := &ballotAuditWriter{ResponseWriter: w}
	w = audit
	defer func() {
		if audit.status >= 400 {
			log.Printf("ballot rejected status=%d", audit.status)
		}
	}()
	id := browserID(r)
	if id == "" {
		votingError(w, http.StatusConflict, "browser_required")
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		votingError(w, http.StatusUnsupportedMediaType, "json_required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2048)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var choices ballotChoices
	if err := decoder.Decode(&choices); err != nil || decoder.Decode(new(any)) != io.EOF ||
		choices.DancerID < 1 || choices.VocalistID < 1 || choices.ActorID < 1 {
		votingError(w, http.StatusBadRequest, "three_choices_required")
		return
	}

	// The ballot has been read and validated. A lost connection can only
	// lose the acknowledgement, not cancel the database transaction.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 7*time.Second)
	defer cancel()
	token := r.Header.Get("X-MegaLove-Event")
	if v.testRun && token == "" {
		votingError(w, http.StatusConflict, "event_changed")
		return
	}
	err = v.acceptBallotForEvent(ctx, token, id, choices)
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		votingJSON(w, http.StatusOK, map[string]bool{"accepted": true})
	case errors.Is(err, errVotingClosed):
		votingError(w, http.StatusConflict, "voting_closed")
	case errors.Is(err, errBallotEventChanged):
		votingError(w, http.StatusConflict, "event_changed")
	case errors.As(err, &pgErr) && pgErr.Code == "23503":
		votingError(w, http.StatusUnprocessableEntity, "invalid_candidate")
	default:
		log.Printf("voting ballot: %T", err)
		votingError(w, http.StatusServiceUnavailable, "unavailable")
	}
}

func (v votingEvent) acceptBallot(ctx context.Context, id string, choices ballotChoices) error {
	return v.acceptBallotForEvent(ctx, "", id, choices)
}

func (v votingEvent) acceptBallotForEvent(ctx context.Context, expectedEvent, id string, choices ballotChoices) error {
	tx, err := v.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var eventID, runID int64
	var state, eventSlug string
	// Read the run after acquiring the event lock so a waiting submission
	// sees the run committed by the administrator before releasing that lock.
	if _, err := tx.ExecContext(ctx, "SELECT 1 FROM active_voting_event WHERE singleton FOR SHARE"); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, "SELECT e.id FROM events e WHERE "+activeEventFilter+" FOR SHARE OF e", v.slug).Scan(&eventID); err != nil {
		return err
	}
	err = tx.QueryRowContext(ctx, "SELECT "+publicRunState+", concat(e.slug,'/',coalesce(vr.id,0)),coalesce(vr.id,0) FROM events e "+latestRunJoin+" WHERE e.id=$1", eventID, v.kind()).
		Scan(&state, &eventSlug, &runID)
	if err != nil {
		return err
	}
	if expectedEvent != "" && expectedEvent != eventSlug {
		return errBallotEventChanged
	}
	var accepted bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM ballots WHERE run_id = $1 AND browser_id = $2::uuid)
	`, runID, id).Scan(&accepted); err != nil {
		return err
	}
	if accepted {
		log.Printf("ballot repeat event_id=%d", eventID)
		return nil
	}
	if state != "open" {
		return errVotingClosed
	}
	result, err := tx.ExecContext(ctx, `
		WITH arrival AS MATERIALIZED (SELECT clock_timestamp() AS at)
		INSERT INTO ballots (event_id,run_id, browser_id, dancer_id, vocalist_id, actor_id, accepted_at)
		SELECT $1,$6, $2::uuid, $3, $4, $5, arrival.at FROM voting_runs vr CROSS JOIN arrival
		WHERE vr.id=$6 AND vr.closed_at IS NULL AND (vr.deadline IS NULL OR arrival.at < vr.deadline)
		ON CONFLICT (run_id, browser_id) DO NOTHING
	`, eventID, id, choices.DancerID, choices.VocalistID, choices.ActorID, runID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM ballots WHERE run_id=$1 AND browser_id=$2::uuid)", runID, id).Scan(&accepted); err != nil {
			return err
		}
		if !accepted {
			return errVotingClosed
		}
		log.Printf("ballot repeat event_id=%d", eventID)
	}
	return tx.Commit()
}
