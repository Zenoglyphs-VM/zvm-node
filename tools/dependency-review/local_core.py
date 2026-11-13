"""Bind the single declared local replacement to the independently checked Core tree."""

import json

from bounded import ReviewError, unique_object
from source_closure import CORE_COMMIT, CORE_URL

CORE_MODULE = "github.com/Zenoglyphs-VM/zvm-core"
CORE_VERSION = "v0.0.0"
CORE_REPLACEMENT = "./zvm-core"
CORE_TREE = "86f8c8cb55fb298674a73242a928141eebc1dee6"
CORE_MOD_SHA256 = "ee73727a250f768a34cebce98a678f393906a9a1e98e83b54519cf94d30ca91a"
CORE_SUM_SHA256 = "7e3f039005118f12158d9cb7714f9c8ffb4cf365c393cf4e2ee030d5ba316d49"

# Select public module identities only; Go's JSON field selector does not exclude
# other fields such as private cache or checkout paths.
MODULE_TEMPLATE = (r'{"Path":{{printf "%q" .Path}},"Version":{{printf "%q" .Version}},'
                   r'"Main":{{.Main}},"Replace":{{with .Replace}}'
                   r'{"Path":{{printf "%q" .Path}},"Version":{{printf "%q" .Version}}}'
                   r'{{else}}null{{end}}}')


def expected_profile():
    return {"schema": 1, "nominalModule": CORE_MODULE, "nominalVersion": CORE_VERSION,
            "effectiveModule": CORE_REPLACEMENT, "effectiveVersion": "",
            "sourceCommit": CORE_COMMIT, "sourceTree": CORE_TREE, "configuredURL": CORE_URL,
            "goModSha256": CORE_MOD_SHA256, "goSumSha256": CORE_SUM_SHA256,
            "independentSourceEvidenceRequired": True}


def validate_profile(value):
    expected = expected_profile()
    if (not isinstance(value, dict) or set(value) != set(expected) or
            type(value.get("schema")) is not int or value.get("independentSourceEvidenceRequired") is not True or
            value != expected):
        raise ReviewError("local Core replacement needs the exact reviewed source profile")
    return expected


def bind_core(source, raw_selection):
    """Check metadata; consumers must independently match the source observations."""
    selection = json.loads(raw_selection, object_pairs_hook=unique_object)
    wanted = {"Path": CORE_MODULE, "Version": CORE_VERSION, "Main": False,
              "Replace": {"Path": CORE_REPLACEMENT, "Version": ""}}
    if selection != wanted or selection.get("Main") is not False:
        raise ReviewError("actual Core module selection differs from the reviewed replacement")
    required = {"commit": CORE_COMMIT, "tree": CORE_TREE, "path": "zvm-core", "url": CORE_URL,
                "goModSha256": CORE_MOD_SHA256, "goSumSha256": CORE_SUM_SHA256, "rootLockOverrides": {}}
    if (not isinstance(source, dict) or source.get("sourceProfile") != "NODE_AND_EXPLICIT_CORE_GITLINK" or
            source.get("authenticationConfigValuesPrinted") is not False or
            not isinstance(source.get("core"), dict) or not isinstance(source.get("root"), dict) or
            any(source["core"].get(key) != value for key, value in required.items()) or
            source.get("root", {}).get("rootLockOverrides") != {}):
        raise ReviewError("source observations do not bind the selected local Core input")
    return expected_profile()
