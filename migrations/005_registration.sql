ALTER TABLE events ADD COLUMN registration_token text UNIQUE;
ALTER TABLE participants ADD COLUMN registration_submission text;
ALTER TABLE participants ADD UNIQUE (event_id, registration_submission);
