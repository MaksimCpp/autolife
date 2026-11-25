ALTER TABLE platform.outbox_events DROP COLUMN purpose;
UPDATE platform.schema_stamp SET version = 5 WHERE singleton;
