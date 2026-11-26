DROP TABLE platform.file_pins, platform.quota_reservations, platform.quota_limits, platform.files;
UPDATE platform.schema_stamp SET version = 6 WHERE singleton;
