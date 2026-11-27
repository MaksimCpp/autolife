ALTER TABLE platform.execution_attempts
 ADD COLUMN file_usage jsonb CHECK(file_usage IS NULL OR jsonb_typeof(file_usage)='object');
ALTER TABLE platform.execution_event_totals
 ADD COLUMN input_bytes bigint NOT NULL DEFAULT 0,
 ADD COLUMN output_bytes bigint NOT NULL DEFAULT 0,
 ADD COLUMN file_count bigint NOT NULL DEFAULT 0;
UPDATE platform.schema_stamp SET version=11 WHERE singleton;
