import sqlite3
from flask import current_app, g

def init_db():
    with current_app.app_context():
        db = get_db()
        with current_app.open_resource('instance/schema.sql', mode='r') as f:
            db.cursor().executescript(f.read())
        db.commit()

def get_db() -> sqlite3.Connection:
    if "db" not in g:
        conn = sqlite3.connect(
            current_app.config["DATABASE_URI"],
            timeout=30,
        )
        conn.row_factory = sqlite3.Row
        # Minimum viable SQLite config for web apps
        conn.execute("PRAGMA foreign_keys=ON;")
        conn.execute("PRAGMA journal_mode=WAL;")
        conn.execute("PRAGMA synchronous=NORMAL;")
        g.db = conn
    return g.db


def close_db(e=None) -> None:
    db = g.pop("db", None)
    if db is not None:
        db.close()

