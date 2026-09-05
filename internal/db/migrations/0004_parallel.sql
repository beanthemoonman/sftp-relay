-- Mirror parallelism: how many files lftp's `mirror` transfers at once. Distinct
-- from `segments` (pget segments per file) and `concurrency` (concurrent jobs).
-- INSERT OR IGNORE so an existing database keeps whatever it already has.
INSERT OR IGNORE INTO settings (key, value) VALUES ('parallel', '2');
