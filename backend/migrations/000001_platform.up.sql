CREATE TABLE platform.schema_stamp (
    singleton boolean PRIMARY KEY CHECK (singleton),
    version integer NOT NULL CHECK (version > 0)
);
INSERT INTO platform.schema_stamp (singleton, version) VALUES (true, 1);
GRANT SELECT ON platform.schema_stamp TO autolife_api, autolife_worker;
