package main

import (
	"context"
	"database/sql"
	"errors"
)

func (v votingEvent) kind() string {
	if v.testRun {
		return "test"
	}
	return "main"
}

// $2 selects a public channel; the event row and run row must agree on availability.
const latestRunJoin = `LEFT JOIN LATERAL (SELECT * FROM voting_runs WHERE event_id=e.id AND kind=$2 ORDER BY id DESC LIMIT 1) vr ON true`
const publicRunState = `CASE WHEN vr.id IS NULL THEN 'preparing'
 WHEN vr.closed_at IS NOT NULL OR vr.deadline<=clock_timestamp() OR e.voting_ends_at<=clock_timestamp() OR e.state<>'open' THEN 'closed' ELSE 'open' END`

// Called with the event row locked, in the same transaction as its state transition.
func changeRun(ctx context.Context, tx *sql.Tx, eventID int64, mode, target string, minutes int) error {
	if target == "closed" {
		result, err := tx.ExecContext(ctx, `UPDATE voting_runs SET closed_at=clock_timestamp() WHERE event_id=$1 AND closed_at IS NULL AND kind=$2`, eventID, mode)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if n != 1 && err == nil {
			return errVotingTransition
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE voting_runs SET closed_at=deadline WHERE event_id=$1 AND closed_at IS NULL AND (deadline<=clock_timestamp() OR EXISTS(SELECT 1 FROM events e WHERE e.id=$1 AND e.voting_ends_at<=clock_timestamp()))`, eventID); err != nil {
		return err
	}
	kind := mode
	if kind == "new-test" {
		kind = "test"
	}
	var runID int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM voting_runs WHERE event_id=$1 AND kind=$2 ORDER BY id DESC LIMIT 1`, eventID, kind).Scan(&runID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if runID != 0 && mode != "new-test" {
		_, err = tx.ExecContext(ctx, `UPDATE voting_runs SET closed_at=NULL, deadline=clock_timestamp()+make_interval(mins=>$2) WHERE id=$1`, runID, minutes)
		return err
	}
	{
		var complete bool
		if err := tx.QueryRowContext(ctx, "SELECT count(DISTINCT nomination) = 3 FROM candidacies WHERE event_id = $1", eventID).Scan(&complete); err != nil {
			return err
		}
		if !complete {
			return errVotingRoster
		}
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO voting_runs(event_id,kind,started_at,deadline) VALUES($1,$2,statement_timestamp(),statement_timestamp()+make_interval(mins=>$3)) RETURNING id`, eventID, kind, minutes).Scan(&runID)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO run_participants SELECT $1,id,display_name,faculty_code,photo_jpeg FROM participants WHERE event_id=$2`, runID, eventID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO run_candidacies SELECT $1,participant_id,nomination,character_name FROM candidacies WHERE event_id=$2`, runID, eventID)
	return err
}
