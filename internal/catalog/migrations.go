package catalog

const schemaSQL = `
CREATE TABLE IF NOT EXISTS files (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	folder_label TEXT NOT NULL,
	relative_path TEXT NOT NULL,
	filename TEXT NOT NULL,
	extension TEXT NOT NULL DEFAULT '',
	mime_type TEXT NOT NULL DEFAULT '',
	size_bytes INTEGER NOT NULL DEFAULT 0,
	file_mtime DATETIME NOT NULL,
	file_atime DATETIME NOT NULL DEFAULT '1970-01-01 00:00:00',
	first_seen DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	archived_at DATETIME,
	status TEXT NOT NULL DEFAULT 'synced',
	thumbnail_path TEXT DEFAULT '',
	content_hash TEXT DEFAULT '',
	UNIQUE(folder_label, relative_path)
);

CREATE INDEX IF NOT EXISTS idx_files_folder_status ON files(folder_label, status);
CREATE INDEX IF NOT EXISTS idx_files_status ON files(status);
CREATE INDEX IF NOT EXISTS idx_files_mtime ON files(file_mtime);

CREATE TABLE IF NOT EXISTS folder_configs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	folder_label TEXT NOT NULL UNIQUE,
	syncthing_id TEXT NOT NULL DEFAULT '',
	syncthing_path TEXT NOT NULL,
	budget_bytes INTEGER NOT NULL DEFAULT 0,
	grace_minutes INTEGER NOT NULL DEFAULT 30,
	enabled INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS persistent_paths (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	folder_label TEXT NOT NULL,
	pattern TEXT NOT NULL,
	UNIQUE(folder_label, pattern)
);

CREATE TABLE IF NOT EXISTS events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	timestamp DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	event_type TEXT NOT NULL,
	folder TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL DEFAULT '',
	details TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_events_timestamp ON events(timestamp);
CREATE INDEX IF NOT EXISTS idx_events_type ON events(event_type);
`
