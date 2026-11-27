ALTER TABLE platform.execution_attempts DROP COLUMN file_usage;
ALTER TABLE platform.execution_event_totals DROP COLUMN input_bytes,DROP COLUMN output_bytes,DROP COLUMN file_count;
UPDATE platform.schema_stamp SET version=10 WHERE singleton;
