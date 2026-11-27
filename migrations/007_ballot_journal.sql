ALTER TABLE ballots ADD COLUMN journal_id bigint GENERATED ALWAYS AS IDENTITY UNIQUE;
CREATE INDEX ballots_event_accepted_idx ON ballots (event_id, accepted_at);
