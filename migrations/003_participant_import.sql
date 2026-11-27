ALTER TABLE participants ADD COLUMN import_key text;
ALTER TABLE participants ADD CONSTRAINT participants_event_import_key UNIQUE (event_id, import_key);
