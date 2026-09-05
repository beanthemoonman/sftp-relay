-- lftp drives sftp:// through ssh, which cannot take a password non-interactively.
-- Password-auth remote servers therefore need sshpass on the NAS; its absolute
-- path is probed at startup and cached here, exactly like lftp_path.
INSERT OR IGNORE INTO settings (key, value) VALUES ('sshpass_path', '');
