ALTER TABLE events ADD COLUMN backup_synced_revision text;

CREATE OR REPLACE FUNCTION reset_backup_review() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE events SET backup_reviewed_count = NULL, backup_synced_revision = NULL
    WHERE id IN (OLD.event_id, NEW.event_id)
      AND (backup_reviewed_count IS NOT NULL OR backup_synced_revision IS NOT NULL);
    RETURN NULL;
END;
$$;
