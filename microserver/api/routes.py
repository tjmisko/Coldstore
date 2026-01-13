from flask import render_template, jsonify
from . import api_bp
from sqlite3 import Row
from ..models.db import get_db, close_db


@api_bp.route("/")
def home():
    return render_template("index.html")

@api_bp.route("/load")
def load():
    try:
        connection = get_db()
        cursor = connection.cursor()
        table_exists = cursor.execute("SELECT name FROM sqlite_master WHERE type='table' AND name='files';").fetchone()
        connection.commit()
        if not table_exists:
            cursor.execute("CREATE TABLE files(name, date, time, owner, description, permissions);")
            connection.commit()
            test_files = [
                ('testfile1.md', '2026-01-12', '16:31', 'tjmisko', 'Placeholder description #1', 'r'),
                ('testfile2.md', '2026-01-12', '16:32', 'tjmisko', 'Placeholder description #1', 'r'),
                ('testfile3.md', '2026-01-12', '16:33', 'tjmisko', 'Placeholder description #1', 'rw'),
                ('testfile4.md', '2026-01-12', '16:34', 'tjmisko', 'Placeholder description #1', 'x')
            ]
            cursor.executemany("""
                INSERT INTO files VALUES (?, ?, ?, ?, ?, ?);
            """, test_files)
            connection.commit()
            return "Contents inserted"
    except Exception as e:
        return str(e)
    return "Records already up to date!"

@api_bp.route("/query")
def query():
    connection = get_db()
    cursor = connection.cursor()
    contents = cursor.execute("SELECT * FROM files;").fetchall()
    connection.commit()
    close_db()
    return jsonify([dict(c) for c in contents])


@api_bp.route("/clear")
def clear():
    connection = get_db()
    cursor = connection.cursor()
    cursor.execute("DELETE FROM files WHERE true;")
    connection.commit()
    return "Cleared Table files"
