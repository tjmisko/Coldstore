from flask import Blueprint

api_bp = Blueprint(
    "api_bp", __name__,
    static_folder="static", 
    template_folder="templates"
)

from . import routes
