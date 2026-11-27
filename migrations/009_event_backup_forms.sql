ALTER TABLE events ADD COLUMN backup_form_id text UNIQUE;
ALTER TABLE events ADD COLUMN backup_responder_url text;
ALTER TABLE events ADD COLUMN backup_requested boolean NOT NULL DEFAULT false;
