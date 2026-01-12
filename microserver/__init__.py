from flask import Flask, g, current_app
from .api import api_bp
import sqlite3
from .models import db


def create_app(Config):
    app = Flask(__name__)
    app.config.from_object(Config)
    app.register_blueprint(api_bp)
    return app
