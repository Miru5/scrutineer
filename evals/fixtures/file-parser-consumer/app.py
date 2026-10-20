import json
import zipfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
RESOURCES = HERE / "resources.zip"


def load_config():
    with open(HERE / "config.json", encoding="utf-8") as handle:
        return json.load(handle)


def help_text(archive=RESOURCES):
    if not archive.exists():
        # The release build packs help.txt; create it here for a source checkout.
        with zipfile.ZipFile(archive, "w") as bundle:
            bundle.writestr("help.txt", "Press n for a new note.")
    with zipfile.ZipFile(archive) as bundle:
        return bundle.read("help.txt").decode("utf-8")
