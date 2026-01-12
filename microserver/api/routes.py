from flask import render_template
from . import api_bp


@api_bp.route("/")
def home():
    return render_template("index.html")
