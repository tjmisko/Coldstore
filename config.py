from os import environ, path
from dotenv import load_dotenv
from datetime import timedelta
import logging

basedir = path.abspath(path.dirname(__file__))
print("App Directory: " + basedir)
load_dotenv(path.join(basedir, '.env'))

class Config:
    FLASK_ENV = 'development'
    LOG_LEVEL = logging.DEBUG
    LOG_DIR = path.join(basedir, 'logs')
    TESTING = True
    SECRET_KEY = environ['SECRET_KEY']
    DATABASE_URI = environ['DATABASE_URI']
    RELOAD = False
    JSONIFY_PRETTYPRINT_REGULAR = True
