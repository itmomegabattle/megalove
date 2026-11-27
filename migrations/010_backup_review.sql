ALTER TABLE events ADD COLUMN backup_reviewed_count integer CHECK (backup_reviewed_count > 0);

CREATE FUNCTION reset_backup_review() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE events SET backup_reviewed_count = NULL
    WHERE id IN (OLD.event_id, NEW.event_id) AND backup_reviewed_count IS NOT NULL;
    RETURN NULL;
END;
$$;
CREATE TRIGGER participants_reset_backup_review AFTER INSERT OR UPDATE OR DELETE ON participants
FOR EACH ROW EXECUTE FUNCTION reset_backup_review();
CREATE TRIGGER candidacies_reset_backup_review AFTER INSERT OR UPDATE OR DELETE ON candidacies
FOR EACH ROW EXECUTE FUNCTION reset_backup_review();
