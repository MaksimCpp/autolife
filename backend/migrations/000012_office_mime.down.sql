ALTER TABLE platform.files DROP CONSTRAINT files_mime_check;
ALTER TABLE platform.files ADD CONSTRAINT files_mime_check CHECK (mime IN (
    'image/jpeg',
    'image/png',
    'image/webp',
    'application/pdf'
));
UPDATE platform.schema_stamp SET version=11 WHERE singleton;
