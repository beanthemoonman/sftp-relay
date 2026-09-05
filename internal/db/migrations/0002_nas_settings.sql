-- Settings added with the NAS client: the pinned NAS host key (captured on
-- first connect) and a non-default SSH port. INSERT OR IGNORE so an existing
-- database keeps whatever it already has.
INSERT OR IGNORE INTO settings (key, value) VALUES
    ('nas_host_key', ''),
    ('nas_port', '22');
